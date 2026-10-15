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
		if err := tm.loadCredentials(); err != nil {
			return "", fmt.Errorf("load credentials: %w", err)
		}
		tm.mu.RLock()
		creds = tm.creds
		tm.mu.RUnlock()
	}

	// Refresh if within 5 minutes of expiry
	if time.Now().UnixMilli() >= creds.ExpiresAt-5*60*1000 {
		if err := tm.refresh(); err != nil {
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
	return tm.refresh()
}

func (tm *TokenManager) loadCredentials() error {
	// Try macOS Keychain first
	creds, err := tm.readFromKeychain()
	if err == nil && creds != nil {
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

func (tm *TokenManager) refresh() error {
	// Prevent thundering herd: only one goroutine refreshes at a time
	tm.refreshing.Lock()
	defer tm.refreshing.Unlock()

	// Double-check: another goroutine may have already refreshed
	tm.mu.RLock()
	if tm.creds != nil && time.Now().UnixMilli() < tm.creds.ExpiresAt-5*60*1000 {
		tm.mu.RUnlock()
		return nil
	}
	refreshToken := ""
	if tm.creds != nil {
		refreshToken = tm.creds.RefreshToken
	}
	tm.mu.RUnlock()

	if refreshToken == "" {
		// Reload from source to get refresh token
		if err := tm.loadCredentials(); err != nil {
			return err
		}
		tm.mu.RLock()
		refreshToken = tm.creds.RefreshToken
		tm.mu.RUnlock()
	}

	log.Printf("[token] refreshing OAuth token...")

	formData := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {OAuthClientID},
	}

	resp, err := tm.httpClient.PostForm(TokenRefreshURL, formData)
	if err != nil {
		return fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("refresh failed with status %d", resp.StatusCode)
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"` // seconds
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("parse refresh response: %w", err)
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
