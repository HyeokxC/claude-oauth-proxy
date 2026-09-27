package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
)

// StreamTransformResponse reads SSE events from upstream and writes them
// to the client with tool names reversed. Flushes after each event.
func StreamTransformResponse(w http.ResponseWriter, body io.ReadCloser) {
	defer body.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("[response] ResponseWriter does not support Flusher, copying raw")
		io.Copy(w, body)
		return
	}

	scanner := bufio.NewScanner(body)
	// Increase buffer for large SSE events
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var summary struct {
		messageStart, contentStart, contentDelta, messageDelta, messageStop, errors int
		inputTokens, outputTokens, malformed                                        int
		stopReason, errorType                                                       string
	}

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "data: ") {
			data := line[6:]
			if data != "[DONE]" {
				var event struct {
					Type  string `json:"type"`
					Delta struct {
						StopReason string `json:"stop_reason"`
					} `json:"delta"`
					Error struct {
						Type string `json:"type"`
					} `json:"error"`
					Usage struct {
						OutputTokens int `json:"output_tokens"`
					} `json:"usage"`
					Message struct {
						Usage struct {
							InputTokens int `json:"input_tokens"`
						} `json:"usage"`
					} `json:"message"`
				}
				if json.Unmarshal([]byte(data), &event) == nil {
					switch event.Type {
					case "message_start":
						summary.messageStart++
						summary.inputTokens = event.Message.Usage.InputTokens
					case "content_block_start":
						summary.contentStart++
					case "content_block_delta":
						summary.contentDelta++
					case "message_delta":
						summary.messageDelta++
						summary.outputTokens = event.Usage.OutputTokens
						switch event.Delta.StopReason {
						case "end_turn", "max_tokens", "stop_sequence", "tool_use", "refusal", "content_filter":
							summary.stopReason = event.Delta.StopReason
						case "":
						default:
							summary.stopReason = "other"
						}
					case "message_stop":
						summary.messageStop++
					case "error":
						summary.errors++
						switch event.Error.Type {
						case "rate_limit_error", "authentication_error", "permission_error", "invalid_request_error", "overloaded_error", "api_error", "content_filter":
							summary.errorType = event.Error.Type
						default:
							summary.errorType = "other"
						}
					}
				} else {
					summary.malformed++
				}
				transformed := reverseToolNamesInSSEData([]byte(data))
				w.Write([]byte("data: "))
				w.Write(transformed)
				w.Write([]byte("\n"))
				flusher.Flush()
				continue
			}
		}

		// Pass through non-data lines (event:, id:, retry:, empty lines)
		w.Write([]byte(line))
		w.Write([]byte("\n"))
		flusher.Flush()
	}

	if err := scanner.Err(); err != nil {
		log.Printf("[response] SSE scan error: %v", err)
	}
	log.Printf("[response] SSE summary: message_start=%d content_block_start=%d content_block_delta=%d message_delta=%d message_stop=%d error=%d malformed=%d stop_reason=%s error_type=%s input_tokens=%d output_tokens=%d",
		summary.messageStart, summary.contentStart, summary.contentDelta, summary.messageDelta, summary.messageStop, summary.errors, summary.malformed, summary.stopReason, summary.errorType, summary.inputTokens, summary.outputTokens)
}

// TransformResponseBody reverses tool names in a non-streaming JSON response.
func TransformResponseBody(body []byte) []byte {
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		return body
	}

	reverseToolNamesInContent(msg)

	result, err := json.Marshal(msg)
	if err != nil {
		return body
	}
	return result
}

func reverseToolNamesInSSEData(data []byte) []byte {
	var event map[string]any
	if err := json.Unmarshal(data, &event); err != nil {
		return data
	}

	eventType, _ := event["type"].(string)

	switch eventType {
	case "content_block_start":
		if cb, ok := event["content_block"].(map[string]any); ok {
			if cb["type"] == "tool_use" {
				if name, ok := cb["name"].(string); ok {
					if original, exists := ReverseToolNameMap[name]; exists {
						cb["name"] = original
					}
				}
			}
		}
	case "message_start":
		if msg, ok := event["message"].(map[string]any); ok {
			reverseToolNamesInContent(msg)
		}
	case "message_delta":
		if delta, ok := event["delta"].(map[string]any); ok {
			reverseToolNamesInContent(delta)
		}
	}

	result, err := json.Marshal(event)
	if err != nil {
		return data
	}
	return result
}

func reverseToolNamesInContent(msg map[string]any) {
	content, ok := msg["content"].([]any)
	if !ok {
		return
	}

	for _, block := range content {
		if b, ok := block.(map[string]any); ok {
			if b["type"] == "tool_use" {
				if name, ok := b["name"].(string); ok {
					if original, exists := ReverseToolNameMap[name]; exists {
						b["name"] = original
					}
				}
			}
		}
	}
}

// IsSSE checks if the response is a server-sent event stream.
func IsSSE(resp *http.Response) bool {
	ct := resp.Header.Get("Content-Type")
	return strings.Contains(ct, "text/event-stream")
}

// CopyResponseHeaders copies response headers from upstream to client.
// If stripContentLength is true, Content-Length is removed (for transformed bodies).
func CopyResponseHeaders(w http.ResponseWriter, resp *http.Response, stripContentLength ...bool) {
	strip := len(stripContentLength) > 0 && stripContentLength[0]
	for k, vv := range resp.Header {
		if strip && strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
}

// ReadAndTransformBody reads a non-streaming response body and transforms it.
func ReadAndTransformBody(w http.ResponseWriter, body io.ReadCloser) {
	defer body.Close()

	var buf bytes.Buffer
	io.Copy(&buf, body)

	transformed := TransformResponseBody(buf.Bytes())
	w.Write(transformed)
}
