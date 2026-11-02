package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

func keychainIdentity() (string, string, error) {
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

func readKeychainData(service, account string) ([]byte, error) {
	out, err := exec.Command("security", "find-generic-password", "-s", service, "-a", account, "-w").Output()
	if err != nil {
		return nil, errors.New("keychain lookup failed")
	}
	if len(out) > 2*maxCredentialDocument+1 {
		return nil, errCredentialDocument
	}
	data := []byte(strings.TrimSuffix(string(out), "\n"))
	if !json.Valid(data) {
		decoded, err := hex.DecodeString(string(data))
		if err != nil {
			return nil, errCredentialDocument
		}
		data = decoded
	}
	return data, nil
}

func (tm *TokenManager) readFromKeychain() (*Credentials, error) {
	service, account, err := keychainIdentity()
	if err != nil {
		return nil, errors.New("keychain identity unavailable")
	}
	data, err := readKeychainData(service, account)
	if err != nil {
		return nil, err
	}
	return parseCredentials(data)
}

func (tm *TokenManager) readFromFile() (*Credentials, error) {
	configDir, _, err := effectiveConfigDir()
	if err != nil {
		return nil, err
	}
	data, err := readCredentialFile(filepath.Join(configDir, ".credentials.json"))
	if err != nil {
		return nil, errors.New("credential file unavailable")
	}
	return parseCredentials(data)
}

const maxCredentialDocument = 1024 * 1024

func readCredentialFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("credential file unavailable")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxCredentialDocument+1))
	if err != nil || len(data) > maxCredentialDocument {
		return nil, errors.New("credential file unreadable")
	}
	return data, nil
}
