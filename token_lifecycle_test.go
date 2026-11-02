package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type failingSaveStore struct {
	credentialStore
	fail  bool
	saves int
}

func (s *failingSaveStore) Save(base *credentialRecord, next Credentials) (*credentialRecord, error) {
	s.saves++
	if s.fail {
		return nil, errors.New("synthetic-private-write-error")
	}
	return s.credentialStore.Save(base, next)
}

func TestTokenCooldown_clockAndChangedSourceBypass(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	calls := 0
	manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		calls++
		return refreshResponse(req, 400, `{"error":"invalid_grant"}`), nil
	})
	manager.now = func() time.Time { return now }
	stale := syntheticTokenCredentials("stale", now.Add(-time.Hour))
	writeSourceCredentials(t, dir, stale)
	for i := 0; i < 2; i++ {
		if _, err := manager.GetValidToken(); err == nil {
			t.Fatal("expected refresh rejection")
		}
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
	now = now.Add(refreshFailureCooldown)
	if _, err := manager.GetValidToken(); err == nil || calls != 2 {
		t.Fatalf("cooldown did not expire: calls=%d err=%v", calls, err)
	}
	fresh := syntheticTokenCredentials("login", now.Add(time.Hour))
	writeSourceCredentials(t, dir, fresh)
	if token, err := manager.GetValidToken(); err != nil || token != fresh.AccessToken || calls != 2 {
		t.Fatalf("changed source did not bypass cooldown: calls=%d err=%v", calls, err)
	}
}

func TestTokenPendingSave_retriesWithoutRefreshOrRollback(t *testing.T) {
	calls := 0
	manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		calls++
		return refreshResponse(req, 200, syntheticRefreshSuccess), nil
	})
	stale := syntheticTokenCredentials("stale", time.Now().Add(-time.Hour))
	writeSourceCredentials(t, dir, stale)
	store := &failingSaveStore{credentialStore: manager.store, fail: true}
	manager.store = store
	for i := 0; i < 2; i++ {
		token, err := manager.GetValidToken()
		if !errors.Is(err, ErrCredentialPersistence) || token != "" || strings.Contains(err.Error(), "private") {
			t.Fatalf("expected sanitized persistence error: %v", err)
		}
		if manager.creds.AccessToken != "synthetic-new-access" {
			t.Fatal("rotated token was lost")
		}
	}
	if calls != 1 || store.saves != 2 {
		t.Fatalf("refresh=%d saves=%d", calls, store.saves)
	}
	onDisk, err := manager.store.Load(manager.source)
	if err != nil || onDisk.auth != stale {
		t.Fatalf("failed save changed source: %v", err)
	}
	store.fail = false
	if err := manager.ForceRefreshRejected(stale.AccessToken); err != nil {
		t.Fatal(err)
	}
	token, err := manager.GetValidToken()
	if err != nil || token != "synthetic-new-access" || calls != 1 {
		t.Fatalf("pending recovery: calls=%d err=%v", calls, err)
	}
}

func TestTokenPendingSave_newLoginSupersedesRotation(t *testing.T) {
	manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		return refreshResponse(req, 200, syntheticRefreshSuccess), nil
	})
	writeSourceCredentials(t, dir, syntheticTokenCredentials("stale", time.Now().Add(-time.Hour)))
	store := &failingSaveStore{credentialStore: manager.store, fail: true}
	manager.store = store
	if _, err := manager.GetValidToken(); !errors.Is(err, ErrCredentialPersistence) {
		t.Fatal(err)
	}
	fresh := syntheticTokenCredentials("login", time.Now().Add(time.Hour))
	writeSourceCredentials(t, dir, fresh)
	if token, err := manager.GetValidToken(); err != nil || token != fresh.AccessToken || store.saves != 1 || manager.pending != nil {
		t.Fatalf("login not adopted: saves=%d err=%v", store.saves, err)
	}
}

func TestTokenConcurrentCallers_shareRefresh(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired", true: "concurrent_401"}[forced], func(t *testing.T) {
			var calls atomic.Int32
			manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				return refreshResponse(req, 200, syntheticRefreshSuccess), nil
			})
			expiry := time.Now().Add(-time.Hour)
			if forced {
				expiry = time.Now().Add(time.Hour)
			}
			stale := syntheticTokenCredentials("stale", expiry)
			writeSourceCredentials(t, dir, stale)
			var wg sync.WaitGroup
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if forced {
						if err := manager.ForceRefreshRejected(stale.AccessToken); err != nil {
							t.Error(err)
						}
					}
					if token, err := manager.GetValidToken(); err != nil || token != "synthetic-new-access" {
						t.Errorf("invalid concurrent result: %v", err)
					}
				}()
			}
			wg.Wait()
			if calls.Load() != 1 {
				t.Fatalf("refresh requests=%d", calls.Load())
			}
		})
	}
}

func TestForcedRefresh_doesNotServeRejectedUnexpiredToken(t *testing.T) {
	calls := 0
	manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		calls++
		return refreshResponse(req, 400, `{"error":"invalid_grant"}`), nil
	})
	stale := syntheticTokenCredentials("rejected", time.Now().Add(time.Hour))
	writeSourceCredentials(t, dir, stale)
	if err := manager.ForceRefreshRejected(stale.AccessToken); err == nil {
		t.Fatal("expected forced failure")
	}
	if token, err := manager.GetValidToken(); err == nil || token != "" || calls != 1 {
		t.Fatalf("rejected token served: calls=%d err=%v", calls, err)
	}
}

func TestRefreshRecovery_maximumTwoDistinctAttempts(t *testing.T) {
	var dir string
	calls := 0
	manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		calls++
		label := "second"
		if calls == 2 {
			label = "third"
		}
		writeSourceCredentials(t, dir, syntheticTokenCredentials(label, time.Now().Add(-time.Hour)))
		return refreshResponse(req, 400, `{"error":"invalid_grant"}`), nil
	})
	writeSourceCredentials(t, dir, syntheticTokenCredentials("first", time.Now().Add(-time.Hour)))
	if _, err := manager.GetValidToken(); err == nil || calls != 2 {
		t.Fatalf("unbounded recovery: calls=%d err=%v", calls, err)
	}
}

func TestRefresh_doesNotResurrectDeletedFile(t *testing.T) {
	var dir string
	manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		if err := os.Remove(filepath.Join(dir, ".credentials.json")); err != nil {
			t.Fatal(err)
		}
		return refreshResponse(req, 200, syntheticRefreshSuccess), nil
	})
	writeSourceCredentials(t, dir, syntheticTokenCredentials("stale", time.Now().Add(-time.Hour)))
	for i := 0; i < 2; i++ {
		if _, err := manager.GetValidToken(); !errors.Is(err, ErrCredentialPersistence) {
			t.Fatalf("expected pending save: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".credentials.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleted source recreated")
	}
	files, err := filepath.Glob(filepath.Join(dir, ".cc-proxy-credentials-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary files left behind: %v", err)
	}
}
