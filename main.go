// Package main provides an HTTP proxy for Anthropic's Claude API that enables
// access to Claude's thinking process by intercepting requests with "-thinking"
// model suffix, adding the thinking capability, and filtering the thinking
// content from responses while logging it to the console.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// Configuration variables
var (
	proxyListenAddress = flag.String("listen", "localhost:8080", "Address to listen on")
	targetURL          = flag.String("target", "https://api.anthropic.com", "Target API URL")
	thinkingBudget     = flag.Int("budget", 1024, "Token budget for thinking")
	logThinking        = flag.Bool("log", true, "Whether to log thinking content")
	messagesEndpoint   = "/v1/messages"
)

// ThinkingConfig represents the thinking field to add
type ThinkingConfig struct {
	BudgetTokens int    `json:"budget_tokens"`
	Type         string `json:"type"`
}

// SSEEvent represents a server-sent event
type SSEEvent struct {
	Event string
	Data  string
}

// parseSSE parses a server-sent event string into an SSEEvent
func parseSSE(eventStr string) (*SSEEvent, error) {
	eventStr = strings.TrimSpace(eventStr)
	if eventStr == "" {
		return nil, nil // Empty event
	}

	var event, data string
	scanner := bufio.NewScanner(strings.NewReader(eventStr))

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}

	if event == "" && data == "" {
		return nil, fmt.Errorf("invalid SSE format: %s", eventStr)
	}

	return &SSEEvent{
		Event: event,
		Data:  data,
	}, nil
}

// isThinkingBlock checks if an event represents a thinking content block
func isThinkingBlock(event *SSEEvent) bool {
	if event.Event != "content_block_start" {
		return false
	}

	var contentBlockStart struct {
		Type         string `json:"type"`
		Index        int    `json:"index"`
		ContentBlock struct {
			Type string `json:"type"`
		} `json:"content_block"`
	}

	if err := json.Unmarshal([]byte(event.Data), &contentBlockStart); err != nil {
		return false
	}

	return contentBlockStart.ContentBlock.Type == "thinking"
}

// isContentBlockDelta checks if an event is a content_block_delta
func isContentBlockDelta(event *SSEEvent) bool {
	return event.Event == "content_block_delta"
}

// isContentBlockStop checks if an event is a content_block_stop
func isContentBlockStop(event *SSEEvent) bool {
	return event.Event == "content_block_stop"
}

// getContentBlockIndex extracts the index from content block events
func getContentBlockIndex(event *SSEEvent) (int, error) {
	var blockEvent struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
	}

	if err := json.Unmarshal([]byte(event.Data), &blockEvent); err != nil {
		return -1, err
	}

	return blockEvent.Index, nil
}

// extractThinkingDelta extracts thinking content from a thinking_delta event
func extractThinkingDelta(event *SSEEvent) (string, error) {
	var deltaEvent struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta struct {
			Type     string `json:"type"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
	}

	if err := json.Unmarshal([]byte(event.Data), &deltaEvent); err != nil {
		return "", err
	}

	if deltaEvent.Delta.Type != "thinking_delta" {
		return "", nil
	}

	return deltaEvent.Delta.Thinking, nil
}

// modifyModelName changes the model name by removing "-thinking" suffix
func modifyModelName(modelName string) string {
	return strings.Replace(modelName, "-thinking", "", 1)
}

// hasThinkingSuffix checks if a model name has the "-thinking" suffix
func hasThinkingSuffix(modelName string) bool {
	return strings.Contains(modelName, "-thinking")
}

// forwardRequestWithModifications forwards request with added thinking capability
func forwardRequestWithModifications(w http.ResponseWriter, r *http.Request, bodyBytes []byte, originalModelName string) {
	// Parse the JSON body
	var bodyJSON map[string]any
	if err := json.Unmarshal(bodyBytes, &bodyJSON); err != nil {
		http.Error(w, "Error parsing JSON request body", http.StatusBadRequest)
		return
	}

	// Modify model name
	modifiedModelName := modifyModelName(originalModelName)
	bodyJSON["model"] = modifiedModelName
	log.Printf("Modified model name from '%s' to '%s'", originalModelName, modifiedModelName)

	// Add the "thinking" field
	bodyJSON["thinking"] = ThinkingConfig{
		BudgetTokens: *thinkingBudget,
		Type:         "enabled",
	}

	// Ensure streaming is enabled
	bodyJSON["stream"] = true

	// Convert the modified body back to JSON
	modifiedBody, err := json.Marshal(bodyJSON)
	if err != nil {
		http.Error(w, "Error re-encoding JSON", http.StatusInternalServerError)
		return
	}

	// Forward request with modifications and filter response
	forwardRequestAndHandleResponse(w, r, modifiedBody, true)
}

// forwardRequestAsIs forwards request exactly as received
func forwardRequestAsIs(w http.ResponseWriter, r *http.Request, bodyBytes []byte) {
	// Forward request without modifications and stream response as-is
	forwardRequestAndHandleResponse(w, r, bodyBytes, false)
}

// forwardRequestAndHandleResponse handles the actual forwarding and response processing
func forwardRequestAndHandleResponse(w http.ResponseWriter, r *http.Request, bodyBytes []byte, filterThinking bool) {
	// Create a new request to forward to the target
	forwardReq, err := http.NewRequest(r.Method, *targetURL+r.URL.Path, bytes.NewReader(bodyBytes))
	if err != nil {
		http.Error(w, "Error creating forward request", http.StatusInternalServerError)
		return
	}

	// Copy headers
	for name, values := range r.Header {
		for _, value := range values {
			forwardReq.Header.Add(name, value)
		}
	}

	// Set content length for the modified body
	forwardReq.ContentLength = int64(len(bodyBytes))
	forwardReq.Header.Set("Content-Length", fmt.Sprintf("%d", len(bodyBytes)))

	// Set host header
	forwardReq.Host = strings.TrimPrefix(*targetURL, "https://")

	// Make the request to the target
	client := &http.Client{
		Timeout: 300 * time.Second, // 5 minute timeout
	}
	resp, err := client.Do(forwardReq)
	if err != nil {
		http.Error(w, "Error forwarding request: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy headers from the target response
	for name, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}

	// Set SSE specific headers
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Content-Type", "text/event-stream")

	// Set status code
	w.WriteHeader(resp.StatusCode)

	// If response is an error (non-2xx), just copy the body directly
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if _, err := io.Copy(w, resp.Body); err != nil {
			log.Printf("Error copying error response: %v", err)
		}
		return
	}

	// Flush headers to client
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	// If we're not filtering thinking content, just stream the response directly
	if !filterThinking {
		// Simple streaming copy for non-thinking models
		buffer := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buffer)
			if err != nil && err != io.EOF {
				log.Printf("Error reading response: %v", err)
				break
			}
			if n > 0 {
				if _, err := w.Write(buffer[:n]); err != nil {
					log.Printf("Error writing response: %v", err)
					break
				}
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
			if err == io.EOF {
				break
			}
		}
		return
	}

	// For thinking models, process the SSE stream to filter out thinking blocks
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB buffer
	var buffer strings.Builder

	currentThinkingIndex := -1
	inThinkingBlock := false
	var thinkingContent strings.Builder

	for scanner.Scan() {
		line := scanner.Text()

		// Empty line marks the end of an event
		if line == "" {
			eventStr := buffer.String()
			buffer.Reset()

			// Skip empty events
			if eventStr == "" {
				continue
			}

			// Parse the event
			event, err := parseSSE(eventStr)
			if err != nil {
				log.Printf("Error parsing SSE: %v", err)
				continue
			}

			if event == nil {
				continue
			}

			// Handle different types of events
			if isThinkingBlock(event) {
				// Found a thinking block, mark it
				index, _ := getContentBlockIndex(event)
				currentThinkingIndex = index
				inThinkingBlock = true
				thinkingContent.Reset() // Reset accumulated thinking content
				log.Printf("Found thinking block at index %d", index)
				continue // Skip sending this event
			}

			if inThinkingBlock {
				// Check if this is a delta for the current thinking block
				if isContentBlockDelta(event) {
					index, err := getContentBlockIndex(event)
					if err == nil && index == currentThinkingIndex {
						// Extract thinking content from the delta
						thinkingDelta, err := extractThinkingDelta(event)
						if err == nil && thinkingDelta != "" {
							thinkingContent.WriteString(thinkingDelta)
						}
						continue // Skip sending this event
					}
				}

				// If we get here with a content_block_stop for the thinking block,
				// log the thinking content and mark that we're no longer in a thinking block
				if isContentBlockStop(event) {
					index, err := getContentBlockIndex(event)
					if err == nil && index == currentThinkingIndex {
						if *logThinking {
							log.Printf("\n===== THINKING CONTENT =====\n%s\n==========================\n",
								thinkingContent.String())
						}
						inThinkingBlock = false
						continue // Skip sending this event
					}
				}
			}

			// Forward all other events
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Event, event.Data)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		} else {
			buffer.WriteString(line)
			buffer.WriteString("\n")
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Error reading SSE stream: %v", err)
	}
}

func main() {
	// Parse command line flags
	flag.Parse()

	// Handler for requests
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Received request: %s %s", r.Method, r.URL.Path)

		// Only process POST requests to messages endpoint
		if r.Method == "POST" && r.URL.Path == messagesEndpoint {
			// Read the request body
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "Error reading request body", http.StatusBadRequest)
				return
			}
			r.Body.Close()

			// Try to parse the request body
			var bodyJSON map[string]any
			if err := json.Unmarshal(bodyBytes, &bodyJSON); err != nil {
				log.Printf("Error parsing request body: %v", err)
				// If we can't parse the body, just forward it as-is
				forwardRequestAsIs(w, r, bodyBytes)
				return
			}

			// Check if the model name has the "-thinking" suffix
			modelName, ok := bodyJSON["model"].(string)
			if ok && hasThinkingSuffix(modelName) {
				log.Printf("Detected model with thinking suffix: %s", modelName)
				// Forward with thinking modifications
				forwardRequestWithModifications(w, r, bodyBytes, modelName)
			} else {
				log.Printf("Forwarding request for regular model without modifications")
				// Forward as-is for regular models
				forwardRequestAsIs(w, r, bodyBytes)
			}
		} else {
			// For non-messages endpoints or non-POST methods, forward directly
			body, _ := io.ReadAll(r.Body)
			r.Body.Close()
			forwardRequestAsIs(w, r, body)
		}
	})

	// Create a server with proper configuration
	server := &http.Server{
		Addr:    *proxyListenAddress,
		Handler: handler,
	}

	// Set up signal handling for graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Start the server in a goroutine
	go func() {
		log.Printf("Starting proxy server on %s", *proxyListenAddress)
		log.Printf("Forwarding to %s", *targetURL)
		log.Printf("Thinking budget: %d tokens", *thinkingBudget)
		log.Printf("Log thinking: %v", *logThinking)

		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Error starting server: %v", err)
		}
	}()

	// Wait for interrupt signal
	<-stop
	log.Println("Shutting down server...")

	// Create a deadline for server shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Attempt graceful shutdown
	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Server gracefully stopped")
}
