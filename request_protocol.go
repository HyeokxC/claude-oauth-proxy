package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf16"
)

const sdkIdentity = "You are a Claude agent, built on Anthropic's Claude Agent SDK."

var sessionIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)
var optionalSkillsCatalog = regexp.MustCompile(`(?s)<available_skills>.*?</available_skills>`)

func validSessionID(value string) bool { return sessionIDPattern.MatchString(value) }

func newSessionID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate request session: %w", err)
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		id[:4], id[4:6], id[6:8], id[8:10], id[10:]), nil
}

func firstUserText(msg map[string]any) string {
	messages, _ := msg["messages"].([]any)
	for _, item := range messages {
		message, ok := item.(map[string]any)
		if !ok || message["role"] != "user" {
			continue
		}
		switch content := message["content"].(type) {
		case string:
			if content != "" {
				return content
			}
		case []any:
			for _, item := range content {
				block, ok := item.(map[string]any)
				if !ok || block["type"] != "text" || block["isMeta"] == true {
					continue
				}
				if text, ok := block["text"].(string); ok && text != "" {
					return text
				}
			}
		}
	}
	return ""
}

func billingFingerprint(text string) string {
	units := utf16.Encode([]rune(text))
	sampled := ""
	for _, position := range [...]int{4, 7, 20} {
		if position >= len(units) {
			sampled += "0"
			continue
		}
		sampled += string(utf16.Decode([]uint16{units[position]}))
	}
	hash := sha256.Sum256([]byte("59cf53e54c78" + sampled + CCVersion))
	return hex.EncodeToString(hash[:])[:3]
}

func addNativeSystemBlocks(msg map[string]any) {
	billing := map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=" + CCVersion + "." + billingFingerprint(firstUserText(msg)) + "; cc_entrypoint=sdk-cli;"}
	blocks := []any{billing}
	switch system := msg["system"].(type) {
	case string:
		blocks = append(blocks, map[string]any{"type": "text", "text": sdkIdentity})
		cleaned := optionalSkillsCatalog.ReplaceAllString(system, "")
		if strings.TrimSpace(cleaned) != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": cleaned})
		}
	case []any:
		identityPresent := false
		if len(system) > 0 {
			if first, ok := system[0].(map[string]any); ok {
				identityPresent = first["text"] == sdkIdentity
			}
		}
		if !identityPresent {
			blocks = append(blocks, map[string]any{"type": "text", "text": sdkIdentity})
		}
		for _, item := range system {
			if block, ok := item.(map[string]any); ok && block["type"] == "text" {
				if text, ok := block["text"].(string); ok {
					cleaned := optionalSkillsCatalog.ReplaceAllString(text, "")
					if strings.TrimSpace(cleaned) == "" && cleaned != text {
						continue
					}
					block["text"] = cleaned
				}
			}
			blocks = append(blocks, item)
		}
	default:
		blocks = append(blocks, map[string]any{"type": "text", "text": sdkIdentity})
	}
	msg["system"] = blocks
}
