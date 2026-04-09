package main

const (
	DefaultListenAddr = "127.0.0.1:34156"
	UpstreamBase      = "https://api.anthropic.com"
	OAuthClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	TokenRefreshURL   = "https://platform.claude.com/v1/oauth/token"
	CCVersion         = "2.1.97"
	AnthropicVersion  = "2023-06-01"

	KeychainService = "Claude Code-credentials"
)

// Required beta features to include in every request.
var RequiredBetas = []string{"claude-code-20250219", "oauth-2025-04-20"}

// Beta features to strip from requests.
var StripBetas = []string{"context-1m"}

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
