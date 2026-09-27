package main

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestLogSentBodySummary_reportsOptionalBlockPresence_withoutLoggingContent(t *testing.T) {
	// Given: optional catalog markers inside synthetic system text, not in user text.
	const secret = "synthetic-private-catalog-description"
	request := `{"model":"claude-opus-5-5","system":[{"type":"text","text":"core <available_skills>` + secret + `</available_skills> <mcp_instructions>tools</mcp_instructions>"}],"messages":[{"role":"user","content":"hello"}]}`
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	// When: the proxy summarizes the outbound body.
	LogSentBodySummary([]byte(request))

	// Then: presence is visible but catalog contents are never logged.
	if !strings.Contains(logs.String(), "available_skills=true") || !strings.Contains(logs.String(), "mcp_instructions=true") {
		t.Errorf("optional system markers missing from structural summary: %s", logs.String())
	}
	if strings.Contains(logs.String(), secret) {
		t.Error("system catalog text leaked into logs")
	}
}
