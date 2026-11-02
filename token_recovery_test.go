package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRefresh_prefersFreshSourceBeforeOAuth(t *testing.T) {
	for _, forced := range []bool{false, true} {
		name := "expired_cache"
		if forced {
			name = "rejected_cache"
		}
		t.Run(name, func(t *testing.T) {
			manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
				t.Error("fresh source must be preferred to refresh")
				return nil, errors.New("blocked refresh")
			})
			stale := syntheticTokenCredentials("stale", time.Now().Add(-time.Hour))
			manager.creds = &stale
			fresh := syntheticTokenCredentials("login", time.Now().Add(time.Hour))
			writeSourceCredentials(t, dir, fresh)
			if forced {
				if err := manager.ForceRefresh(); err != nil {
					t.Fatal(err)
				}
			}
			if token, err := manager.GetValidToken(); err != nil || token != fresh.AccessToken {
				t.Fatalf("fresh source not used: %v", err)
			}
		})
	}
}

func TestRefreshRecovery_secondDistinctRevisionCanRefresh(t *testing.T) {
	var dir string
	calls := 0
	second := syntheticTokenCredentials("second", time.Now().Add(-time.Hour))
	manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			writeSourceCredentials(t, dir, second)
			return refreshResponse(req, 400, `{"error":"invalid_grant"}`), nil
		}
		if err := req.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if req.PostForm.Get("refresh_token") != second.RefreshToken {
			t.Error("second attempt used stale revision")
		}
		return refreshResponse(req, 200, syntheticRefreshSuccess), nil
	})
	writeSourceCredentials(t, dir, syntheticTokenCredentials("first", time.Now().Add(-time.Hour)))
	if token, err := manager.GetValidToken(); err != nil || token != "synthetic-new-access" || calls != 2 {
		t.Fatalf("second revision recovery: calls=%d err=%v", calls, err)
	}
}

func TestForcedRefresh_remembersRejectionWhenSourceUnavailable(t *testing.T) {
	calls := 0
	manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		calls++
		return refreshResponse(req, 400, `{"error":"invalid_grant"}`), nil
	})
	stale := syntheticTokenCredentials("stale", time.Now().Add(time.Hour))
	writeSourceCredentials(t, dir, stale)
	if _, err := manager.GetValidToken(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, ".credentials.json")); err != nil {
		t.Fatal(err)
	}
	if err := manager.ForceRefreshRejected(stale.AccessToken); err == nil {
		t.Fatal("missing source should fail")
	}
	writeSourceCredentials(t, dir, stale)
	if token, err := manager.GetValidToken(); err == nil || token != "" || calls != 1 {
		t.Fatalf("rejected token served after source recovery: calls=%d err=%v", calls, err)
	}
}

func TestForcedRefresh_lateOld401DoesNotClearNewRejection(t *testing.T) {
	calls := 0
	manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return refreshResponse(req, 200, syntheticRefreshSuccess), nil
		}
		return refreshResponse(req, 400, `{"error":"invalid_grant"}`), nil
	})
	stale := syntheticTokenCredentials("stale", time.Now().Add(time.Hour))
	writeSourceCredentials(t, dir, stale)
	if err := manager.ForceRefreshRejected(stale.AccessToken); err != nil {
		t.Fatal(err)
	}
	if err := manager.ForceRefreshRejected("synthetic-new-access"); err == nil {
		t.Fatal("new rejection should fail")
	}
	if err := manager.ForceRefreshRejected(stale.AccessToken); err == nil {
		t.Fatal("late rejection bypassed failure cooldown")
	}
	if token, err := manager.GetValidToken(); err == nil || token != "" || calls != 2 {
		t.Fatalf("new rejected token served: calls=%d err=%v", calls, err)
	}
}

func TestRefresh_shortLivedSuccessDoesNotRefreshTwice(t *testing.T) {
	calls := 0
	manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		calls++
		return refreshResponse(req, 200, `{"access_token":"synthetic-new-access","expires_in":60}`), nil
	})
	writeSourceCredentials(t, dir, syntheticTokenCredentials("stale", time.Now().Add(-time.Hour)))
	store := &failingSaveStore{credentialStore: manager.store, fail: true}
	manager.store = store
	if _, err := manager.GetValidToken(); !errors.Is(err, ErrCredentialPersistence) {
		t.Fatal(err)
	}
	store.fail = false
	if token, err := manager.GetValidToken(); err != nil || token != "synthetic-new-access" || calls != 1 {
		t.Fatalf("pending short-lived rotation refreshed again: calls=%d err=%v", calls, err)
	}
}

func TestRefresh_sameRejectedAccessRetainsRotationButCoolsDown(t *testing.T) {
	calls := 0
	manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		calls++
		return refreshResponse(req, 200, `{"access_token":"synthetic-stale-access","refresh_token":"synthetic-rotated-refresh","expires_in":3600}`), nil
	})
	stale := syntheticTokenCredentials("stale", time.Now().Add(time.Hour))
	writeSourceCredentials(t, dir, stale)
	if err := manager.ForceRefreshRejected(stale.AccessToken); err == nil {
		t.Fatal("rejected access was accepted")
	}
	if token, err := manager.GetValidToken(); err == nil || token != "" || calls != 1 {
		t.Fatalf("rejected rotation did not cool down: calls=%d err=%v", calls, err)
	}
	record, err := manager.store.Load(manager.source)
	if err != nil || record.auth.RefreshToken != "synthetic-rotated-refresh" {
		t.Fatalf("refresh rotation lost: %v", err)
	}
}
