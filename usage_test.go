package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const usageTestToken = "synthetic-usage-bearer-secret-0987654321"

type usageClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *usageClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *usageClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type usageFixture struct {
	handler *ProxyHandler
	svc     *usageService
	clock   *usageClock
	hits    *atomic.Int32
	logs    *bytes.Buffer
}

func newUsageFixture(t *testing.T, upstream http.HandlerFunc) *usageFixture {
	t.Helper()
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	hits := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		upstream(w, r)
	}))
	t.Cleanup(server.Close)
	manager := NewTokenManager()
	manager.creds = &Credentials{AccessToken: usageTestToken, ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	handler := NewProxyHandler(manager)
	clock := &usageClock{now: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}
	svc := handler.usage()
	svc.baseURL = server.URL
	svc.now = clock.Now
	t.Cleanup(func() { usageServices.Delete(handler) })
	return &usageFixture{handler: handler, svc: svc, clock: clock, hits: hits, logs: &logs}
}

func (f *usageFixture) get(t *testing.T, method string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, httptest.NewRequest(method, "/usage", nil))
	if ct := recorder.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	if cc := recorder.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("cache-control = %q", cc)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), usageTestToken) || strings.Contains(f.logs.String(), usageTestToken) {
		t.Fatal("token leaked into response or logs")
	}
	return recorder, body
}

func usageOK(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}
}

const usageSampleBody = `{"five_hour":{"utilization":12.5,"resets_at":"2026-11-05T17:00:00.123456+00:00"},"seven_day":{"utilization":40,"resets_at":null},"seven_day_opus":null,"extra_usage":{"is_enabled":true,"monthly_limit":5000,"used_credits":12.5,"utilization":null,"currency":"USD"}}`

func TestUsage_mapsUpstreamWindowsAndSkipsNullKeys(t *testing.T) {
	// Given: an upstream reporting two windows, a null window and extra usage.
	var gotAuth, gotBeta, gotAccept, gotPath string
	f := newUsageFixture(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotBeta, gotAccept, gotPath = r.Header.Get("Authorization"), r.Header.Get("anthropic-beta"), r.Header.Get("Accept"), r.URL.Path
		usageOK(usageSampleBody)(w, r)
	})
	// When: ADE asks for usage.
	recorder, body := f.get(t, http.MethodGet)
	// Then: windows keep upstream ids and order, null keys are skipped, extra usage keeps numbers/booleans only.
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if gotAuth != "Bearer "+usageTestToken || gotBeta != "oauth-2025-04-20" || gotAccept != "application/json" || gotPath != "/api/oauth/usage" {
		t.Errorf("upstream request: path=%q beta=%q accept=%q auth-ok=%v", gotPath, gotBeta, gotAccept, gotAuth == "Bearer "+usageTestToken)
	}
	if body["source"] != "anthropic-oauth" || body["stale"] != false || body["fetchedAt"] != float64(f.clock.Now().UnixMilli()) {
		t.Errorf("envelope = %v", body)
	}
	windows, _ := json.Marshal(body["windows"])
	want := `[{"id":"five_hour","resetsAt":"2026-11-05T17:00:00.123456+00:00","usedPercent":12.5},{"id":"seven_day","resetsAt":null,"usedPercent":40}]`
	if string(windows) != want {
		t.Errorf("windows = %s", windows)
	}
	extra, _ := json.Marshal(body["extraUsage"])
	if string(extra) != `{"is_enabled":true,"monthly_limit":5000,"used_credits":12.5}` {
		t.Errorf("extraUsage = %s", extra)
	}
	if !strings.Contains(f.logs.String(), "[usage] upstream 200 in ") || !strings.Contains(f.logs.String(), "cached until") {
		t.Errorf("log line missing: %s", f.logs.String())
	}
}

func TestUsage_addsModelScopedLimitsAsWindows(t *testing.T) {
	// Given: the 2026-11 body shape, where the weekly Fable limit lives only in "limits".
	body := `{"five_hour":null,"seven_day":{"utilization":69,"resets_at":"2026-11-11T17:00:00.283344+00:00"},"seven_day_opus":null,` +
		`"limits":[{"kind":"weekly_all","group":"weekly","percent":69,"resets_at":"2026-11-11T17:00:00.283344+00:00","scope":null},` +
		`{"kind":"weekly_scoped","group":"weekly","percent":9,"resets_at":"2026-11-11T17:00:00.283543+00:00","scope":{"model":{"id":null,"display_name":"Fable"},"surface":null},"is_active":false},` +
		`{"kind":"weekly_scoped","group":"weekly","percent":5,"scope":{"model":{"display_name":"Opus"}}},` +
		`{"kind":"odd","group":"monthly","percent":1,"scope":{"model":{"display_name":"X"}}}]}`
	snap, err := parseUsageBody([]byte(body))
	// Then: the Fable limit follows the top-level windows; unscoped and unknown groups add nothing.
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(snap.Windows)
	want := `[{"id":"seven_day","usedPercent":69,"resetsAt":"2026-11-11T17:00:00.283344+00:00"},` +
		`{"id":"seven_day_fable","usedPercent":9,"resetsAt":"2026-11-11T17:00:00.283543+00:00"},{"id":"seven_day_opus","usedPercent":5,"resetsAt":null}]`
	if string(got) != want {
		t.Errorf("windows = %s", got)
	}
	// And: a top-level key wins over the same scoped limit.
	dup, _ := parseUsageBody([]byte(`{"seven_day_fable":{"utilization":20,"resets_at":null},"limits":[{"group":"weekly","percent":9,"scope":{"model":{"display_name":"Fable"}}}]}`))
	if len(dup.Windows) != 1 || dup.Windows[0].UsedPercent != 20 {
		t.Errorf("duplicate windows = %+v", dup.Windows)
	}
}

func TestUsage_servesCacheForSixtySeconds(t *testing.T) {
	// Given: a successful first lookup whose extra usage has no numeric fields.
	f := newUsageFixture(t, usageOK(`{"five_hour":{"utilization":1,"resets_at":null},"extra_usage":{"is_enabled":false,"monthly_limit":null}}`))
	_, first := f.get(t, http.MethodGet)
	if _, ok := first["extraUsage"]; ok {
		t.Errorf("extraUsage without numbers must be omitted: %v", first)
	}
	// When: asking again just before and just after the 60 s TTL.
	f.clock.Advance(59 * time.Second)
	_, cached := f.get(t, http.MethodGet)
	hitsBeforeExpiry := f.hits.Load()
	f.clock.Advance(2 * time.Second)
	_, refreshed := f.get(t, http.MethodGet)
	// Then: only the expired lookup reaches upstream again.
	if hitsBeforeExpiry != 1 || f.hits.Load() != 2 {
		t.Fatalf("hits before expiry=%d after=%d", hitsBeforeExpiry, f.hits.Load())
	}
	if cached["fetchedAt"] != first["fetchedAt"] || refreshed["fetchedAt"] == first["fetchedAt"] {
		t.Errorf("fetchedAt first=%v cached=%v refreshed=%v", first["fetchedAt"], cached["fetchedAt"], refreshed["fetchedAt"])
	}
}

func TestUsage_rateLimitServesStaleAndBacksOff(t *testing.T) {
	// Given: one good lookup, then an upstream that only answers 429.
	var limited atomic.Bool
	f := newUsageFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if limited.Load() {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"synthetic-rate-limit-detail"}`))
			return
		}
		usageOK(usageSampleBody)(w, r)
	})
	_, good := f.get(t, http.MethodGet)
	limited.Store(true)
	f.clock.Advance(61 * time.Second)
	// When: the cache expires while upstream is rate limited.
	recorder, stale := f.get(t, http.MethodGet)
	// Then: the last good result is served as stale and upstream is left alone for 30 s, then 60 s.
	if recorder.Code != http.StatusOK || stale["stale"] != true || stale["fetchedAt"] != good["fetchedAt"] {
		t.Fatalf("status=%d body=%v", recorder.Code, stale)
	}
	steps := []struct {
		advance  time.Duration
		wantHits int32
	}{{29 * time.Second, 2}, {2 * time.Second, 3}, {59 * time.Second, 3}, {2 * time.Second, 4}}
	for _, step := range steps {
		f.clock.Advance(step.advance)
		_, body := f.get(t, http.MethodGet)
		if body["stale"] != true || f.hits.Load() != step.wantHits {
			t.Fatalf("after +%s: stale=%v hits=%d want %d", step.advance, body["stale"], f.hits.Load(), step.wantHits)
		}
	}
	if strings.Contains(f.logs.String(), "synthetic-rate-limit-detail") {
		t.Error("upstream error body leaked into logs")
	}
}

func TestUsage_rateLimitWithoutCacheHonoursRetryAfter(t *testing.T) {
	// Given: no cached result and a 429 with a long Retry-After.
	f := newUsageFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	// When: usage is requested twice inside the window.
	recorder, body := f.get(t, http.MethodGet)
	f.clock.Advance(100 * time.Second)
	_, again := f.get(t, http.MethodGet)
	// Then: 503 with the upstream retry hint, and no second upstream call.
	if recorder.Code != http.StatusServiceUnavailable || body["retryAfterSeconds"] != float64(120) || recorder.Header().Get("Retry-After") != "120" {
		t.Fatalf("status=%d body=%v", recorder.Code, body)
	}
	if again["retryAfterSeconds"] != float64(20) || f.hits.Load() != 1 {
		t.Errorf("second body=%v hits=%d", again, f.hits.Load())
	}
}

func TestUsage_upstreamFailureWithoutCacheReturns503WithoutBody(t *testing.T) {
	// Given: an upstream 500 whose body carries private-looking detail.
	const detail = "synthetic-private-upstream-detail account=acct_123"
	f := newUsageFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(detail))
	})
	// When: usage is requested with nothing cached.
	recorder, body := f.get(t, http.MethodGet)
	// Then: a short safe reason is returned and the upstream body appears nowhere.
	if recorder.Code != http.StatusServiceUnavailable || body["error"] != "usage upstream returned status 500" || body["retryAfterSeconds"] != float64(30) {
		t.Fatalf("status=%d body=%v", recorder.Code, body)
	}
	for _, out := range []string{recorder.Body.String(), f.logs.String()} {
		if strings.Contains(out, "synthetic-private") || strings.Contains(out, "acct_123") {
			t.Errorf("upstream body leaked: %s", out)
		}
	}
	if !strings.Contains(f.logs.String(), "[usage] upstream 500 in ") {
		t.Errorf("log line missing: %s", f.logs.String())
	}
}

func TestUsage_rejectsNonGet(t *testing.T) {
	// Given: a working upstream.
	f := newUsageFixture(t, usageOK(usageSampleBody))
	// When: a non-GET method is used.
	recorder, _ := f.get(t, http.MethodPost)
	// Then: 405 without touching upstream.
	if recorder.Code != http.StatusMethodNotAllowed || f.hits.Load() != 0 {
		t.Fatalf("status=%d hits=%d", recorder.Code, f.hits.Load())
	}
}

func TestUsage_concurrentCallersShareOneUpstreamRequest(t *testing.T) {
	// Given: an upstream that holds the first request until released.
	arrived := make(chan struct{}, 8)
	release := make(chan struct{})
	f := newUsageFixture(t, func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
		usageOK(usageSampleBody)(w, r)
	})
	joined := make(chan struct{}, 8)
	f.svc.onJoin = func() { joined <- struct{}{} }
	const callers = 5
	codes := make([]int, callers)
	var wg sync.WaitGroup
	// When: several callers ask while the first upstream call is in flight.
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			f.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/usage", nil))
			codes[i] = recorder.Code
		}()
	}
	timeout := time.After(5 * time.Second)
	for waiting := 0; waiting < callers; {
		select {
		case <-arrived:
			waiting++
		case <-joined:
			waiting++
		case <-timeout:
			close(release)
			t.Fatal("callers did not converge on one in-flight request")
		}
	}
	close(release)
	wg.Wait()
	// Then: exactly one upstream request served every caller.
	if f.hits.Load() != 1 {
		t.Fatalf("upstream hits = %d", f.hits.Load())
	}
	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("caller %d status = %d", i, code)
		}
	}
}
