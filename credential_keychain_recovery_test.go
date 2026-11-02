package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSourceMigration_failedKeychainSaveDoesNotDowngradePendingRotation(t *testing.T) {
	for _, changedLogin := range []bool{false, true} {
		name := "unchanged_auth"
		if changedLogin {
			name = "new_file_login"
		}
		t.Run(name, func(t *testing.T) {
			calls, writes := 0, 0
			manager, dir, keychain := migratingTokenManager(t, func(req *http.Request) (*http.Response, error) {
				calls++
				return refreshResponse(req, 200, syntheticRefreshSuccess), nil
			})
			stale := syntheticTokenCredentials("stale", time.Now().Add(-time.Hour))
			writeSyntheticRecord(t, keychain, stale)
			fail := true
			manager.store.(*sourceCredentialStore).nativeWrite = func(service, account string, expected, next []byte) error {
				writes++
				if fail {
					return errors.New("synthetic-private-native-error")
				}
				return os.WriteFile(keychain, next, 0600)
			}
			if _, err := manager.GetValidToken(); !errors.Is(err, ErrCredentialPersistence) {
				t.Fatal(err)
			}
			base := manager.pending.base
			if err := os.Remove(keychain); err != nil {
				t.Fatal(err)
			}
			fileLogin := stale
			if changedLogin {
				fileLogin = syntheticTokenCredentials("file-login", time.Now().Add(time.Hour))
			}
			writeSourceCredentials(t, dir, fileLogin)
			if changedLogin {
				if token, err := manager.GetValidToken(); err != nil || token != fileLogin.AccessToken || manager.pending != nil {
					t.Fatalf("new file login not adopted: %v", err)
				}
			} else {
				if _, err := manager.GetValidToken(); !errors.Is(err, ErrCredentialPersistence) {
					t.Fatalf("failed Keychain write downgraded: %v", err)
				}
				if manager.pending == nil || manager.pending.base != base || manager.pending.base.path != "" || manager.creds.AccessToken != "synthetic-new-access" {
					t.Fatal("Keychain pending destination/rotation lost")
				}
			}
			data, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := parseCredentials(data)
			if err != nil || *got != fileLogin || calls != 1 || writes != 1 {
				t.Fatalf("plaintext source overwritten or refresh repeated: %v", err)
			}
			if !changedLogin {
				writeSyntheticRecord(t, keychain, stale)
				fail = false
				if token, err := manager.GetValidToken(); err != nil || token != "synthetic-new-access" || calls != 1 || writes != 2 {
					t.Fatalf("Keychain pending recovery: refreshes=%d writes=%d err=%v", calls, writes, err)
				}
			}
		})
	}
}
