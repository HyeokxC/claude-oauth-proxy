package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

func effectiveConfigDir() (string, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("resolve home directory: %w", err)
	}
	defaultDir, err := filepath.Abs(filepath.Join(home, ".claude"))
	if err != nil {
		return "", "", fmt.Errorf("resolve default config directory: %w", err)
	}
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if configDir == "" {
		return defaultDir, defaultDir, nil
	}
	configDir, err = filepath.Abs(configDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve config directory: %w", err)
	}
	return configDir, defaultDir, nil
}

func keychainAccount(username string) string {
	if username == "" {
		return "claude-code-user"
	}
	for _, char := range username {
		if !(char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '.' || char == '_' || char == '-') {
			return "claude-code-user"
		}
	}
	return username
}

func keychainItem() (string, string, error) {
	configDir, defaultDir, err := effectiveConfigDir()
	if err != nil {
		return "", "", err
	}
	service := KeychainService
	if configDir != defaultDir {
		sum := sha256.Sum256([]byte(configDir))
		service = fmt.Sprintf("%s-%x", service, sum[:4])
	}
	account := "claude-code-user"
	if current, err := user.Current(); err == nil {
		account = keychainAccount(current.Username)
	}
	return service, account, nil
}

func readKeychainRaw() ([]byte, error) {
	service, account, err := keychainItem()
	if err != nil {
		return nil, err
	}
	out, err := exec.Command("security", "find-generic-password", "-s", service, "-a", account, "-w").Output()
	if err != nil {
		return nil, errors.New("keychain lookup failed")
	}
	return []byte(strings.TrimSpace(string(out))), nil
}

func (tm *TokenManager) readFromKeychain() (*Credentials, error) {
	data, err := readKeychainRaw()
	if err != nil {
		return nil, err
	}
	return parseCredentials(data)
}

func credentialsFilePath() (string, error) {
	configDir, _, err := effectiveConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, ".credentials.json"), nil
}

func (tm *TokenManager) readFromFile() (*Credentials, error) {
	path, err := credentialsFilePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read credentials file: %w", err)
	}
	return parseCredentials(data)
}

func mergeCredentials(raw []byte, creds *Credentials) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, errors.New("parse stored credentials")
	}
	target := top
	nested, isNested := top["claudeAiOauth"]
	if isNested {
		target = nil
		if err := json.Unmarshal(nested, &target); err != nil || target == nil {
			return nil, errors.New("parse stored credentials")
		}
	}
	for key, value := range map[string]any{
		"accessToken":  creds.AccessToken,
		"refreshToken": creds.RefreshToken,
		"expiresAt":    creds.ExpiresAt,
	} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		target[key] = encoded
	}
	if isNested {
		encoded, err := json.Marshal(target)
		if err != nil {
			return nil, err
		}
		top["claudeAiOauth"] = encoded
	}
	return json.Marshal(top)
}

// storedMatches reports whether the stored credentials are still the snapshot we
// last synced with, so a newer CLI login is never overwritten.
func storedMatches(raw []byte, expected Credentials) bool {
	stored, err := parseCredentials(raw)
	return err == nil && *stored == expected
}

// writeToKeychain passes the secret through stdin as hex so it never appears in argv.
func (tm *TokenManager) writeToKeychain(creds *Credentials) error {
	raw, err := readKeychainRaw()
	if err != nil {
		return err
	}
	if !storedMatches(raw, tm.sourceCreds) {
		return errStoredCredentialsChanged
	}
	merged, err := mergeCredentials(raw, creds)
	if err != nil {
		return err
	}
	service, account, err := keychainItem()
	if err != nil {
		return err
	}
	cmd := exec.Command("security", "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %q -a %q -X %s\n", service, account, hex.EncodeToString(merged)))
	if out, err := cmd.CombinedOutput(); err != nil || len(bytes.TrimSpace(out)) > 0 {
		return errors.New("keychain update failed")
	}
	return nil
}

func (tm *TokenManager) writeToFile(creds *Credentials) error {
	path, err := credentialsFilePath()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return errors.New("read credentials file")
	}
	if !storedMatches(raw, tm.sourceCreds) {
		return errStoredCredentialsChanged
	}
	merged, err := mergeCredentials(raw, creds)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".credentials-*.json")
	if err != nil {
		return errors.New("create credentials temp file")
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(merged); err != nil {
		tmp.Close()
		return errors.New("write credentials temp file")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("write credentials temp file")
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return errors.New("replace credentials file")
	}
	return nil
}

var errStoredCredentialsChanged = errors.New("stored credentials changed since last sync")
