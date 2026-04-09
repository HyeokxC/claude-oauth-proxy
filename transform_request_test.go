package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestTransformBetaHeaders(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantHas  []string
		wantNot  []string
	}{
		{
			name:    "empty adds required",
			input:   "",
			wantHas: []string{"claude-code-20250219", "oauth-2025-04-20"},
		},
		{
			name:    "strips context-1m",
			input:   "context-1m,files-api-2025-04-14",
			wantHas: []string{"claude-code-20250219", "oauth-2025-04-20", "files-api-2025-04-14"},
			wantNot: []string{"context-1m"},
		},
		{
			name:    "deduplicates",
			input:   "oauth-2025-04-20,files-api-2025-04-14",
			wantHas: []string{"oauth-2025-04-20", "claude-code-20250219", "files-api-2025-04-14"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", "http://test", nil)
			if tt.input != "" {
				req.Header.Set("anthropic-beta", tt.input)
			}
			transformBetaHeaders(req)
			result := req.Header.Get("anthropic-beta")

			for _, want := range tt.wantHas {
				if !containsBeta(result, want) {
					t.Errorf("expected %q in %q", want, result)
				}
			}
			for _, notWant := range tt.wantNot {
				if containsBeta(result, notWant) {
					t.Errorf("did not expect %q in %q", notWant, result)
				}
			}
		})
	}
}

func containsBeta(header, beta string) bool {
	for _, b := range splitBetas(header) {
		if b == beta {
			return true
		}
	}
	return false
}

func splitBetas(s string) []string {
	var result []string
	for _, b := range split(s, ",") {
		b = trim(b)
		if b != "" {
			result = append(result, b)
		}
	}
	return result
}

func split(s, sep string) []string {
	if s == "" {
		return nil
	}
	var result []string
	for s != "" {
		i := indexOf(s, sep)
		if i < 0 {
			result = append(result, s)
			break
		}
		result = append(result, s[:i])
		s = s[i+len(sep):]
	}
	return result
}

func indexOf(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func trim(s string) string {
	for len(s) > 0 && s[0] == ' ' {
		s = s[1:]
	}
	for len(s) > 0 && s[len(s)-1] == ' ' {
		s = s[:len(s)-1]
	}
	return s
}

func TestTransformRequestBody_ToolNames(t *testing.T) {
	input := map[string]any{
		"model": "claude-opus-4-6",
		"tools": []any{
			map[string]any{"name": "todowrite", "description": "write todos"},
			map[string]any{"name": "background_output", "description": "bg output"},
			map[string]any{"name": "read_file", "description": "read a file"},
		},
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "tool_use", "id": "t1", "name": "todowrite", "input": map[string]any{}},
				},
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "t1", "name": "todowrite", "content": "done"},
				},
			},
		},
	}

	body, _ := json.Marshal(input)
	result := TransformRequestBody(body)

	var output map[string]any
	json.Unmarshal(result, &output)

	// Check tools
	tools := output["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "task_write" {
		t.Errorf("expected task_write, got %v", tools[0].(map[string]any)["name"])
	}
	if tools[1].(map[string]any)["name"] != "bg_output" {
		t.Errorf("expected bg_output, got %v", tools[1].(map[string]any)["name"])
	}
	if tools[2].(map[string]any)["name"] != "read_file" {
		t.Errorf("expected read_file unchanged, got %v", tools[2].(map[string]any)["name"])
	}

	// Check messages
	msgs := output["messages"].([]any)
	assistantContent := msgs[0].(map[string]any)["content"].([]any)
	if assistantContent[0].(map[string]any)["name"] != "task_write" {
		t.Errorf("expected tool_use name task_write, got %v", assistantContent[0].(map[string]any)["name"])
	}
	userContent := msgs[1].(map[string]any)["content"].([]any)
	if userContent[0].(map[string]any)["name"] != "task_write" {
		t.Errorf("expected tool_result name task_write, got %v", userContent[0].(map[string]any)["name"])
	}
}

func TestTransformSystemPrompt(t *testing.T) {
	input := map[string]any{
		"model": "claude-opus-4-6",
		"system": "Hello\n<available_skills>\nskill1\nskill2\n</available_skills>\n<agent-identity>secret identity</agent-identity>\n<env>\nENV_VAR=1\n</env>",
	}

	body, _ := json.Marshal(input)
	result := TransformRequestBody(body)

	var output map[string]any
	json.Unmarshal(result, &output)

	system := output["system"].(string)

	if containsStr(system, "<available_skills>") {
		t.Error("expected <available_skills> to be replaced")
	}
	if !containsStr(system, "## Commands Reference") {
		t.Error("expected ## Commands Reference")
	}
	if containsStr(system, "<agent-identity>") || containsStr(system, "secret identity") {
		t.Error("expected agent-identity block to be removed")
	}
	if containsStr(system, "<env>") {
		t.Error("expected <env> to be replaced")
	}
	if !containsStr(system, "# Environment") {
		t.Error("expected # Environment")
	}
}

func containsStr(s, sub string) bool {
	return indexOf(s, sub) >= 0
}
