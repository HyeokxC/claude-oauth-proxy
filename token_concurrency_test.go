package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUnauthorizedRecovery_deduplicatesConcurrentRejectedToken(t *testing.T) {
	// Given: many requests have received 401 for the same unexpired access token.
	manager, _ := credentialFixture(t, Credentials{"rejected", "refresh", time.Now().Add(time.Hour).UnixMilli()})
	var exchanges atomic.Int32
	manager.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		exchanges.Add(1)
		return tokenResponse(200, `{"access_token":"renewed","expires_in":3600}`), nil
	})
	start := make(chan struct{})
	errors := make(chan error, 20)
	var workers sync.WaitGroup
	// When: all recoveries run concurrently and name the observed rejected token.
	for range 20 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			errors <- manager.RefreshAfterUnauthorized("rejected")
		}()
	}
	close(start)
	workers.Wait()
	close(errors)
	// Then: only one exchange occurs, and every caller shares its result.
	for err := range errors {
		if err != nil {
			t.Error(err)
		}
	}
	if exchanges.Load() != 1 {
		t.Fatalf("refresh exchanges=%d", exchanges.Load())
	}
}

func TestRecovery_keepsOriginalSourceWhenKeychainUnavailable(t *testing.T) {
	// Given: Keychain was selected; a different fallback-file identity also exists.
	manager, _ := credentialFixture(t, Credentials{"old", "refresh", 1})
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if err := os.WriteFile(filepath.Join(configDir, ".credentials.json"), []byte(syntheticCredentials), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOKEN_FIXTURE", filepath.Join(t.TempDir(), "missing"))
	// When: Keychain becomes unavailable during recovery.
	_, err := manager.GetValidToken()
	// Then: it fails instead of silently adopting fallback credentials.
	if err == nil || manager.creds.AccessToken != "old" {
		t.Fatal("silently switched credential source")
	}
}

func TestHealth_recoversAfterCLILoginWithoutRestart(t *testing.T) {
	// Given: the same live handler survives a failed refresh, then a CLI login.
	manager, write := credentialFixture(t, Credentials{"old", "refresh", 1})
	server := httptest.NewServer(NewProxyHandler(manager))
	t.Cleanup(server.Close)
	before, err := server.Client().Get(server.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	before.Body.Close()
	if before.StatusCode != 503 {
		t.Fatalf("initial health=%d", before.StatusCode)
	}
	write(Credentials{"new", "new-refresh", time.Now().Add(time.Hour).UnixMilli()})
	// When: health is requested again without recreating the manager or server.
	after, err := server.Client().Get(server.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer after.Body.Close()
	// Then: the real HTTP health surface recovers to 200.
	if after.StatusCode != 200 {
		t.Fatalf("health after login=%d", after.StatusCode)
	}
}

func TestRefresh_doesNotExposeErrorResponse(t *testing.T) {
	// Given: an OAuth server sends a body containing synthetic sensitive text.
	manager, _ := credentialFixture(t, Credentials{"old", "refresh", 1})
	manager.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return tokenResponse(400, `{"error_description":"synthetic-secret"}`), nil
	})
	// When: refresh fails.
	_, err := manager.GetValidToken()
	// Then: errors expose the status, not the response body.
	if err == nil || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatal("unsafe refresh error")
	}
}
