package main

import (
	"crypto/sha256"
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

func (tm *TokenManager) readFromKeychain() (*Credentials, error) {
	configDir, defaultDir, err := effectiveConfigDir()
	if err != nil {
		return nil, err
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
	out, err := exec.Command("security", "find-generic-password", "-s", service, "-a", account, "-w").Output()
	if err != nil {
		return nil, errors.New("keychain lookup failed")
	}
	return parseCredentials([]byte(strings.TrimSpace(string(out))))
}

func (tm *TokenManager) readFromFile() (*Credentials, error) {
	configDir, _, err := effectiveConfigDir()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(configDir, ".credentials.json"))
	if err != nil {
		return nil, fmt.Errorf("read credentials file: %w", err)
	}
	return parseCredentials(data)
}
