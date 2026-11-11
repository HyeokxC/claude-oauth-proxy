package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Synthetic Keychain: find-generic-password prints the fixture, `security -i`
// decodes the hex payload of add-generic-password into it.
func writableKeychainFixture(t *testing.T, stored string) (*TokenManager, string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	fakeSecurity(t, `if [ "$1" = "-i" ]; then
  read -r line
  printf '%s' "${line##* -X }" | xxd -r -p > "$TOKEN_FIXTURE"
else
  cat "$TOKEN_FIXTURE"
fi
`)
	path := filepath.Join(t.TempDir(), "credentials.json")
	t.Setenv("TOKEN_FIXTURE", path)
	if err := os.WriteFile(path, []byte(stored), 0600); err != nil {
		t.Fatal(err)
	}
	manager := NewTokenManager()
	if err := manager.loadCredentials(); err != nil {
		t.Fatal(err)
	}
	manager.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return tokenResponse(200, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`), nil
	})
	return manager, path
}

func TestRefresh_writesRotatedTokenBackPreservingOtherFields(t *testing.T) {
	// Given: an expired Keychain credential carrying CLI-only metadata.
	manager, path := writableKeychainFixture(t, `{"claudeAiOauth":{"accessToken":"old-access","refreshToken":"old-refresh","expiresAt":1,"scopes":["user:inference"],"subscriptionType":"max"},"mcpOAuth":{"k":"v"}}`)

	// When: the proxy refreshes.
	if _, err := manager.GetValidToken(); err != nil {
		t.Fatal(err)
	}

	// Then: the rotated tokens are stored and unrelated fields survive.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		ClaudeAiOauth struct {
			AccessToken      string   `json:"accessToken"`
			RefreshToken     string   `json:"refreshToken"`
			ExpiresAt        int64    `json:"expiresAt"`
			Scopes           []string `json:"scopes"`
			SubscriptionType string   `json:"subscriptionType"`
		} `json:"claudeAiOauth"`
		McpOAuth map[string]string `json:"mcpOAuth"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	got := stored.ClaudeAiOauth
	if got.AccessToken != "rotated-access" || got.RefreshToken != "rotated-refresh" || got.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatal("rotated credentials were not written back")
	}
	if len(got.Scopes) != 1 || got.SubscriptionType != "max" || stored.McpOAuth["k"] != "v" {
		t.Fatal("write-back dropped unrelated fields")
	}
	if manager.sourceCreds != *manager.creds {
		t.Fatal("source snapshot not advanced after write-back")
	}
}

func TestRefresh_doesNotOverwriteNewerCLILogin(t *testing.T) {
	// Given: the CLI logs in again while the proxy's refresh request is in flight.
	manager, path := writableKeychainFixture(t, `{"claudeAiOauth":{"accessToken":"old-access","refreshToken":"old-refresh","expiresAt":1}}`)
	cliLogin := `{"claudeAiOauth":{"accessToken":"cli-access","refreshToken":"cli-refresh","expiresAt":1}}`
	manager.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := os.WriteFile(path, []byte(cliLogin), 0600); err != nil {
			t.Fatal(err)
		}
		return tokenResponse(200, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`), nil
	})

	// When: the proxy finishes its refresh.
	if _, err := manager.GetValidToken(); err != nil {
		t.Fatal(err)
	}

	// Then: the CLI's credential is left untouched.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != cliLogin {
		t.Fatal("overwrote a newer CLI credential")
	}
}

func TestRefresh_writesRotatedTokenBackToFileSource(t *testing.T) {
	// Given: Keychain is unavailable and the file source holds an expired credential.
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	fakeSecurity(t, "exit 44\n")
	path := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(path, []byte(`{"claudeAiOauth":{"accessToken":"old-access","refreshToken":"old-refresh","expiresAt":1}}`), 0600); err != nil {
		t.Fatal(err)
	}
	manager := NewTokenManager()
	manager.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return tokenResponse(200, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`), nil
	})

	// When: the proxy refreshes.
	if _, err := manager.GetValidToken(); err != nil {
		t.Fatal(err)
	}

	// Then: the file holds the rotated refresh token with owner-only permissions.
	stored, err := manager.readFromFile()
	if err != nil || stored.RefreshToken != "rotated-refresh" {
		t.Fatalf("file source not updated: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("credentials file permissions: %v %v", info.Mode().Perm(), err)
	}
}
