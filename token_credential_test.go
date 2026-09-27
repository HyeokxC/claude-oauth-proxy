package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const syntheticCredentials = `{"claudeAiOauth":{"accessToken":"synthetic-access","refreshToken":"synthetic-refresh","expiresAt":4102444800000}}`

func fakeSecurity(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "security"), []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestReadFromKeychain_usesQualifiedAccountAndDefaultService(t *testing.T) {
	// Given: a synthetic security executable and the effective default config directory.
	userInfo, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("USER", "not-the-os-account")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude", "..", ".claude"))
	dir := fakeSecurity(t, "printf '%s\n' \"$@\" > \"$CAPTURE\"\nprintf '%s' '"+syntheticCredentials+"'\n")
	capture := filepath.Join(dir, "args")
	t.Setenv("CAPTURE", capture)

	// When: the loader reads Keychain.
	creds, err := NewTokenManager().readFromKeychain()

	// Then: it names the OS account and unsuffixed service, never an account-less lookup.
	if err != nil {
		t.Fatal(err)
	}
	if creds.AccessToken != "synthetic-access" {
		t.Fatalf("access token: got %q", creds.AccessToken)
	}
	args, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"find-generic-password", "-s", KeychainService, "-a", keychainAccount(userInfo.Username), "-w"}
	if got := strings.Split(strings.TrimSpace(string(args)), "\n"); !reflect.DeepEqual(got, want) {
		t.Fatalf("security args: got %q, want %q", got, want)
	}
}

func TestReadFromKeychain_hashesAbsoluteNonDefaultConfigDirectory(t *testing.T) {
	// Given: a non-default config dir expressed with a relative segment.
	configDir := filepath.Join(t.TempDir(), "config")
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(workingDir, configDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(relative, "..", "config"))
	dir := fakeSecurity(t, "printf '%s\n' \"$@\" > \"$CAPTURE\"\nprintf '%s' '"+syntheticCredentials+"'\n")
	capture := filepath.Join(dir, "args")
	t.Setenv("CAPTURE", capture)

	// When: the loader reads Keychain.
	_, err = NewTokenManager().readFromKeychain()

	// Then: the service suffix is the first eight lowercase SHA-256 hex digits of the absolute path.
	if err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(configDir))
	wantService := fmt.Sprintf("%s-%x", KeychainService, sum[:4])
	if !strings.Contains(string(args), "-s\n"+wantService+"\n-a\n") {
		t.Fatalf("security args lack expected config-derived service: %q", string(args))
	}
}

func TestReadFromFile_usesEffectiveConfigDirectory(t *testing.T) {
	// Given: synthetic credentials only in an overridden config directory.
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	if err := os.WriteFile(filepath.Join(configDir, ".credentials.json"), []byte(`{"accessToken":"synthetic-flat","refreshToken":"synthetic-refresh","expiresAt":4102444800000}`), 0600); err != nil {
		t.Fatal(err)
	}

	// When: the loader uses the file fallback.
	creds, err := NewTokenManager().readFromFile()

	// Then: the flat file in the effective directory is parsed.
	if err != nil {
		t.Fatal(err)
	}
	if creds.AccessToken != "synthetic-flat" {
		t.Fatalf("access token: got %q", creds.AccessToken)
	}
}

func TestReadFromKeychain_doesNotExposeCommandStderr(t *testing.T) {
	// Given: a failed synthetic security command with sensitive diagnostic text.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	fakeSecurity(t, "printf '%s' 'synthetic-sensitive-stderr' >&2\nexit 1\n")

	// When: Keychain lookup fails.
	_, err := NewTokenManager().readFromKeychain()

	// Then: its returned error cannot leak the command's diagnostic.
	if err == nil || strings.Contains(err.Error(), "synthetic-sensitive-stderr") {
		t.Fatalf("command error must be secret-free, got %v", err)
	}
}

func TestParseCredentials_acceptsNestedAndFlat(t *testing.T) {
	for _, input := range []string{syntheticCredentials, `{"accessToken":"synthetic-access","refreshToken":"synthetic-refresh","expiresAt":4102444800000}`} {
		// Given: a synthetic credential encoding.
		// When: credentials are parsed.
		creds, err := parseCredentials([]byte(input))
		// Then: both formats retain the same token value.
		if err != nil || creds.AccessToken != "synthetic-access" {
			t.Fatalf("parse synthetic credentials: %v", err)
		}
	}
}

func TestKeychainAccount_fallsBackForUnsafeUsername(t *testing.T) {
	for _, name := range []string{"", "name with spaces", "name/with/slash", "nonascii-한글"} {
		// Given: a username outside Claude Code's safe account character set.
		// When: the username is selected for a Keychain account.
		got := keychainAccount(name)
		// Then: the fixed compatibility account is used.
		if got != "claude-code-user" {
			t.Fatalf("unsafe username %q selected account %q", name, got)
		}
	}
	for _, name := range []string{"alice", "alice.smith-01_test"} {
		if got := keychainAccount(name); got != name {
			t.Fatalf("safe username %q selected account %q", name, got)
		}
	}
}

func TestReadFromKeychain_usesDefaultServiceWhenOverrideUnset(t *testing.T) {
	// Given: no effective config override and a synthetic security executable.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	dir := fakeSecurity(t, "printf '%s\n' \"$@\" > \"$CAPTURE\"\nprintf '%s' '"+syntheticCredentials+"'\n")
	capture := filepath.Join(dir, "args")
	t.Setenv("CAPTURE", capture)

	// When: Keychain credentials are loaded.
	_, err := NewTokenManager().readFromKeychain()

	// Then: the default service is not suffixed.
	if err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "-s\n"+KeychainService+"\n-a\n") {
		t.Fatalf("default service absent from security args: %q", string(args))
	}
}
