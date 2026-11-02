package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func migratingTokenManager(t *testing.T, transport roundTripFunc) (*TokenManager, string, string) {
	t.Helper()
	manager, dir := isolatedTokenManager(t, transport)
	keychain := filepath.Join(dir, "synthetic-keychain.json")
	t.Setenv("SYNTHETIC_KEYCHAIN", keychain)
	fakeSecurity(t, "exec /bin/cat \"$SYNTHETIC_KEYCHAIN\"\n")
	manager.store.(*sourceCredentialStore).nativeWrite = func(service, account string, expected, next []byte) error {
		t.Error("source migration must not write the new Keychain login")
		return errStoreWrite
	}
	return manager, dir, keychain
}

func writeSyntheticRecord(t *testing.T, path string, creds Credentials) {
	t.Helper()
	data, err := json.Marshal(creds)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func removeOldSource(t *testing.T, dir string, remove bool) {
	t.Helper()
	if remove {
		if err := os.Remove(filepath.Join(dir, ".credentials.json")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSourceMigration_activeKeychainWinsDuringReloadAndRecovery(t *testing.T) {
	for _, phase := range []string{"reload", "failed_refresh", "pending_save", "successful_refresh"} {
		for _, remove := range []bool{false, true} {
			name := phase + "/old_file_remains"
			if remove {
				name = phase + "/old_file_removed"
			}
			t.Run(name, func(t *testing.T) {
				calls := 0
				var dir, keychain string
				fresh := syntheticTokenCredentials("login", time.Now().Add(time.Hour))
				manager, dir, keychain := migratingTokenManager(t, func(req *http.Request) (*http.Response, error) {
					calls++
					if phase == "failed_refresh" || phase == "successful_refresh" {
						writeSyntheticRecord(t, keychain, fresh)
						removeOldSource(t, dir, remove)
					}
					if phase == "failed_refresh" {
						return refreshResponse(req, 400, `{"error":"invalid_grant"}`), nil
					}
					return refreshResponse(req, 200, syntheticRefreshSuccess), nil
				})
				expiry := time.Now().Add(-time.Hour)
				if phase == "reload" {
					expiry = time.Now().Add(time.Hour)
				}
				stale := syntheticTokenCredentials("stale", expiry)
				writeSourceCredentials(t, dir, stale)
				if phase == "pending_save" {
					manager.store = &failingSaveStore{credentialStore: manager.store, fail: true}
					if _, err := manager.GetValidToken(); !errors.Is(err, ErrCredentialPersistence) {
						t.Fatal(err)
					}
				}
				if phase == "reload" {
					if _, err := manager.GetValidToken(); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "reload" || phase == "pending_save" {
					writeSyntheticRecord(t, keychain, fresh)
					removeOldSource(t, dir, remove)
				}
				if token, err := manager.GetValidToken(); err != nil || token != fresh.AccessToken {
					t.Fatalf("active Keychain login not adopted: %v", err)
				}
				wantCalls := 1
				if phase == "reload" {
					wantCalls = 0
				}
				if calls != wantCalls || manager.pending != nil || manager.source.path != "" {
					t.Fatalf("migration state: refreshes=%d pending=%t", calls, manager.pending != nil)
				}
				if !remove {
					data, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
					if err != nil {
						t.Fatal(err)
					}
					creds, err := parseCredentials(data)
					if err != nil || *creds != stale {
						t.Fatalf("inactive old file overwritten: %v", err)
					}
				}
			})
		}
	}
}

func TestSourceMigration_unchangedRevisionPreservesPendingDestination(t *testing.T) {
	for _, duringRefresh := range []bool{false, true} {
		for _, remove := range []bool{false, true} {
			name := "pending/old_file_remains"
			if duringRefresh {
				name = "during_refresh/old_file_remains"
			}
			if remove {
				name += "_then_removed"
			}
			t.Run(name, func(t *testing.T) {
				calls := 0
				var dir, keychain string
				stale := syntheticTokenCredentials("stale", time.Now().Add(-time.Hour))
				manager, dir, keychain := migratingTokenManager(t, func(req *http.Request) (*http.Response, error) {
					calls++
					if duringRefresh {
						writeSyntheticRecord(t, keychain, stale)
						removeOldSource(t, dir, remove)
					}
					return refreshResponse(req, 200, syntheticRefreshSuccess), nil
				})
				writeSourceCredentials(t, dir, stale)
				store := &failingSaveStore{credentialStore: manager.store, fail: !duringRefresh}
				manager.store = store
				if _, err := manager.GetValidToken(); !errors.Is(err, ErrCredentialPersistence) {
					t.Fatalf("rotation should stay pending: %v", err)
				}
				base := manager.pending.base
				if !duringRefresh {
					writeSyntheticRecord(t, keychain, stale)
					removeOldSource(t, dir, remove)
				}
				store.fail = false
				if _, err := manager.GetValidToken(); !errors.Is(err, ErrCredentialPersistence) {
					t.Fatalf("unchanged migrated auth must not replace pending rotation: %v", err)
				}
				if manager.pending == nil || manager.pending.base != base || manager.creds.AccessToken != "synthetic-new-access" || calls != 1 {
					t.Fatal("pending rotation/destination lost or refreshed again")
				}
				if err := os.Remove(keychain); err != nil {
					t.Fatal(err)
				}
				if remove {
					writeSourceCredentials(t, dir, stale)
				}
				if token, err := manager.GetValidToken(); err != nil || token != "synthetic-new-access" || calls != 1 {
					t.Fatalf("original destination recovery failed: calls=%d err=%v", calls, err)
				}
			})
		}
	}
}
