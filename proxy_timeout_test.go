package main

import (
	"context"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func timeoutProxyFixture(t *testing.T, upstreamHandler http.HandlerFunc) (*ProxyHandler, *httptest.Server) {
	t.Helper()
	manager, _ := credentialFixture(t, Credentials{"synthetic-access", "synthetic-refresh", time.Now().Add(time.Hour).UnixMilli()})
	handler := NewProxyHandler(manager)
	transport, ok := handler.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatal("proxy must use an HTTP transport")
	}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the request so net/http can detect a disconnected caller while
		// the upstream handler is waiting for cancellation.
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		upstreamHandler(w, r)
	}))
	t.Cleanup(upstream.Close)
	host, _, err := net.SplitHostPort(upstream.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.RootCAs = roots
	transport.TLSClientConfig.ServerName = host
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	t.Cleanup(transport.CloseIdleConnections)
	proxy := httptest.NewServer(handler)
	t.Cleanup(proxy.Close)
	return handler, proxy
}

func timeoutProxyRequest(t *testing.T, ctx context.Context, proxy *httptest.Server) *http.Request {
	t.Helper()
	body := `{"model":"claude-opus-5-5","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"synthetic"}]}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestProxyTimeout_configurationDoesNotLimitResponseBody(t *testing.T) {
	handler := NewProxyHandler(nil)
	if handler.httpClient.Timeout != 0 {
		t.Errorf("client timeout = %s; a whole-request deadline truncates long SSE responses", handler.httpClient.Timeout)
	}
	transport, ok := handler.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatal("proxy must use an HTTP transport")
	}
	if transport.ResponseHeaderTimeout != 120*time.Second {
		t.Errorf("response header timeout = %s, want 120s", transport.ResponseHeaderTimeout)
	}
}

func TestProxyTimeout_rejectsDelayedResponseHeaders(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	handler, proxy := timeoutProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			close(upstreamCanceled)
		case <-timer.C:
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{}`)
		}
	})
	handler.httpClient.Transport.(*http.Transport).ResponseHeaderTimeout = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := proxy.Client().Do(timeoutProxyRequest(t, ctx, proxy))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "upstream request failed") {
		t.Fatalf("delayed headers: status=%d body=%q", resp.StatusCode, body)
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(time.Second):
		t.Fatal("header timeout did not cancel the upstream request")
	}
}

const timeoutMessageStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"synthetic-message\",\"content\":[],\"usage\":{\"input_tokens\":1}}}\n\n"
const timeoutMessageStop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func writeTimeoutSSE(w http.ResponseWriter, event string) error {
	if _, err := io.WriteString(w, event); err != nil {
		return err
	}
	return http.NewResponseController(w).Flush()
}

func serveTimeoutSSE(w http.ResponseWriter, r *http.Request, duration time.Duration) error {
	w.Header().Set("Content-Type", "text/event-stream")
	if err := writeTimeoutSSE(w, timeoutMessageStart); err != nil {
		return err
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	ticker := time.NewTicker(min(duration/4, time.Second))
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return r.Context().Err()
		case <-ticker.C:
			if err := writeTimeoutSSE(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n"); err != nil {
				return err
			}
		case <-timer.C:
			return writeTimeoutSSE(w, timeoutMessageStop)
		}
	}
}

func testProxySSECompletion(t *testing.T, duration, headerTimeout time.Duration) {
	t.Helper()
	upstreamDone := make(chan error, 1)
	handler, proxy := timeoutProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamDone <- serveTimeoutSSE(w, r, duration)
	})
	if headerTimeout > 0 {
		handler.httpClient.Transport.(*http.Transport).ResponseHeaderTimeout = headerTimeout
	}
	// Keep the production client deadline untouched, including in the opt-in
	// test that would fail at 120 seconds with the previous implementation.
	ctx, cancel := context.WithTimeout(context.Background(), duration+10*time.Second)
	defer cancel()
	resp, err := proxy.Client().Do(timeoutProxyRequest(t, ctx, proxy))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("SSE response: status=%d content-type=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"type":"message_start"`) || !strings.Contains(string(body), `"type":"message_stop"`) {
		t.Fatalf("SSE did not reach message_stop after %s: %s", duration, body)
	}
	select {
	case err := <-upstreamDone:
		if err != nil {
			t.Fatalf("upstream stream: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream stream did not finish")
	}
}

func TestProxyTimeout_streamContinuesAfterResponseHeaders(t *testing.T) {
	testProxySSECompletion(t, 600*time.Millisecond, 200*time.Millisecond)
}

func TestProxyTimeout_longSSEExceeds120Seconds(t *testing.T) {
	if os.Getenv("CC_PROXY_LONG_STREAM_TEST") != "1" {
		t.Skip("set CC_PROXY_LONG_STREAM_TEST=1 to run the 125-second local SSE regression")
	}
	testProxySSECompletion(t, 125*time.Second, 0)
}

func TestProxyTimeout_callerCancellationReachesUpstream(t *testing.T) {
	upstreamDone := make(chan error, 1)
	_, proxy := timeoutProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if err := writeTimeoutSSE(w, timeoutMessageStart); err != nil {
			upstreamDone <- err
			return
		}
		<-r.Context().Done()
		upstreamDone <- r.Context().Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := proxy.Client().Do(timeoutProxyRequest(t, ctx, proxy))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE response status = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-upstreamDone:
		if err != context.Canceled {
			t.Fatalf("upstream cancellation = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not reach the upstream request")
	}
}
