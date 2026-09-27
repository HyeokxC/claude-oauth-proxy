package main

import (
	"bytes"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamTransformResponse_logsOnlyStructuralOutcome_whenUpstreamStopsForFilter(t *testing.T) {
	// Given: a synthetic upstream SSE with a text delta and explicit filter stop reason.
	const secret = "synthetic-private-text-delta"
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":4}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"" + secret + "\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\"},\"usage\":{\"output_tokens\":0}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	recorder := httptest.NewRecorder()

	// When: the proxy streams the upstream events to a caller.
	StreamTransformResponse(recorder, io.NopCloser(strings.NewReader(sse)))

	// Then: SSE framing and stop reason survive, but only structural data enters logs.
	if strings.Count(recorder.Body.String(), "\n\n") != 4 || !strings.Contains(recorder.Body.String(), secret) || !strings.Contains(recorder.Body.String(), `"stop_reason":"refusal"`) {
		t.Error("SSE event boundaries or response fields changed")
	}
	if strings.Contains(logs.String(), secret) {
		t.Error("SSE text delta leaked into logs")
	}
	if !strings.Contains(logs.String(), "stop_reason=refusal") || !strings.Contains(logs.String(), "output_tokens=0") || !strings.Contains(logs.String(), "input_tokens=4") || !strings.Contains(logs.String(), "malformed=0") || !strings.Contains(logs.String(), "message_delta=1") {
		t.Errorf("structural SSE outcome not logged: %s", logs.String())
	}
}
