package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Only synthetic credentials are read; the real security executable is never run.
func credentialFixture(t *testing.T, creds Credentials) (*TokenManager, func(Credentials)) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	fakeSecurity(t, "cat \"$TOKEN_FIXTURE\"\n")
	path := filepath.Join(t.TempDir(), "credentials.json")
	t.Setenv("TOKEN_FIXTURE", path)
	write := func(value Credentials) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(creds)
	manager := NewTokenManager()
	if err := manager.loadCredentials(); err != nil {
		t.Fatal(err)
	}
	manager.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return tokenResponse(400, `{"error":"invalid_grant"}`), nil
	})
	return manager, write
}

func tokenResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestGetValidToken_recoversAfterCLILogin(t *testing.T) {
	// Given: expired cached credentials, followed by a new CLI login.
	manager, write := credentialFixture(t, Credentials{"old-access", "old-refresh", 1})
	write(Credentials{"new-access", "new-refresh", time.Now().Add(time.Hour).UnixMilli()})
	calls := 0
	manager.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return tokenResponse(400, `{"error":"invalid_grant"}`), nil
	})
	// When: the next request asks for a valid token.
	token, err := manager.GetValidToken()
	// Then: new credentials are used before submitting the obsolete refresh token.
	if err != nil || token != "new-access" || calls != 0 {
		t.Fatalf("did not recover from source: err=%v, tokenMatches=%v, refreshes=%d", err, token == "new-access", calls)
	}
}

func TestForceRefresh_refreshesBeforeExpiry(t *testing.T) {
	// Given: upstream rejected an access token which has not expired locally.
	manager, _ := credentialFixture(t, Credentials{"rejected-access", "refresh", time.Now().Add(time.Hour).UnixMilli()})
	calls := 0
	manager.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return tokenResponse(200, `{"access_token":"renewed-access","refresh_token":"renewed-refresh","expires_in":3600}`), nil
	})
	// When: a 401 triggers forced recovery.
	err := manager.ForceRefresh()
	// Then: expiry must not suppress the refresh.
	if err != nil || calls != 1 || manager.creds.AccessToken != "renewed-access" {
		t.Fatalf("forced recovery failed: err=%v, refreshes=%d", err, calls)
	}
}

func TestRefresh_recoversWhenCLILoginRacesFailedRefresh(t *testing.T) {
	// Given: CLI replaces the credential while the old refresh request is in flight.
	manager, write := credentialFixture(t, Credentials{"old-access", "old-refresh", 1})
	manager.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		write(Credentials{"new-access", "new-refresh", time.Now().Add(time.Hour).UnixMilli()})
		return tokenResponse(400, `{"error":"invalid_grant"}`), nil
	})
	// When: refresh fails.
	token, err := manager.GetValidToken()
	// Then: a single source reload recovers without restarting.
	if err != nil || token != "new-access" {
		t.Fatalf("recovery failed: %v", err)
	}
}

func TestRefresh_doesNotRestoreUnchangedSourceAfterRotation(t *testing.T) {
	// Given: the proxy rotated credentials, but the CLI-owned source is unchanged.
	manager, _ := credentialFixture(t, Credentials{"initial-access", "initial-refresh", 1})
	calls := 0
	manager.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		want := "initial-refresh"
		if calls > 1 {
			want = "rotated-refresh"
		}
		if r.Form.Get("refresh_token") != want {
			t.Error("reused obsolete refresh token")
		}
		return tokenResponse(200, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`), nil
	})
	if _, err := manager.GetValidToken(); err != nil {
		t.Fatal(err)
	}
	// When: another real unauthorized response requires recovery.
	err := manager.ForceRefresh()
	// Then: memory rotation is retained rather than replaced by the old source.
	if err != nil || calls != 2 {
		t.Fatalf("refreshes=%d err=%v", calls, err)
	}
}

func TestRefresh_rejectsMalformedSuccess(t *testing.T) {
	for _, body := range []string{`{"expires_in":3600}`, `{"access_token":"new","expires_in":0}`, `{"access_token":"new","expires_in":-1}`} {
		t.Run(body, func(t *testing.T) {
			// Given: a token endpoint returned an unusable success payload.
			manager, _ := credentialFixture(t, Credentials{"old", "refresh", 1})
			manager.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) { return tokenResponse(200, body), nil })
			// When: refreshing credentials.
			_, err := manager.GetValidToken()
			// Then: the invalid credential must not replace the existing snapshot.
			if err == nil || manager.creds.AccessToken != "old" {
				t.Fatal("accepted malformed token response")
			}
		})
	}
}
