package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// UsageUpstreamBase is the origin used for the read-only OAuth quota lookup.
// It is a variable so tests can point it at a local stub.
var UsageUpstreamBase = UpstreamBase

const (
	usagePath           = "/api/oauth/usage"
	usageCacheTTL       = 60 * time.Second
	usageRequestTimeout = 10 * time.Second
	usageBackoffMin     = 30 * time.Second
	usageBackoffMax     = 15 * time.Minute
	usageFailureGate    = 30 * time.Second
	usageMaxBodyBytes   = 1 << 20
)

type usageWindow struct {
	ID          string  `json:"id"`
	UsedPercent float64 `json:"usedPercent"`
	ResetsAt    *string `json:"resetsAt"`
}

type usageSnapshot struct {
	FetchedAt  int64
	Windows    []usageWindow
	ExtraUsage map[string]any
	elapsed    time.Duration
}

type usageResponse struct {
	Source     string         `json:"source"`
	FetchedAt  int64          `json:"fetchedAt"`
	Stale      bool           `json:"stale"`
	Windows    []usageWindow  `json:"windows"`
	ExtraUsage map[string]any `json:"extraUsage,omitempty"`
}

type usageErrorResponse struct {
	Error             string `json:"error"`
	RetryAfterSeconds *int   `json:"retryAfterSeconds"`
}

type usageCall struct {
	done chan struct{}
}

type usageService struct {
	tokenMgr *TokenManager
	baseURL  string
	client   *http.Client
	now      func() time.Time
	// onJoin is a test hook invoked (under lock) when a caller joins an in-flight fetch.
	onJoin func()

	mu         sync.Mutex
	good       *usageSnapshot
	goodUntil  time.Time
	gateUntil  time.Time
	gateReason string
	backoff    time.Duration
	inflight   *usageCall
}

var usageServices sync.Map

func newUsageService(tm *TokenManager) *usageService {
	return &usageService{
		tokenMgr: tm,
		baseURL:  UsageUpstreamBase,
		client: &http.Client{
			Timeout: usageRequestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now: time.Now,
	}
}

func (p *ProxyHandler) usage() *usageService {
	if svc, ok := usageServices.Load(p); ok {
		return svc.(*usageService)
	}
	svc, _ := usageServices.LoadOrStore(p, newUsageService(p.tokenMgr))
	return svc.(*usageService)
}

func (p *ProxyHandler) handleUsage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeUsageJSON(w, http.StatusMethodNotAllowed, usageErrorResponse{Error: "method not allowed"})
		return
	}
	snap, stale, reason, retryAfter := p.usage().get()
	if snap == nil {
		if retryAfter != nil {
			w.Header().Set("Retry-After", strconv.Itoa(*retryAfter))
		}
		writeUsageJSON(w, http.StatusServiceUnavailable, usageErrorResponse{Error: reason, RetryAfterSeconds: retryAfter})
		return
	}
	windows := snap.Windows
	if windows == nil {
		windows = []usageWindow{}
	}
	writeUsageJSON(w, http.StatusOK, usageResponse{
		Source:     "anthropic-oauth",
		FetchedAt:  snap.FetchedAt,
		Stale:      stale,
		Windows:    windows,
		ExtraUsage: snap.ExtraUsage,
	})
}

func writeUsageJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"encode failed","retryAfterSeconds":null}`))
		return
	}
	w.WriteHeader(status)
	w.Write(data)
}

func (s *usageService) get() (*usageSnapshot, bool, string, *int) {
	s.mu.Lock()
	for {
		now := s.now()
		if s.good != nil && now.Before(s.goodUntil) {
			snap := s.good
			s.mu.Unlock()
			return snap, false, "", nil
		}
		if now.Before(s.gateUntil) {
			snap, reason := s.good, s.gateReason
			retry := int(math.Ceil(s.gateUntil.Sub(now).Seconds()))
			s.mu.Unlock()
			if snap != nil {
				return snap, true, "", nil
			}
			return nil, false, reason, &retry
		}
		if s.inflight == nil {
			break
		}
		call := s.inflight
		if s.onJoin != nil {
			s.onJoin()
		}
		s.mu.Unlock()
		<-call.done
		s.mu.Lock()
	}
	call := &usageCall{done: make(chan struct{})}
	s.inflight = call
	s.mu.Unlock()

	snap, failure := s.fetch()

	s.mu.Lock()
	now := s.now()
	if failure == nil {
		s.good = snap
		s.goodUntil = now.Add(usageCacheTTL)
		s.gateUntil = time.Time{}
		s.backoff = 0
	} else {
		gate := usageFailureGate
		if failure.rateLimited {
			if s.backoff == 0 {
				s.backoff = usageBackoffMin
			} else {
				s.backoff = min(s.backoff*2, usageBackoffMax)
			}
			gate = max(s.backoff, failure.retryAfter)
		}
		s.gateUntil = now.Add(gate)
		s.gateReason = failure.reason
	}
	s.logLocked(failure, snap)
	s.inflight = nil
	close(call.done)
	good, gateUntil, reason := s.good, s.gateUntil, s.gateReason
	s.mu.Unlock()

	if failure == nil {
		return snap, false, "", nil
	}
	if good != nil {
		return good, true, "", nil
	}
	retry := int(math.Ceil(gateUntil.Sub(now).Seconds()))
	return nil, false, reason, &retry
}

type usageFailure struct {
	status      int
	elapsed     time.Duration
	reason      string
	rateLimited bool
	retryAfter  time.Duration
}

func (s *usageService) logLocked(failure *usageFailure, snap *usageSnapshot) {
	if failure == nil {
		log.Printf("[usage] upstream 200 in %dms (cached until %s)",
			snap.elapsed.Milliseconds(), s.goodUntil.Format(time.RFC3339))
		return
	}
	status := "error"
	if failure.status != 0 {
		status = strconv.Itoa(failure.status)
	}
	log.Printf("[usage] upstream %s in %dms (%s; retry after %s)",
		status, failure.elapsed.Milliseconds(), failure.reason, s.gateUntil.Format(time.RFC3339))
}

func (s *usageService) fetch() (*usageSnapshot, *usageFailure) {
	start := s.now()
	token, err := s.tokenMgr.GetValidToken()
	if err != nil {
		return nil, &usageFailure{elapsed: s.now().Sub(start), reason: "usage credential unavailable"}
	}
	status, header, body, err := s.request(token)
	if err == nil && status == http.StatusUnauthorized {
		if s.tokenMgr.RefreshAfterUnauthorized(token) == nil {
			if token, err = s.tokenMgr.GetValidToken(); err == nil {
				status, header, body, err = s.request(token)
			}
		}
	}
	elapsed := s.now().Sub(start)
	if err != nil {
		return nil, &usageFailure{elapsed: elapsed, reason: "usage upstream request failed"}
	}
	switch {
	case status == http.StatusTooManyRequests:
		return nil, &usageFailure{status: status, elapsed: elapsed, reason: "usage upstream rate limited",
			rateLimited: true, retryAfter: parseRetryAfter(header.Get("Retry-After"), s.now())}
	case status != http.StatusOK:
		return nil, &usageFailure{status: status, elapsed: elapsed, reason: fmt.Sprintf("usage upstream returned status %d", status)}
	}
	snap, err := parseUsageBody(body)
	if err != nil {
		return nil, &usageFailure{status: status, elapsed: elapsed, reason: "usage upstream returned invalid data"}
	}
	snap.FetchedAt = s.now().UnixMilli()
	snap.elapsed = elapsed
	return snap, nil
}

func (s *usageService) request(token string) (int, http.Header, []byte, error) {
	// Detached from any single caller: concurrent callers share this request.
	ctx, cancel := context.WithTimeout(context.Background(), usageRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.baseURL, "/")+usagePath, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "claude-cli/"+CCVersion+" (external, sdk-cli)")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, usageMaxBodyBytes))
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, body, nil
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if secs, err := strconv.Atoi(value); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}

// parseUsageBody maps every top-level object with a numeric "utilization" to a
// window, preserving upstream key order. extra_usage is reported separately.
// Model-scoped entries of the "limits" list (e.g. the weekly Fable limit) follow
// as "<window>_<model>" windows unless a top-level key already reported them.
func parseUsageBody(body []byte) (*usageSnapshot, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("usage body is not an object")
	}
	snap := &usageSnapshot{Windows: []usageWindow{}}
	var limits []map[string]any
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := keyTok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		if key == "limits" {
			_ = json.Unmarshal(raw, &limits)
			continue
		}
		var obj map[string]any
		if json.Unmarshal(raw, &obj) != nil || obj == nil {
			continue
		}
		if key == "extra_usage" {
			snap.ExtraUsage = scalarFields(obj)
			continue
		}
		util, ok := obj["utilization"].(float64)
		if !ok {
			continue
		}
		window := usageWindow{ID: key, UsedPercent: math.Min(100, math.Max(0, util))}
		if resets, ok := obj["resets_at"].(string); ok {
			if _, err := time.Parse(time.RFC3339Nano, resets); err == nil {
				window.ResetsAt = &resets
			}
		}
		snap.Windows = append(snap.Windows, window)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	snap.Windows = append(snap.Windows, scopedLimitWindows(limits, snap.Windows)...)
	return snap, nil
}

var limitGroupWindow = map[string]string{"weekly": "seven_day", "session": "five_hour", "five_hour": "five_hour"}

func scopedLimitWindows(limits []map[string]any, existing []usageWindow) []usageWindow {
	seen := map[string]bool{}
	for _, w := range existing {
		seen[w.ID] = true
	}
	out := []usageWindow{}
	for _, limit := range limits {
		percent, ok := limit["percent"].(float64)
		if !ok {
			continue
		}
		scope, _ := limit["scope"].(map[string]any)
		model, _ := scope["model"].(map[string]any)
		name, _ := model["display_name"].(string)
		group, _ := limit["group"].(string)
		prefix := limitGroupWindow[group]
		slug := strings.Trim(strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
				return r
			}
			return '_'
		}, strings.ToLower(name)), "_")
		if prefix == "" || slug == "" || seen[prefix+"_"+slug] {
			continue
		}
		id := prefix + "_" + slug
		seen[id] = true
		window := usageWindow{ID: id, UsedPercent: math.Min(100, math.Max(0, percent))}
		if resets, ok := limit["resets_at"].(string); ok {
			if _, err := time.Parse(time.RFC3339Nano, resets); err == nil {
				window.ResetsAt = &resets
			}
		}
		out = append(out, window)
	}
	return out
}

func scalarFields(obj map[string]any) map[string]any {
	out := map[string]any{}
	hasNumber := false
	for k, v := range obj {
		switch v := v.(type) {
		case float64:
			out[k] = v
			hasNumber = true
		case bool:
			out[k] = v
		}
	}
	if !hasNumber {
		return nil
	}
	return out
}
