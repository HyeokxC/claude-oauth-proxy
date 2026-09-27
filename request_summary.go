package main

import (
	"encoding/json"
	"log"
	"strings"
)

// LogSentBodySummary emits only counts and sizes, never user-supplied text.
func LogSentBodySummary(body []byte) {
	if len(body) == 0 {
		return
	}
	var msg struct {
		Messages []json.RawMessage `json:"messages"`
		Tools    []json.RawMessage `json:"tools"`
		System   json.RawMessage   `json:"system"`
		Stream   bool              `json:"stream"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		log.Printf("[proxy] sent body: bytes=%d json=false", len(body))
		return
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	var systemText string
	if err := json.Unmarshal(msg.System, &blocks); err == nil {
		for _, block := range blocks {
			systemText += block.Text
		}
	} else {
		json.Unmarshal(msg.System, &systemText)
	}
	log.Printf("[proxy] sent body: bytes=%d messages=%d tools=%d system_bytes=%d stream=%t available_skills=%t mcp_instructions=%t",
		len(body), len(msg.Messages), len(msg.Tools), len(msg.System), msg.Stream,
		strings.Contains(systemText, "<available_skills>"), strings.Contains(systemText, "<mcp_instructions>"))
}
