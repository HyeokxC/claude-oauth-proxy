package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
)

// TransformRequestHeaders replaces auth and injects required headers.
func TransformRequestHeaders(upstream *http.Request, original *http.Request, token string) {
	// Copy all headers from original
	for k, vv := range original.Header {
		for _, v := range vv {
			upstream.Header.Add(k, v)
		}
	}

	// Strip headers that must be rewritten
	upstream.Header.Del("Accept-Encoding")
	upstream.Header.Del("Content-Length") // let Go set this from body size
	upstream.Header.Del("Host")

	// 1. Auth: remove x-api-key, set Bearer token
	upstream.Header.Del("x-api-key")
	upstream.Header.Set("Authorization", "Bearer "+token)

	// 2. Billing header
	upstream.Header.Set("x-anthropic-billing-header",
		fmt.Sprintf("cc_version=%s.cli; cc_entrypoint=cli; cch=00000;", CCVersion))

	// 3. Beta headers: merge required, strip unwanted
	transformBetaHeaders(upstream)

	// 4. Anthropic version
	upstream.Header.Set("anthropic-version", AnthropicVersion)

	// 5. User-Agent: must look like Claude Code
	upstream.Header.Set("User-Agent", fmt.Sprintf("claude-code/%s", CCVersion))
}

func transformBetaHeaders(req *http.Request) {
	existing := req.Header.Get("anthropic-beta")
	var betas []string
	if existing != "" {
		betas = strings.Split(existing, ",")
	}

	// Strip unwanted betas
	filtered := make([]string, 0, len(betas))
	for _, b := range betas {
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		strip := false
		for _, s := range StripBetas {
			if b == s {
				strip = true
				break
			}
		}
		if !strip {
			filtered = append(filtered, b)
		}
	}

	// Add required betas (dedup)
	for _, req := range RequiredBetas {
		found := false
		for _, b := range filtered {
			if b == req {
				found = true
				break
			}
		}
		if !found {
			filtered = append(filtered, req)
		}
	}

	req.Header.Set("anthropic-beta", strings.Join(filtered, ","))
}

// TransformRequestBody applies tool name substitution and system prompt conversion.
func TransformRequestBody(body []byte) []byte {
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		log.Printf("[transform] failed to parse request body, forwarding as-is: %v", err)
		return body
	}

	// Tool name substitution in tools array
	if tools, ok := msg["tools"].([]any); ok {
		for _, t := range tools {
			if tool, ok := t.(map[string]any); ok {
				if name, ok := tool["name"].(string); ok {
					if mapped, exists := ToolNameMap[name]; exists {
						tool["name"] = mapped
					}
				}
			}
		}
	}

	// Tool name substitution in messages
	if messages, ok := msg["messages"].([]any); ok {
		for _, m := range messages {
			if message, ok := m.(map[string]any); ok {
				transformMessageContent(message)
			}
		}
	}

	// System prompt XML -> Markdown conversion
	transformSystemPrompt(msg)

	result, err := json.Marshal(msg)
	if err != nil {
		log.Printf("[transform] failed to marshal transformed body, forwarding original: %v", err)
		return body
	}
	return result
}

func transformMessageContent(message map[string]any) {
	content, ok := message["content"]
	if !ok {
		return
	}

	switch c := content.(type) {
	case []any:
		for _, block := range c {
			if b, ok := block.(map[string]any); ok {
				// tool_use blocks
				if b["type"] == "tool_use" {
					if name, ok := b["name"].(string); ok {
						if mapped, exists := ToolNameMap[name]; exists {
							b["name"] = mapped
						}
					}
				}
				// tool_result blocks
				if b["type"] == "tool_result" {
					if name, ok := b["name"].(string); ok {
						if mapped, exists := ToolNameMap[name]; exists {
							b["name"] = mapped
						}
					}
				}
			}
		}
	}
}

var (
	reAvailableSkillsOpen  = regexp.MustCompile(`<available_skills>`)
	reAvailableSkillsClose = regexp.MustCompile(`</available_skills>`)
	reAgentIdentity        = regexp.MustCompile(`(?s)<agent-identity>.*?</agent-identity>`)
	reEnvOpen              = regexp.MustCompile(`<env>`)
	reEnvClose             = regexp.MustCompile(`</env>`)
)

func transformSystemPrompt(msg map[string]any) {
	system, ok := msg["system"]
	if !ok {
		return
	}

	switch s := system.(type) {
	case string:
		msg["system"] = transformSystemText(s)
	case []any:
		for i, block := range s {
			if b, ok := block.(map[string]any); ok {
				if b["type"] == "text" {
					if text, ok := b["text"].(string); ok {
						b["text"] = transformSystemText(text)
					}
				}
				s[i] = b
			}
		}
	}
}

func transformSystemText(text string) string {
	text = reAvailableSkillsOpen.ReplaceAllString(text, "## Commands Reference")
	text = reAvailableSkillsClose.ReplaceAllString(text, "")
	text = reAgentIdentity.ReplaceAllString(text, "")
	text = reEnvOpen.ReplaceAllString(text, "# Environment")
	text = reEnvClose.ReplaceAllString(text, "")
	return text
}
