package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateLimitCooldown is how long the proxy short-circuits incoming requests
// after upstream returns 429. Anthropic's per-minute bucket needs a quiet
// window to refill; without this, opencode's retry loop keeps the bucket
// pinned at zero and 429s never clear.
const RateLimitCooldown = 15 * time.Second

type ProxyHandler struct {
	tokenMgr   *TokenManager
	httpClient *http.Client

	cooldownMu    sync.Mutex
	cooldownUntil time.Time
}

func (p *ProxyHandler) inCooldown() time.Duration {
	p.cooldownMu.Lock()
	defer p.cooldownMu.Unlock()
	if d := time.Until(p.cooldownUntil); d > 0 {
		return d
	}
	return 0
}

func (p *ProxyHandler) startCooldown(d time.Duration) {
	p.cooldownMu.Lock()
	defer p.cooldownMu.Unlock()
	until := time.Now().Add(d)
	if until.After(p.cooldownUntil) {
		p.cooldownUntil = until
	}
}

func NewProxyHandler(tokenMgr *TokenManager) *ProxyHandler {
	return &ProxyHandler{
		tokenMgr: tokenMgr,
		httpClient: &http.Client{
			// No client-wide timeout: it also cuts off long-running SSE bodies.
			// Bound connection setup and header waits independently instead.
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   120 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
				TLSHandshakeTimeout:   120 * time.Second,
				ResponseHeaderTimeout: 120 * time.Second,
				MaxIdleConns:          10,
				IdleConnTimeout:       90 * time.Second,
				DisableCompression:    true, // preserve SSE streaming
				MaxIdleConnsPerHost:   10,
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

	// Short-circuit while we're cooling down from a recent upstream 429.
	// Returning a synthetic 429 with Retry-After is the cleanest signal
	// to clients (and stops their retry storm from holding the bucket open).
	if remaining := p.inCooldown(); remaining > 0 {
		retryAfter := int(remaining.Seconds()) + 1
		log.Printf("[proxy] cooldown active, short-circuiting 429 (retry in %ds)", retryAfter)
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"proxy cooldown active, slow down"}}`))
		return
	}

	token, err := p.tokenMgr.GetValidToken()
	if err != nil {
		log.Printf("[proxy] credential unavailable")
		http.Error(w, `{"error":"proxy credential unavailable"}`, http.StatusServiceUnavailable)
		return
	}

	resp, err := p.doUpstreamRequest(r, token)
	if err != nil {
		log.Printf("[proxy] upstream transport failed")
		http.Error(w, `{"error":"upstream request failed"}`, http.StatusBadGateway)
		return
	}

	// Retry once on 401 with refreshed token
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		log.Printf("[proxy] got 401, refreshing token and retrying...")
		if err := p.tokenMgr.RefreshAfterUnauthorized(token); err != nil {
			log.Printf("[proxy] force refresh failed")
			http.Error(w, `{"error":"token refresh failed"}`, http.StatusServiceUnavailable)
			return
		}
		token, err = p.tokenMgr.GetValidToken()
		if err != nil {
			log.Printf("[proxy] credential unavailable after refresh")
			http.Error(w, `{"error":"proxy credential unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		resp, err = p.doUpstreamRequest(r, token)
		if err != nil {
			log.Printf("[proxy] retry upstream transport failed")
			http.Error(w, `{"error":"upstream retry failed"}`, http.StatusBadGateway)
			return
		}
	}

	log.Printf("[proxy] upstream responded: status=%d request_id=%q content_type=%q",
		resp.StatusCode, resp.Header.Get("Request-Id"), resp.Header.Get("Content-Type"))

	// Error payloads can contain user content; forward them without logging their text.
	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var failure struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if json.Unmarshal(bodyBytes, &failure) == nil {
			switch failure.Error.Type {
			case "rate_limit_error", "authentication_error", "permission_error", "invalid_request_error", "overloaded_error", "api_error":
				log.Printf("[proxy] upstream error type=%s status=%d", failure.Error.Type, resp.StatusCode)
			}
		}
		LogRateLimitHeaders(resp)

		// On 429, start a cooldown so the per-minute bucket can refill.
		// Honor upstream Retry-After if it's longer than our default.
		if resp.StatusCode == http.StatusTooManyRequests {
			cd := RateLimitCooldown
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs > 0 {
					if d := time.Duration(secs) * time.Second; d > cd {
						cd = d
					}
				}
			}
			p.startCooldown(cd)
			log.Printf("[proxy] entering cooldown for %s after upstream 429", cd)
		}

		// Still forward the error to the client
		CopyResponseHeaders(w, resp)
		w.Write(bodyBytes)
		return
	}

	isMessages := strings.Contains(r.URL.Path, "/messages")

	// Copy response headers, removing Content-Length for transformed responses
	// (transformation may change body size)
	CopyResponseHeaders(w, resp, isMessages)

	// Transform and stream response
	if isMessages && IsSSE(resp) {
		StreamTransformResponse(w, resp.Body)
		// StreamTransformResponse closes body
	} else if isMessages {
		ReadAndTransformBody(w, resp.Body)
		// ReadAndTransformBody closes body
	} else {
		io.Copy(w, resp.Body)
		resp.Body.Close()
	}
}

func (p *ProxyHandler) doUpstreamRequest(r *http.Request, token string) (*http.Response, error) {
	// Read request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	// Keep the original payload available for the single authentication retry.
	if err := r.Body.Close(); err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	// Transform body for /messages endpoints
	isMessages := strings.Contains(r.URL.Path, "/messages")
	if isMessages && len(body) > 0 {
		body = TransformRequestBody(body)
		LogSentBodySummary(body)
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
	if err := TransformRequestHeaders(upstream, r, token); err != nil {
		return nil, err
	}
	upstream.ContentLength = int64(len(body))

	log.Printf("[proxy] %s %s (body: %d bytes)", r.Method, r.URL.Path, len(body))

	return p.httpClient.Do(upstream)
}

// LogRateLimitHeaders prints any anthropic/x-ratelimit/retry-after headers from
// the upstream response so we can see why the request was rejected.
func LogRateLimitHeaders(resp *http.Response) {
	for k, vv := range resp.Header {
		lk := strings.ToLower(k)
		if lk == "retry-after" ||
			strings.HasPrefix(lk, "anthropic-ratelimit-") ||
			strings.HasPrefix(lk, "x-ratelimit-") {
			log.Printf("[proxy]   << %s: %s", k, strings.Join(vv, ", "))
		}
	}
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
