package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func benchmarkHandler(t *testing.T, transport roundTripFunc) *ProxyHandler {
	t.Helper()
	manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		return refreshResponse(req, http.StatusOK, syntheticRefreshSuccess), nil
	})
	creds := Credentials{AccessToken: "synthetic-token", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	writeSourceCredentials(t, dir, creds)
	manager.creds = &creds
	handler := NewProxyHandler(manager)
	handler.httpClient.Transport = transport
	return handler
}

func benchmarkRequest(t *testing.T, server *httptest.Server, path, mode string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(`{"model":"test","max_tokens":20000,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if mode != "" {
		request.Header.Set("X-CC-Proxy-Mode", mode)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func TestBenchmarkMode_forwards401Once_whenRecognized(t *testing.T) {
	attempts := 0
	handler := benchmarkHandler(t, func(request *http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"type":"authentication_error"}}`))}, nil
	})
	handler.tokenMgr.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Error("benchmark 401 must not trigger refresh")
		return nil, errors.New("blocked benchmark refresh")
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	response := benchmarkRequest(t, server, "/v1/messages", benchmarkMode)
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnauthorized || attempts != 1 || response.Header.Get("X-CC-Proxy-Mode") != benchmarkMode || !strings.Contains(string(body), "authentication_error") {
		t.Fatalf("status=%d attempts=%d ack=%q body=%q", response.StatusCode, attempts, response.Header.Get("X-CC-Proxy-Mode"), body)
	}
}

func TestBenchmarkMode_preserves401Retry_whenAbsent(t *testing.T) {
	attempts := 0
	handler := benchmarkHandler(t, func(request *http.Request) (*http.Response, error) {
		attempts++
		want := "Bearer synthetic-token"
		if attempts == 2 {
			want = "Bearer synthetic-new-access"
		}
		if request.Header.Get("Authorization") != want {
			t.Errorf("authorization did not rotate on retry")
		}
		status := http.StatusUnauthorized
		if attempts == 2 {
			status = http.StatusOK
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	response := benchmarkRequest(t, server, "/v1/other", "")
	if response.StatusCode != http.StatusOK || attempts != 2 || response.Header.Get("X-CC-Proxy-Mode") != "" {
		t.Fatalf("status=%d attempts=%d ack=%q", response.StatusCode, attempts, response.Header.Get("X-CC-Proxy-Mode"))
	}
}

func TestBenchmarkMode_rejectsUnknownBeforeUpstream(t *testing.T) {
	attempts := 0
	handler := benchmarkHandler(t, func(request *http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	response := benchmarkRequest(t, server, "/v1/messages", "unknown-mode")
	if response.StatusCode != http.StatusBadRequest || attempts != 0 || response.Header.Get("X-CC-Proxy-Mode") != "" {
		t.Fatalf("status=%d attempts=%d ack=%q", response.StatusCode, attempts, response.Header.Get("X-CC-Proxy-Mode"))
	}
}

func TestBenchmarkMode_acknowledgesUnhealthyHealth(t *testing.T) {
	manager, _ := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		t.Error("unexpected refresh")
		return nil, errors.New("blocked synthetic refresh")
	})
	server := httptest.NewServer(NewProxyHandler(manager))
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-CC-Proxy-Mode", benchmarkMode)

	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var health struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(body, &health) != nil || !strings.HasPrefix(health.Status, "error:") || strings.Contains(health.Status, "synthetic") || response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("X-CC-Proxy-Mode") != benchmarkMode {
		t.Fatalf("status=%d ack=%q body=%q", response.StatusCode, response.Header.Get("X-CC-Proxy-Mode"), body)
	}
}

func TestBenchmarkMode_stripsInternalAndIdempotencyHeaders(t *testing.T) {
	attempts := 0
	handler := benchmarkHandler(t, func(request *http.Request) (*http.Response, error) {
		attempts++
		if request.Header.Get("X-CC-Proxy-Mode") != "" || request.Header.Get("Idempotency-Key") != "" || request.Header.Get("X-Idempotency-Key") != "" {
			t.Errorf("internal marker or replay key forwarded: %v", request.Header)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		if !strings.Contains(string(body), `"max_tokens":16384`) {
			t.Errorf("request body transformation missing: %q", body)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Cc-Proxy-Mode": {"spoofed"}}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", strings.NewReader(`{"model":"test","max_tokens":20000,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-CC-Proxy-Mode", benchmarkMode)
	request.Header.Set("Idempotency-Key", "marker-one")
	request.Header.Set("X-Idempotency-Key", "marker-two")

	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || attempts != 1 || len(response.Header.Values("X-CC-Proxy-Mode")) != 1 || response.Header.Get("X-CC-Proxy-Mode") != benchmarkMode {
		t.Fatalf("status=%d attempts=%d ack=%v", response.StatusCode, attempts, response.Header.Values("X-CC-Proxy-Mode"))
	}
}

func TestBenchmarkMode_acknowledgesTransportErrorWithoutRetry(t *testing.T) {
	attempts := 0
	transportFailure := errors.New("synthetic transport failure")
	handler := benchmarkHandler(t, func(request *http.Request) (*http.Response, error) {
		attempts++
		return nil, transportFailure
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	response := benchmarkRequest(t, server, "/v1/messages", benchmarkMode)
	if response.StatusCode != http.StatusBadGateway || attempts != 1 || response.Header.Get("X-CC-Proxy-Mode") != benchmarkMode {
		t.Fatalf("status=%d attempts=%d ack=%q", response.StatusCode, attempts, response.Header.Get("X-CC-Proxy-Mode"))
	}
}

func TestBenchmarkMode_forwardsUpstreamErrorOnce(t *testing.T) {
	attempts := 0
	handler := benchmarkHandler(t, func(request *http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{StatusCode: http.StatusInternalServerError, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"type":"api_error"}}`))}, nil
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	response := benchmarkRequest(t, server, "/v1/messages", benchmarkMode)
	if response.StatusCode != http.StatusInternalServerError || attempts != 1 || response.Header.Get("X-CC-Proxy-Mode") != benchmarkMode {
		t.Fatalf("status=%d attempts=%d ack=%q", response.StatusCode, attempts, response.Header.Get("X-CC-Proxy-Mode"))
	}
}

func TestBenchmarkMode_acknowledgesCooldownWithoutUpstream(t *testing.T) {
	handler := NewProxyHandler(nil)
	handler.startCooldown(time.Minute)
	server := httptest.NewServer(handler)
	defer server.Close()

	response := benchmarkRequest(t, server, "/v1/messages", benchmarkMode)
	if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("X-CC-Proxy-Mode") != benchmarkMode {
		t.Fatalf("status=%d ack=%q", response.StatusCode, response.Header.Get("X-CC-Proxy-Mode"))
	}
}

func TestBenchmarkMode_rejectsUnknownHealthBeforeCredentials(t *testing.T) {
	server := httptest.NewServer(NewProxyHandler(nil))
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-CC-Proxy-Mode", "unknown-mode")

	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || response.Header.Get("X-CC-Proxy-Mode") != "" {
		t.Fatalf("status=%d ack=%q", response.StatusCode, response.Header.Get("X-CC-Proxy-Mode"))
	}
}
