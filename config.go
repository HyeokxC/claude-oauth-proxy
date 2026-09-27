package main

const (
	DefaultListenAddr = "127.0.0.1:34156"
	UpstreamBase      = "https://api.anthropic.com"
	OAuthClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	TokenRefreshURL   = "https://platform.claude.com/v1/oauth/token"
	CCVersion         = "2.1.282"
	AnthropicVersion  = "2023-06-01"

	KeychainService = "Claude Code-credentials"

	// MaxTokensCap caps max_tokens in /messages requests to keep the per-request
	// footprint within Claude Code OAuth subscription rate-limit windows.
	// Anthropic reserves max_tokens upfront in the per-minute bucket regardless
	// of actual response length, so an uncapped 32k+ ask burns the budget on
	// the very first request.
	MaxTokensCap = 16384
)

// Required beta features to include in every request.
var RequiredBetas = []string{
	"claude-code-20250219", "oauth-2025-04-20", "interleaved-thinking-2025-05-14",
	"thinking-token-count-2026-05-13", "context-management-2025-06-27",
	"prompt-caching-scope-2026-01-05", "mid-conversation-system-2026-04-07",
	"per-turn-control-2026-07-01", "mid-conversation-tool-changes-2026-07-01",
	"effort-2025-11-24", "extended-cache-ttl-2025-04-11",
}

// Beta features to strip from requests.
// context-1m and structured-outputs are not present in the captured native profile.
var StripBetas = []string{
	"context-1m",
	"structured-outputs-2025-11-13",
}

// Tool name mappings: OpenCode name -> Claude Code name.
var ToolNameMap = map[string]string{
	"todowrite":         "task_write",
	"background_output": "bg_output",
}

// Reverse map built at init.
var ReverseToolNameMap map[string]string

func init() {
	ReverseToolNameMap = make(map[string]string, len(ToolNameMap))
	for k, v := range ToolNameMap {
		ReverseToolNameMap[v] = k
	}
}
