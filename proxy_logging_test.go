package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestProxyHandler_doesNotLogSecrets_whenUpstreamRejectsRequest(t *testing.T) {
	// Given: only synthetic credentials and a local in-memory upstream.
	const token = "synthetic-bearer-secret-1234567890"
	const prompt = "synthetic-private-user-prompt"
	const schema = "synthetic-private-tool-schema"
	const upstreamError = "synthetic-private-upstream-error"
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		t.Error("unexpected refresh")
		return refreshResponse(req, http.StatusBadRequest, `{"error":"invalid_grant"}`), nil
	})
	creds := Credentials{AccessToken: token, ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	writeSourceCredentials(t, dir, creds)
	manager.creds = &creds
	handler := NewProxyHandler(manager)
	handler.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Status:     "429 Too Many Requests",
			Header:     http.Header{"Content-Type": {"application/json"}, "Request-Id": {"req-synthetic"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"` + upstreamError + `"}}`)),
			Request:    req,
		}, nil
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5","max_tokens":16,"messages":[{"role":"user","content":"`+prompt+`"}],"tools":[{"name":"test_tool","input_schema":{"description":"`+schema+`"}}]}`))
	recorder := httptest.NewRecorder()

	// When: the handler sends the transformed request and receives an upstream error.
	handler.ServeHTTP(recorder, request)

	// Then: logs preserve status/correlation, but no payload or credential fragments.
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", recorder.Code)
	}
	for _, secret := range []string{token, "synthetic-be", "7890", prompt, schema, upstreamError} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs contain synthetic private value %q", secret)
		}
	}
	if !strings.Contains(logs.String(), "429") || !strings.Contains(logs.String(), "req-synthetic") {
		t.Errorf("logs lack safe status or request id: %s", logs.String())
	}
}
