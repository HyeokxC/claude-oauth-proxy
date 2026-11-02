package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestOAuthResponse_sanitizesErrorsAndMalformedSuccess(t *testing.T) {
	const secret = "synthetic-private-response"
	tests := []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{"standard_code", 400, `{"error":"invalid_grant","error_description":"` + secret + `"}`, "invalid_grant"},
		{"arbitrary_code", 400, `{"error":"` + secret + `"}`, ""},
		{"nested_error", 401, `{"error":{"message":"` + secret + `"}}`, ""},
		{"malformed_error", 502, secret, ""},
		{"malformed_success", 200, `{"access_token":"` + secret + `","expires_in":"` + secret + `"}`, ""},
		{"missing_access", 200, `{"expires_in":3600}`, ""},
		{"zero_expiry", 200, `{"access_token":"` + secret + `","expires_in":0}`, ""},
		{"overflow_expiry", 200, `{"access_token":"` + secret + `","expires_in":9223372036854775807}`, ""},
		{"empty_refresh", 200, `{"access_token":"` + secret + `","refresh_token":"","expires_in":3600}`, ""},
		{"trailing_json", 200, syntheticRefreshSuccess + `{"private":"` + secret + `"}`, ""},
		{"oversized_success", 200, strings.Repeat(secret, maxOAuthResponse), ""},
		{"oversized_error", 400, strings.Repeat(secret, maxOAuthResponse), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) { return refreshResponse(req, tt.status, tt.body), nil })
			writeSourceCredentials(t, dir, syntheticTokenCredentials("stale", time.Now().Add(-time.Hour)))
			token, err := manager.GetValidToken()
			if err == nil || token != "" || strings.Contains(err.Error(), secret) {
				t.Fatalf("unsafe OAuth error: %v", err)
			}
			if tt.status != 200 {
				var failure *oauthFailure
				if !errors.As(err, &failure) || failure.status != tt.status || failure.code != tt.code {
					t.Fatalf("wrong sanitized failure: %v", err)
				}
			}
		})
	}
}

func TestOAuthResponse_transportErrorIsSanitized(t *testing.T) {
	manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) { return nil, errors.New("synthetic-private-transport") })
	writeSourceCredentials(t, dir, syntheticTokenCredentials("stale", time.Now().Add(-time.Hour)))
	if _, err := manager.GetValidToken(); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe transport error: %v", err)
	}
}

func TestOAuthCodes_allowlist(t *testing.T) {
	for _, code := range []string{"invalid_grant", "invalid_client", "invalid_request", "unauthorized_client", "unsupported_grant_type", "invalid_scope", "temporarily_unavailable", "server_error"} {
		if standardOAuthCode(code) != code {
			t.Fatalf("standard code removed: %s", code)
		}
	}
	if standardOAuthCode("synthetic-private") != "" {
		t.Fatal("arbitrary code allowed")
	}
}
