package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCredentialFile_refreshPreservesSymlinkAndUpdatesCanonicalTarget(t *testing.T) {
	manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		return refreshResponse(req, 200, syntheticRefreshSuccess), nil
	})
	target := filepath.Join(t.TempDir(), "target.json")
	writeSyntheticRecord(t, target, syntheticTokenCredentials("stale", time.Now().Add(-time.Hour)))
	link := filepath.Join(dir, ".credentials.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.GetValidToken(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("credential symlink replaced: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := parseCredentials(data)
	if err != nil || creds.AccessToken != "synthetic-new-access" {
		t.Fatalf("canonical target not updated: %v", err)
	}
	info, err = os.Stat(target)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("canonical target mode: %v", err)
	}
}

func TestCredentialFile_retargetDuringRefreshDoesNotOverwriteDestination(t *testing.T) {
	var link, target string
	fresh := syntheticTokenCredentials("login", time.Now().Add(time.Hour))
	manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		return refreshResponse(req, 200, syntheticRefreshSuccess), nil
	})
	oldTarget := filepath.Join(t.TempDir(), "original.json")
	target = filepath.Join(t.TempDir(), "new.json")
	stale := syntheticTokenCredentials("stale", time.Now().Add(-time.Hour))
	writeSyntheticRecord(t, oldTarget, stale)
	writeSyntheticRecord(t, target, fresh)
	link = filepath.Join(dir, ".credentials.json")
	if err := os.Symlink(oldTarget, link); err != nil {
		t.Fatal(err)
	}
	if token, err := manager.GetValidToken(); err != nil || token != fresh.AccessToken {
		t.Fatalf("retargeted login not adopted: %v", err)
	}
	resolved, err := os.Readlink(link)
	if err != nil || resolved != target {
		t.Fatalf("new symlink target changed: %v", err)
	}
	for path, want := range map[string]Credentials{oldTarget: stale, target: fresh} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseCredentials(data)
		if err != nil || *got != want {
			t.Fatalf("retargeting overwrote source: %v", err)
		}
	}
}

func TestCredentialFile_directorySyncFailureRetainsPendingAndRetriesDurability(t *testing.T) {
	calls, syncs := 0, 0
	manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		calls++
		return refreshResponse(req, 200, syntheticRefreshSuccess), nil
	})
	writeSourceCredentials(t, dir, syntheticTokenCredentials("stale", time.Now().Add(-time.Hour)))
	fail := true
	manager.store.(*sourceCredentialStore).syncDirectory = func(path string) error {
		syncs++
		canonicalDir, err := filepath.EvalSymlinks(dir)
		if err != nil || path != canonicalDir {
			t.Fatalf("wrong directory synced: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
		if err != nil {
			t.Fatal(err)
		}
		creds, err := parseCredentials(data)
		if err != nil || creds.AccessToken != "synthetic-new-access" {
			t.Fatalf("directory sync preceded rename: %v", err)
		}
		if fail {
			return errors.New("synthetic-private-directory-failure")
		}
		return nil
	}
	for i := 0; i < 2; i++ {
		token, err := manager.GetValidToken()
		if !errors.Is(err, ErrCredentialPersistence) || token != "" || strings.Contains(err.Error(), "private") || manager.pending == nil {
			t.Fatalf("directory failure not retained/sanitized: %v", err)
		}
	}
	fail = false
	if token, err := manager.GetValidToken(); err != nil || token != "synthetic-new-access" || calls != 1 || syncs != 3 {
		t.Fatalf("directory durability not retried: refreshes=%d syncs=%d err=%v", calls, syncs, err)
	}
}

func TestCredentialFile_unchangedAuthRetargetKeepsOriginalPendingTarget(t *testing.T) {
	var link, newTarget string
	calls := 0
	manager, dir := isolatedTokenManager(t, func(req *http.Request) (*http.Response, error) {
		calls++
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(newTarget, link); err != nil {
			t.Fatal(err)
		}
		return refreshResponse(req, 200, syntheticRefreshSuccess), nil
	})
	oldTarget := filepath.Join(t.TempDir(), "original.json")
	newTarget = filepath.Join(t.TempDir(), "replacement.json")
	stale := syntheticTokenCredentials("stale", time.Now().Add(-time.Hour))
	writeSyntheticRecord(t, oldTarget, stale)
	writeSyntheticRecord(t, newTarget, stale)
	link = filepath.Join(dir, ".credentials.json")
	if err := os.Symlink(oldTarget, link); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.GetValidToken(); !errors.Is(err, ErrCredentialPersistence) {
		t.Fatalf("retargeted unchanged auth should retain rotation: %v", err)
	}
	wantTarget, err := filepath.EvalSymlinks(oldTarget)
	if err != nil {
		t.Fatal(err)
	}
	if manager.pending == nil || manager.pending.base.path != wantTarget || manager.creds.AccessToken != "synthetic-new-access" {
		t.Fatal("original pending destination or new credentials lost")
	}
	for _, path := range []string{oldTarget, newTarget} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseCredentials(data)
		if err != nil || *got != stale {
			t.Fatalf("retargeted unchanged source overwritten: %v", err)
		}
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(oldTarget, link); err != nil {
		t.Fatal(err)
	}
	if token, err := manager.GetValidToken(); err != nil || token != "synthetic-new-access" || calls != 1 {
		t.Fatalf("pending target recovery: refreshes=%d err=%v", calls, err)
	}
	if _, err := os.Readlink(link); err != nil {
		t.Fatal("recovery replaced the symlink")
	}
}
