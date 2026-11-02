package main

import (
	"bytes"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestKeychainRead_preservesNativeBytesForCLIEncodings(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		hex  bool
	}{
		{"printable", syntheticCredentials, false},
		{"printable_whitespace", "  " + syntheticCredentials + " ", false},
		{"newline", "\n" + syntheticCredentials + "\n", true},
		{"utf8", "{\"accessToken\":\"synthetic-access\",\"refreshToken\":\"synthetic-refresh\",\"expiresAt\":4102444800000,\"label\":\"caf\u00e9\"}", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Given: the documented security -w representation of synthetic bytes.
			dir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			path := filepath.Join(dir, "synthetic-output")
			t.Setenv("SYNTHETIC_KEYCHAIN_OUTPUT", path)
			fakeSecurity(t, "exec /bin/cat \"$SYNTHETIC_KEYCHAIN_OUTPUT\"\n")
			output := tc.raw
			if tc.hex {
				output = hex.EncodeToString([]byte(tc.raw))
			}
			if err := os.WriteFile(path, []byte(output+"\n"), 0600); err != nil {
				t.Fatal(err)
			}

			// When: the CLI boundary decodes its response.
			got, err := readKeychainData("synthetic-service", "synthetic-account")

			// Then: native compare-before-write receives the exact original bytes.
			if err != nil || !bytes.Equal(got, []byte(tc.raw)) {
				t.Fatalf("original Keychain bytes were not preserved: %v", err)
			}
		})
	}
}

func TestKeychainRefresh_updatesHexRenderedDocumentWithoutChangingMetadata(t *testing.T) {
	// Given: a multiline Keychain document rendered as hex by the CLI.
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := filepath.Join(dir, "synthetic-output")
	t.Setenv("SYNTHETIC_KEYCHAIN_OUTPUT", path)
	fakeSecurity(t, "exec /bin/cat \"$SYNTHETIC_KEYCHAIN_OUTPUT\"\n")
	raw := []byte("{\n\"accessToken\":\"synthetic-old-access\",\"refreshToken\":\"synthetic-old-refresh\",\"expiresAt\":1,\"label\":\"caf\u00e9\"\n}\n")
	if err := os.WriteFile(path, []byte(hex.EncodeToString(raw)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manager := NewTokenManager()
	manager.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return refreshResponse(req, http.StatusOK, syntheticRefreshSuccess), nil
	})
	writes := 0
	manager.store.(*sourceCredentialStore).nativeWrite = func(service, account string, expected, next []byte) error {
		writes++
		if !bytes.Equal(expected, raw) {
			t.Error("native writer received rendered text instead of original bytes")
		}
		return os.WriteFile(path, []byte(hex.EncodeToString(next)+"\n"), 0600)
	}

	// When: refreshing and persisting through the synthetic Keychain boundary.
	token, err := manager.GetValidToken()

	// Then: the rotation succeeds and non-ASCII metadata survives a fresh read.
	if err != nil || token != "synthetic-new-access" || writes != 1 {
		t.Fatalf("hex-rendered Keychain refresh failed: writes=%d err=%v", writes, err)
	}
	record, err := manager.store.Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := decodeCredentialDocument(record.data)
	if err != nil || !bytes.Equal(doc.top["label"], []byte("\"caf\u00e9\"")) {
		t.Fatalf("metadata did not survive Keychain rotation: %v", err)
	}
}
