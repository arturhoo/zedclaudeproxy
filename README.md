# Claude 3.7 Sonnet Extended Thinking Proxy for Zed

A simple HTTP proxy that enables access to Claude 3.7 Sonnet Extended Thinking capabilities. This proxy intercepts API requests to Claude models with a "-thinking" suffix, adds the thinking capability, and filters the thinking content from responses while logging it to the console.

Filtering the thinking content from responses is needed as Zed does not expect the thinking content to be returned in the response.

## Features

- Intercepts requests to Claude models with the "-thinking" suffix
- Adds thinking capability to these requests
- Forwards requests to regular Claude models without modification
- Filters thinking content from responses
- Logs thinking content to the console
- Streams responses in real-time

## Usage

```bash
# Run with default settings
go run .

# Custom configuration
go run . --listen=0.0.0.0:8080 --target=https://api.anthropic.com --budget=2048
```

## Zed Configuration

Add the following configuration to your Zed settings:

```json
"language_models": {
  "anthropic": {
    "version": "1",
    "api_url": "http://localhost:8080",
    "available_models": [
      {
        "name": "claude-3-7-sonnet-latest-thinking",
        "display_name": "Claude 3.7 Sonnet Thinking",
      }
    ]
  }
},
```
