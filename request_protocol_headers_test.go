package main

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

var sessionUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestProtocolHeaders_matchesNativeProfile_whenOpusRequestHasNoSession(t *testing.T) {
	// Given: an OpenCode request with a placeholder key and foreign billing header.
	original, _ := http.NewRequest(http.MethodPost, "http://localhost/v1/messages", nil)
	original.Header.Set("x-api-key", "placeholder")
	original.Header.Set("x-anthropic-billing-header", "foreign")
	upstream, _ := http.NewRequest(http.MethodPost, UpstreamBase+"/v1/messages", nil)

	// When: the proxy replaces outbound headers.
	TransformRequestHeaders(upstream, original, "synthetic-access-token")

	// Then: native SDK-CLI identity and one valid session, with no HTTP billing.
	want := map[string]string{
		"Authorization": "Bearer synthetic-access-token",
		"User-Agent":    "claude-cli/2.1.282 (external, sdk-cli)",
		"x-app":         "cli",
		"anthropic-dangerous-direct-browser-access": "true",
		"anthropic-version":                         "2023-06-01",
	}
	for key, value := range want {
		if upstream.Header.Get(key) != value {
			t.Errorf("%s = %q, want %q", key, upstream.Header.Get(key), value)
		}
	}
	if upstream.Header.Get("x-api-key") != "" || upstream.Header.Get("x-anthropic-billing-header") != "" {
		t.Error("outgoing HTTP request still contains API key or billing header")
	}
	if !sessionUUID.MatchString(upstream.Header.Get("x-claude-code-session-id")) {
		t.Errorf("session is not a UUID v4: %q", upstream.Header.Get("x-claude-code-session-id"))
	}
	if strings.Join(strings.Split(upstream.Header.Get("anthropic-beta"), ","), ",") != strings.Join([]string{
		"claude-code-20250219", "oauth-2025-04-20", "interleaved-thinking-2025-05-14",
		"thinking-token-count-2026-05-13", "context-management-2025-06-27",
		"prompt-caching-scope-2026-01-05", "mid-conversation-system-2026-04-07",
		"per-turn-control-2026-07-01", "mid-conversation-tool-changes-2026-07-01",
		"effort-2025-11-24", "extended-cache-ttl-2025-04-11",
	}, ",") {
		t.Errorf("native beta ordering mismatch: %q", upstream.Header.Get("anthropic-beta"))
	}
}

func TestProtocolHeaders_preservesValidSession_whenCallerProvidesOne(t *testing.T) {
	// Given: a valid caller session and duplicated compatible beta.
	original, _ := http.NewRequest(http.MethodPost, "http://localhost/v1/messages", nil)
	original.Header.Set("x-claude-code-session-id", "12345678-1234-4234-8234-123456789abc")
	original.Header.Set("anthropic-beta", "oauth-2025-04-20,files-api-2025-04-14,context-1m,effort-2025-11-24")
	upstream, _ := http.NewRequest(http.MethodPost, UpstreamBase+"/v1/messages", nil)

	// When: the proxy merges request headers.
	TransformRequestHeaders(upstream, original, "synthetic-access-token")

	// Then: the session survives; compatible extra beta survives once.
	if upstream.Header.Get("x-claude-code-session-id") != original.Header.Get("x-claude-code-session-id") {
		t.Error("caller session changed")
	}
	betas := upstream.Header.Get("anthropic-beta")
	if strings.Count(betas, "oauth-2025-04-20") != 1 || strings.Count(betas, "effort-2025-11-24") != 1 || !strings.Contains(betas, "files-api-2025-04-14") || strings.Contains(betas, "context-1m") {
		t.Errorf("beta merge changed compatible values: %q", betas)
	}
}
