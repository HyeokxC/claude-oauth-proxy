package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRefresh_preservesLatestMetadataAndFileShape(t *testing.T) {
	for _, nested := range []bool{false, true} {
		name := "flat"
		if nested {
			name = "nested"
		}
		t.Run(name, func(t *testing.T) {
			var dir string
			stale := syntheticTokenCredentials("stale", time.Now().Add(-time.Hour))
			encode := func(metadata string) []byte {
				t.Helper()
				raw, err := json.Marshal(stale)
				if err != nil {
					t.Fatal(err)
				}
				oauth := string(raw[:len(raw)-1]) + `,"scopes":["synthetic-scope"],"precise":900719925474099312345,"metadata":"` + metadata + `"}`
				if nested {
					oauth = `{"claudeAiOauth":` + oauth + `,"other":{"keep":[true,42]},"large":900719925474099312346}`
				}
				return []byte(oauth)
			}
			manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
				writeCredentialFile(t, dir, encode("latest"))
				// Missing refresh_token must retain the previous token.
				return refreshResponse(req, 200, `{"access_token":"synthetic-new-access","expires_in":3600}`), nil
			})
			writeCredentialFile(t, dir, encode("old"))
			if _, err := manager.GetValidToken(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, ".credentials.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := decodeCredentialDocument(data)
			if err != nil {
				t.Fatal(err)
			}
			if doc.nested != nested || doc.auth.AccessToken != "synthetic-new-access" || doc.auth.RefreshToken != stale.RefreshToken || !bytes.Equal(doc.oauth["metadata"], []byte(`"latest"`)) || !bytes.Equal(doc.oauth["precise"], []byte(`900719925474099312345`)) {
				t.Fatal("credential shape, latest metadata, or token preservation failed")
			}
			if nested && (!bytes.Equal(doc.top["large"], []byte(`900719925474099312346`)) || !bytes.Equal(doc.top["other"], []byte(`{"keep":[true,42]}`))) {
				t.Fatal("top-level metadata lost")
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("file mode: %v", err)
			}
			files, err := filepath.Glob(filepath.Join(dir, ".cc-proxy-credentials-*"))
			if err != nil || len(files) != 0 {
				t.Fatal("temporary files left behind")
			}
		})
	}
}

func TestRefresh_concurrentReloginWinsBeforeSave(t *testing.T) {
	var dir string
	fresh := syntheticTokenCredentials("login", time.Now().Add(time.Hour))
	manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		writeSourceCredentials(t, dir, fresh)
		return refreshResponse(req, 200, syntheticRefreshSuccess), nil
	})
	writeSourceCredentials(t, dir, syntheticTokenCredentials("stale", time.Now().Add(-time.Hour)))
	if token, err := manager.GetValidToken(); err != nil || token != fresh.AccessToken {
		t.Fatalf("new login not adopted: %v", err)
	}
	record, err := manager.store.Load(manager.source)
	if err != nil || record.auth != fresh {
		t.Fatalf("new login overwritten: %v", err)
	}
}

func TestKeychainPersistence_usesInjectedUpdateOnlyWriter(t *testing.T) {
	for _, result := range []string{"success", "failure", "not_found", "compare_conflict"} {
		t.Run(result, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			path := filepath.Join(dir, "synthetic-keychain.json")
			t.Setenv("SYNTHETIC_KEYCHAIN", path)
			fakeSecurity(t, "exec /bin/cat \"$SYNTHETIC_KEYCHAIN\"\n")
			stale := syntheticTokenCredentials("stale", time.Now().Add(-time.Hour))
			data, err := json.Marshal(stale)
			if err != nil {
				t.Fatal(err)
			}
			data = append(append([]byte(`{"top":{"keep":900719925474099312345},"claudeAiOauth":`), data[:len(data)-1]...), []byte(`,"scopes":["synthetic-scope"]}}`)...)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			manager := NewTokenManager()
			manager.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return refreshResponse(req, 200, syntheticRefreshSuccess), nil
			})
			writes := 0
			manager.store = &sourceCredentialStore{nativeWrite: func(service, account string, expected, next []byte) error {
				writes++
				wantService, wantAccount, err := keychainIdentity()
				if err != nil || service != wantService || account != wantAccount || !bytes.Equal(expected, data) {
					t.Fatal("writer identity or expected-data mismatch")
				}
				switch result {
				case "failure":
					return errors.New("synthetic-private-native-error")
				case "not_found":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					return errKeychainNotFound
				case "compare_conflict":
					fresh := syntheticTokenCredentials("login", time.Now().Add(time.Hour))
					raw, err := json.Marshal(fresh)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, raw, 0600); err != nil {
						t.Fatal(err)
					}
					return errSourceChanged
				default:
					return os.WriteFile(path, next, 0600)
				}
			}}
			token, err := manager.GetValidToken()
			if result == "success" {
				if err != nil || token != "synthetic-new-access" {
					t.Fatalf("keychain save: %v", err)
				}
				stored, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Fatal(readErr)
				}
				doc, parseErr := decodeCredentialDocument(stored)
				if parseErr != nil || !doc.nested || !bytes.Equal(doc.top["top"], []byte(`{"keep":900719925474099312345}`)) || !bytes.Equal(doc.oauth["scopes"], []byte(`["synthetic-scope"]`)) {
					t.Fatal("native save lost metadata or shape")
				}
			} else if result == "compare_conflict" {
				if err != nil || token != "synthetic-login-access" {
					t.Fatalf("native conflict: %v", err)
				}
			} else if !errors.Is(err, ErrCredentialPersistence) || token != "" {
				t.Fatalf("expected persistence error: %v", err)
			}
			if writes != 1 {
				t.Fatalf("native writes=%d", writes)
			}
			if _, err := os.Stat(filepath.Join(dir, ".credentials.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("keychain write switched to plaintext")
			}
		})
	}
}

func TestCredentialAdvisoryLock_blocksOnlyCooperatingProxy(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	first, second := &sourceCredentialStore{}, &sourceCredentialStore{}
	unlock, err := first.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if release, err := second.Lock(); err == nil {
		release()
		t.Fatal("second proxy acquired held lock")
	}
}
