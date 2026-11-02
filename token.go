package main

import (
	"encoding/json"
	"errors"
	"net/http"
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

const refreshFailureCooldown = 30 * time.Second

var ErrCredentialPersistence = errors.New("refreshed credentials could not be persisted")

type refreshFailure struct {
	revision Credentials
	until    time.Time
	err      error
}

type pendingCredentials struct {
	base *credentialRecord
	next Credentials
}

type TokenManager struct {
	// All lifecycle transitions, including I/O, are serialized by mu.
	mu         sync.Mutex
	creds      *Credentials
	httpClient *http.Client
	store      credentialStore
	now        func() time.Time
	source     *credentialRecord
	pending    *pendingCredentials
	failures   [2]refreshFailure
	rejected   string
}

func NewTokenManager() *TokenManager {
	return &TokenManager{
		httpClient: &http.Client{Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		store: &sourceCredentialStore{nativeWrite: nativeKeychainUpdate},
		now:   time.Now,
	}
}

func (tm *TokenManager) GetValidToken() (string, error) { return tm.token(false, "") }

// ForceRefresh also refreshes an unexpired token. Use the token-aware method
// when handling 401s so concurrent rejections of one token share a rotation.
func (tm *TokenManager) ForceRefresh() error {
	_, err := tm.token(true, "")
	return err
}

func (tm *TokenManager) ForceRefreshRejected(rejected string) error {
	_, err := tm.token(false, rejected)
	return err
}

func (tm *TokenManager) adopt(record *credentialRecord) {
	tm.source = record
	creds := record.auth
	tm.creds = &creds
}

func (tm *TokenManager) needsRefresh() bool {
	return tm.creds.AccessToken == tm.rejected || tm.now().UnixMilli() >= tm.creds.ExpiresAt-5*60*1000
}

func (tm *TokenManager) rememberFailure(revision Credentials, err error) {
	tm.failures[1] = tm.failures[0]
	tm.failures[0] = refreshFailure{revision, tm.now().Add(refreshFailureCooldown), err}
}

func (tm *TokenManager) token(force bool, rejected string) (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	pendingBefore := tm.pending
	cachedAccess := ""
	if tm.creds != nil {
		cachedAccess = tm.creds.AccessToken
		// Remember a rejection even if locking or reading the source fails.
		// A late 401 for an older token must not clear a newer rejection.
		if rejected == cachedAccess || force && pendingBefore == nil {
			tm.rejected = cachedAccess
		}
	}
	unlock, err := tm.store.Lock()
	if err != nil {
		if pendingBefore != nil {
			return "", ErrCredentialPersistence
		}
		return "", errors.New("credential store unavailable")
	}
	defer unlock()
	if err := tm.reload(); err != nil {
		return "", err
	}
	if rejected != "" && tm.creds.AccessToken == rejected || force && pendingBefore == nil && cachedAccess == "" {
		tm.rejected = tm.creds.AccessToken
	}
	if pendingBefore != nil && *tm.creds == pendingBefore.next && tm.creds.AccessToken != tm.rejected && tm.now().UnixMilli() < tm.creds.ExpiresAt {
		return tm.creds.AccessToken, nil
	}
	// At most two refresh attempts, and never twice for the same auth revision.
	var attempted [2]Credentials
	for attempt := 0; attempt < len(attempted); attempt++ {
		if !tm.needsRefresh() {
			return tm.creds.AccessToken, nil
		}
		revision := *tm.creds
		if attempt > 0 && revision == attempted[0] {
			break
		}
		for _, failure := range tm.failures {
			if failure.revision == revision && tm.now().Before(failure.until) {
				return "", failure.err
			}
		}
		attempted[attempt] = revision
		next, refreshErr := tm.requestRefresh(revision)
		if refreshErr == nil {
			if next.AccessToken == tm.rejected {
				tm.rememberFailure(next, errors.New("refreshed token was already rejected"))
			}
			tm.pending = &pendingCredentials{base: tm.source, next: next}
			if err := tm.savePending(); err != nil {
				return "", err
			}
			if tm.creds.AccessToken == tm.rejected {
				return "", errors.New("refreshed token was already rejected")
			}
			if *tm.creds == next || !tm.needsRefresh() {
				return tm.creds.AccessToken, nil
			}
			continue
		}
		tm.rememberFailure(revision, refreshErr)
		if err := tm.reload(); err != nil || *tm.creds == revision {
			return "", refreshErr
		}
		if !tm.needsRefresh() {
			return tm.creds.AccessToken, nil
		}
	}
	return "", errors.New("credential recovery exhausted")
}

func (tm *TokenManager) reload() error {
	record, err := tm.store.Load(tm.source)
	if tm.pending != nil {
		if err == nil && record.auth != tm.pending.base.auth {
			if record.auth == tm.pending.next && sameCredentialSource(record, tm.pending.base) {
				return tm.savePending() // finish a prior rename's directory sync
			}
			// A new login (or a completed write) supersedes pending rotation.
			tm.pending = nil
			tm.adopt(record)
			return nil
		}
		return tm.savePending()
	}
	if err != nil {
		return errors.New("credential source unavailable")
	}
	tm.adopt(record)
	return nil
}

func (tm *TokenManager) savePending() error {
	pending := tm.pending
	// Keep the rotated credentials in memory even when the write fails.
	next := pending.next
	tm.creds = &next
	record, err := tm.store.Save(pending.base, next)
	if errors.Is(err, errSourceChanged) && record != nil && record.auth != pending.base.auth {
		tm.pending = nil
		tm.adopt(record)
		return nil
	}
	if err != nil {
		return ErrCredentialPersistence
	}
	tm.pending = nil
	tm.adopt(record)
	return nil
}
