package main

import (
	"encoding/json"
	"testing"
)

func TestReverseToolNamesInSSEData_ContentBlockStart(t *testing.T) {
	event := map[string]any{
		"type":  "content_block_start",
		"index": 1,
		"content_block": map[string]any{
			"type": "tool_use",
			"id":   "toolu_01",
			"name": "task_write",
		},
	}

	data, _ := json.Marshal(event)
	result := reverseToolNamesInSSEData(data)

	var output map[string]any
	json.Unmarshal(result, &output)

	cb := output["content_block"].(map[string]any)
	if cb["name"] != "todowrite" {
		t.Errorf("expected todowrite, got %v", cb["name"])
	}
}

func TestReverseToolNamesInSSEData_BgOutput(t *testing.T) {
	event := map[string]any{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]any{
			"type": "tool_use",
			"id":   "toolu_02",
			"name": "bg_output",
		},
	}

	data, _ := json.Marshal(event)
	result := reverseToolNamesInSSEData(data)

	var output map[string]any
	json.Unmarshal(result, &output)

	cb := output["content_block"].(map[string]any)
	if cb["name"] != "background_output" {
		t.Errorf("expected background_output, got %v", cb["name"])
	}
}

func TestReverseToolNamesInSSEData_UnknownTool(t *testing.T) {
	event := map[string]any{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]any{
			"type": "tool_use",
			"id":   "toolu_03",
			"name": "read_file",
		},
	}

	data, _ := json.Marshal(event)
	result := reverseToolNamesInSSEData(data)

	var output map[string]any
	json.Unmarshal(result, &output)

	cb := output["content_block"].(map[string]any)
	if cb["name"] != "read_file" {
		t.Errorf("expected read_file unchanged, got %v", cb["name"])
	}
}

func TestTransformResponseBody(t *testing.T) {
	input := map[string]any{
		"id":    "msg_01",
		"model": "claude-opus-4-6",
		"content": []any{
			map[string]any{"type": "text", "text": "hello"},
			map[string]any{"type": "tool_use", "id": "t1", "name": "task_write", "input": map[string]any{}},
			map[string]any{"type": "tool_use", "id": "t2", "name": "bg_output", "input": map[string]any{}},
		},
	}

	body, _ := json.Marshal(input)
	result := TransformResponseBody(body)

	var output map[string]any
	json.Unmarshal(result, &output)

	content := output["content"].([]any)
	if content[1].(map[string]any)["name"] != "todowrite" {
		t.Errorf("expected todowrite, got %v", content[1].(map[string]any)["name"])
	}
	if content[2].(map[string]any)["name"] != "background_output" {
		t.Errorf("expected background_output, got %v", content[2].(map[string]any)["name"])
	}
}
