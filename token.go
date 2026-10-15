package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type Credentials struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"` // milliseconds since epoch
}

func (c *Credentials) UnmarshalJSON(data []byte) error {
	var wire struct {
		AccessToken  string  `json:"accessToken"`
		RefreshToken string  `json:"refreshToken"`
		ExpiresAt    float64 `json:"expiresAt"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	c.AccessToken = wire.AccessToken
	c.RefreshToken = wire.RefreshToken
	c.ExpiresAt = int64(wire.ExpiresAt)
	return nil
}

type TokenManager struct {
	mu         sync.RWMutex
	creds      *Credentials
	refreshing sync.Mutex
	httpClient *http.Client
	// Protected by refreshing; the CLI-owned source is read, never overwritten.
	sourceReader func() (*Credentials, error)
	sourceCreds  Credentials
}

func NewTokenManager() *TokenManager {
	return &TokenManager{
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// GetValidToken returns a valid access token, refreshing if necessary.
func (tm *TokenManager) GetValidToken() (string, error) {
	tm.mu.RLock()
	creds := tm.creds
	tm.mu.RUnlock()

	if creds == nil {
		tm.refreshing.Lock()
		tm.mu.RLock()
		loaded := tm.creds != nil
		tm.mu.RUnlock()
		var err error
		if !loaded {
			err = tm.loadCredentials()
		}
		tm.refreshing.Unlock()
		if err != nil {
			return "", fmt.Errorf("load credentials: %w", err)
		}
		tm.mu.RLock()
		creds = tm.creds
		tm.mu.RUnlock()
	}

	// Refresh if within 5 minutes of expiry
	if time.Now().UnixMilli() >= creds.ExpiresAt-5*60*1000 {
		if err := tm.refresh(""); err != nil {
			return "", fmt.Errorf("refresh token: %w", err)
		}
		tm.mu.RLock()
		creds = tm.creds
		tm.mu.RUnlock()
	}

	return creds.AccessToken, nil
}

// ForceRefresh forces a token refresh (e.g., after a 401).
func (tm *TokenManager) ForceRefresh() error {
	tm.mu.RLock()
	rejected := ""
	if tm.creds != nil {
		rejected = tm.creds.AccessToken
	}
	tm.mu.RUnlock()
	return tm.RefreshAfterUnauthorized(rejected)
}

// RefreshAfterUnauthorized deduplicates concurrent 401s for the same access token.
func (tm *TokenManager) RefreshAfterUnauthorized(rejected string) error {
	return tm.refresh(rejected)
}

func (tm *TokenManager) loadCredentials() error {
	// Try macOS Keychain first
	creds, err := tm.readFromKeychain()
	if err == nil && creds != nil {
		tm.sourceReader = tm.readFromKeychain
		tm.sourceCreds = *creds
		tm.mu.Lock()
		tm.creds = creds
		tm.mu.Unlock()
		log.Printf("[token] loaded from keychain (expires: %s)", time.UnixMilli(creds.ExpiresAt).Format(time.RFC3339))
		return nil
	}
	log.Printf("[token] keychain read failed: %v, trying file fallback", err)

	// Fallback to file
	creds, err = tm.readFromFile()
	if err != nil {
		return fmt.Errorf("no credentials found: keychain and file both failed: %w", err)
	}

	tm.sourceReader = tm.readFromFile
	tm.sourceCreds = *creds
	tm.mu.Lock()
	tm.creds = creds
	tm.mu.Unlock()
	log.Printf("[token] loaded from file (expires: %s)", time.UnixMilli(creds.ExpiresAt).Format(time.RFC3339))
	return nil
}

// parseCredentials handles both nested {"claudeAiOauth": {...}} and flat formats.
func parseCredentials(data []byte) (*Credentials, error) {
	// Try nested format first
	var nested struct {
		ClaudeAiOauth Credentials `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &nested); err == nil && nested.ClaudeAiOauth.AccessToken != "" {
		return &nested.ClaudeAiOauth, nil
	}

	// Try flat format
	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}
	if creds.AccessToken == "" {
		return nil, fmt.Errorf("no access token in credentials")
	}
	return &creds, nil
}

func (tm *TokenManager) refresh(rejected string) error {
	// Prevent thundering herd: only one goroutine refreshes at a time
	tm.refreshing.Lock()
	defer tm.refreshing.Unlock()

	// Double-check: another goroutine may have already refreshed
	tm.mu.RLock()
	if tm.creds != nil && time.Now().UnixMilli() < tm.creds.ExpiresAt-5*60*1000 &&
		(rejected == "" || tm.creds.AccessToken != rejected) {
		tm.mu.RUnlock()
		return nil
	}
	tm.mu.RUnlock()
	if tm.reloadChangedCredentials(rejected) {
		return nil
	}
	tm.mu.RLock()
	refreshToken := ""
	if tm.creds != nil {
		refreshToken = tm.creds.RefreshToken
	}
	tm.mu.RUnlock()

	if refreshToken == "" {
		return fmt.Errorf("no refresh token available; run claude auth login --claudeai")
	}

	log.Printf("[token] refreshing OAuth token...")

	formData := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {OAuthClientID},
	}

	resp, err := tm.httpClient.PostForm(TokenRefreshURL, formData)
	if err != nil {
		return fmt.Errorf("refresh request failed")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if tm.reloadChangedCredentials(rejected) {
			return nil
		}
		return fmt.Errorf("refresh failed with status %d", resp.StatusCode)
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"` // seconds
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("invalid refresh response")
	}
	if result.AccessToken == "" || result.ExpiresIn <= 0 || result.ExpiresIn > 365*24*60*60 {
		return fmt.Errorf("invalid refresh credential or expiry")
	}
	if tm.reloadChangedCredentials(rejected) {
		return nil
	}

	newCreds := &Credentials{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresAt:    time.Now().UnixMilli() + result.ExpiresIn*1000,
	}
	if newCreds.RefreshToken == "" {
		newCreds.RefreshToken = refreshToken // keep old if not returned
	}

	tm.mu.Lock()
	tm.creds = newCreds
	tm.mu.Unlock()

	log.Printf("[token] refreshed successfully (expires: %s)", time.UnixMilli(newCreds.ExpiresAt).Format(time.RFC3339))
	return nil
}
