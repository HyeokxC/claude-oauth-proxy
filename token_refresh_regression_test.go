package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// isolatedTokenManager blocks the real Keychain and network before any token call.
func isolatedTokenManager(t *testing.T, transport roundTripFunc) (*TokenManager, string) {
	t.Helper()
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	fakeSecurity(t, "exit 1\n")
	manager := NewTokenManager()
	manager.httpClient.Transport = transport
	return manager, configDir
}

func syntheticTokenCredentials(label string, expiresAt time.Time) Credentials {
	return Credentials{
		AccessToken:  "synthetic-" + label + "-access",
		RefreshToken: "synthetic-" + label + "-refresh",
		ExpiresAt:    expiresAt.UnixMilli(),
	}
}

func writeCredentialFile(t *testing.T, configDir string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(configDir, ".credentials.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func writeSourceCredentials(t *testing.T, configDir string, creds Credentials) {
	t.Helper()
	data, err := json.Marshal(map[string]Credentials{"claudeAiOauth": creds})
	if err != nil {
		t.Fatal(err)
	}
	writeCredentialFile(t, configDir, data)
}

func refreshResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

const syntheticRefreshSuccess = `{"access_token":"synthetic-new-access","refresh_token":"synthetic-new-refresh","expires_in":3600}`

func TestGetValidToken_recoversFreshSourceCredentialsAfterRefresh400(t *testing.T) {
	// Given: an expired cached token whose source is rotated by another client while our refresh is rejected.
	stale := syntheticTokenCredentials("stale", time.Now().Add(-time.Hour))
	fresh := syntheticTokenCredentials("fresh", time.Now().Add(time.Hour))
	var configDir string
	manager, configDir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		writeSourceCredentials(t, configDir, fresh)
		return refreshResponse(req, http.StatusBadRequest, `{"error":"invalid_grant"}`), nil
	})
	writeSourceCredentials(t, configDir, stale)
	manager.creds = &stale

	// When: a caller asks for a valid token.
	token, err := manager.GetValidToken()

	// Then: the fresh source credentials are used instead of surfacing the stale refresh failure.
	if err != nil {
		t.Fatalf("expected recovery from updated source, got error: %v", err)
	}
	if token != fresh.AccessToken {
		t.Fatalf("access token: got %q, want %q", token, fresh.AccessToken)
	}
}

func TestGetValidToken_persistsRefreshedTokensPreservingCredentialFile(t *testing.T) {
	// Given: an expired nested credential file carrying fields the proxy does not model.
	manager, configDir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		return refreshResponse(req, http.StatusOK, syntheticRefreshSuccess), nil
	})
	expired := time.Now().Add(-time.Hour).UnixMilli()
	original := `{"claudeAiOauth":{"accessToken":"synthetic-old-access","refreshToken":"synthetic-old-refresh","expiresAt":` +
		jsonNumber(expired) + `,"scopes":["user:inference","user:profile"],"subscriptionType":"synthetic-plan"},` +
		`"mcpOAuth":{"synthetic-server":{"accessToken":"synthetic-mcp","nested":{"keep":true}}},"syntheticTopLevel":42}`
	writeCredentialFile(t, configDir, []byte(original))
	before := time.Now().UnixMilli()

	// When: the expired token is refreshed successfully.
	if _, err := manager.GetValidToken(); err != nil {
		t.Fatal(err)
	}

	// Then: the file holds the new tokens under the same nested key and keeps every unknown field.
	persisted := readJSONObject(t, filepath.Join(configDir, ".credentials.json"))
	want := readJSONString(t, original)
	if _, flattened := persisted["accessToken"]; flattened {
		t.Fatalf("credential file was flattened: %v", persisted)
	}
	for _, key := range []string{"mcpOAuth", "syntheticTopLevel"} {
		if !reflect.DeepEqual(persisted[key], want[key]) {
			t.Fatalf("top-level field %q: got %#v, want %#v", key, persisted[key], want[key])
		}
	}
	oauth, ok := persisted["claudeAiOauth"].(map[string]any)
	if !ok {
		t.Fatalf("claudeAiOauth missing or not an object: %#v", persisted["claudeAiOauth"])
	}
	wantOAuth := want["claudeAiOauth"].(map[string]any)
	for _, key := range []string{"scopes", "subscriptionType"} {
		if !reflect.DeepEqual(oauth[key], wantOAuth[key]) {
			t.Fatalf("nested field %q: got %#v, want %#v", key, oauth[key], wantOAuth[key])
		}
	}
	if oauth["accessToken"] != "synthetic-new-access" || oauth["refreshToken"] != "synthetic-new-refresh" {
		t.Fatalf("persisted tokens not updated: access=%v refresh=%v", oauth["accessToken"], oauth["refreshToken"])
	}
	if expiresAt, isNumber := oauth["expiresAt"].(float64); !isNumber || int64(expiresAt) <= before {
		t.Fatalf("persisted expiresAt not advanced past %d: %#v", before, oauth["expiresAt"])
	}
}

func TestForceRefresh_refreshesUnexpiredCachedToken(t *testing.T) {
	// Given: an unexpired cached token that upstream has rejected.
	var calls atomic.Int32
	var sentRefreshToken atomic.Value
	manager, configDir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if err := req.ParseForm(); err != nil {
			return nil, err
		}
		sentRefreshToken.Store(req.PostForm.Get("refresh_token"))
		return refreshResponse(req, http.StatusOK, syntheticRefreshSuccess), nil
	})
	cached := syntheticTokenCredentials("cached", time.Now().Add(time.Hour))
	writeSourceCredentials(t, configDir, cached)
	manager.creds = &cached

	// When: a forced refresh is requested.
	if err := manager.ForceRefresh(); err != nil {
		t.Fatal(err)
	}

	// Then: one refresh request is sent with the cached refresh token and its new access token is served.
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh requests: got %d, want 1", got)
	}
	if got := sentRefreshToken.Load(); got != cached.RefreshToken {
		t.Fatalf("refresh_token sent: got %v, want %q", got, cached.RefreshToken)
	}
	token, err := manager.GetValidToken()
	if err != nil {
		t.Fatal(err)
	}
	if token != "synthetic-new-access" {
		t.Fatalf("access token after forced refresh: got %q", token)
	}
}

func TestGetValidToken_doesNotRepeatFailedRefreshForUnchangedCredentials(t *testing.T) {
	// Given: expired credentials, unchanged in the source, whose refresh is rejected.
	var calls atomic.Int32
	manager, configDir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return refreshResponse(req, http.StatusBadRequest, `{"error":"invalid_grant"}`), nil
	})
	stale := syntheticTokenCredentials("stale", time.Now().Add(-time.Hour))
	writeSourceCredentials(t, configDir, stale)
	manager.creds = &stale

	// When: two callers immediately ask for a token in sequence.
	_, firstErr := manager.GetValidToken()
	_, secondErr := manager.GetValidToken()

	// Then: both fail, but only the first reaches the refresh endpoint.
	if firstErr == nil || secondErr == nil {
		t.Fatalf("expected both calls to fail: first=%v second=%v", firstErr, secondErr)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh requests for unchanged credentials: got %d, want 1", got)
	}
}

func jsonNumber(value int64) string {
	return strconv.FormatInt(value, 10)
}

func readJSONString(t *testing.T, raw string) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func readJSONObject(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return readJSONString(t, string(data))
}
