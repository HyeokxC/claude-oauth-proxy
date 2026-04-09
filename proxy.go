package main

import (
	"bytes"
	"crypto/tls"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type ProxyHandler struct {
	tokenMgr   *TokenManager
	httpClient *http.Client
}

func NewProxyHandler(tokenMgr *TokenManager) *ProxyHandler {
	return &ProxyHandler{
		tokenMgr: tokenMgr,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
				MaxIdleConns:        10,
				IdleConnTimeout:     90 * time.Second,
				DisableCompression:  true, // preserve SSE streaming
				MaxIdleConnsPerHost: 10,
			},
			// Don't follow redirects
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (p *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Health check
	if r.URL.Path == "/health" {
		p.handleHealth(w)
		return
	}

	token, err := p.tokenMgr.GetValidToken()
	if err != nil {
		log.Printf("[proxy] token error: %v", err)
		http.Error(w, `{"error":"proxy token error: `+err.Error()+`"}`, http.StatusServiceUnavailable)
		return
	}

	resp, err := p.doUpstreamRequest(r, token)
	if err != nil {
		log.Printf("[proxy] upstream error: %v", err)
		http.Error(w, `{"error":"upstream request failed: `+err.Error()+`"}`, http.StatusBadGateway)
		return
	}

	// Retry once on 401 with refreshed token
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		log.Printf("[proxy] got 401, refreshing token and retrying...")
		if err := p.tokenMgr.ForceRefresh(); err != nil {
			log.Printf("[proxy] force refresh failed: %v", err)
			http.Error(w, `{"error":"token refresh failed"}`, http.StatusServiceUnavailable)
			return
		}
		token, _ = p.tokenMgr.GetValidToken()
		resp, err = p.doUpstreamRequest(r, token)
		if err != nil {
			log.Printf("[proxy] retry upstream error: %v", err)
			http.Error(w, `{"error":"upstream retry failed"}`, http.StatusBadGateway)
			return
		}
	}
	defer resp.Body.Close()

	// Copy response headers
	CopyResponseHeaders(w, resp)

	// Transform and stream response
	isMessages := strings.Contains(r.URL.Path, "/messages")
	if isMessages && IsSSE(resp) {
		StreamTransformResponse(w, resp.Body)
	} else if isMessages {
		ReadAndTransformBody(w, resp.Body)
	} else {
		io.Copy(w, resp.Body)
	}
}

func (p *ProxyHandler) doUpstreamRequest(r *http.Request, token string) (*http.Response, error) {
	// Read request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}

	// Transform body for /messages endpoints
	isMessages := strings.Contains(r.URL.Path, "/messages")
	if isMessages && len(body) > 0 {
		body = TransformRequestBody(body)
	}

	// Build upstream URL
	upstreamURL := UpstreamBase + r.URL.Path
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	upstream, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	// Transform headers
	TransformRequestHeaders(upstream, r, token)
	upstream.ContentLength = int64(len(body))

	log.Printf("[proxy] %s %s -> %s (body: %d bytes)", r.Method, r.URL.Path, upstreamURL, len(body))

	return p.httpClient.Do(upstream)
}

func (p *ProxyHandler) handleHealth(w http.ResponseWriter) {
	token, err := p.tokenMgr.GetValidToken()
	status := "ok"
	if err != nil {
		status = "error: " + err.Error()
	}

	w.Header().Set("Content-Type", "application/json")
	if token == "" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	w.Write([]byte(`{"status":"` + status + `"}`))
}
