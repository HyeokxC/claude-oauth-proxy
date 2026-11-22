package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func protocolBody(t *testing.T, input string) map[string]any {
	t.Helper()
	result := TransformRequestBody([]byte(input))
	var body map[string]any
	if err := json.Unmarshal(result, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func protocolBlocks(t *testing.T, body map[string]any) []any {
	t.Helper()
	blocks, ok := body["system"].([]any)
	if !ok {
		t.Fatalf("system must be ordered text blocks, got %T", body["system"])
	}
	return blocks
}

func TestProtocolBody_billingFingerprint_whenShortFirstUserText(t *testing.T) {
	// Given: a short synthetic user text has no code units at offsets 4, 7, 20.
	input := `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"OK"}]}`

	// When: the Opus profile transforms the request.
	blocks := protocolBlocks(t, protocolBody(t, input))

	// Then: the first block has the independently supplied billing hash vector.
	if len(blocks) != 2 {
		t.Fatalf("system blocks = %d, want billing and identity", len(blocks))
	}
	text := blocks[0].(map[string]any)["text"].(string)
	want := "x-anthropic-billing-header: cc_version=2.1.330.d01; cc_entrypoint=sdk-cli;"
	if text != want || len(text) != 74 {
		t.Errorf("billing contract: got %q (%d bytes), want %q (74 bytes)", text, len(text), want)
	}
	sum := sha256.Sum256([]byte(text))
	if hex.EncodeToString(sum[:]) != "28275d055e5b9121560b088ceb9669418a305dd093da0b0f2d32cda96a632ff1" {
		t.Errorf("billing block hash differs from native vector")
	}
}

func TestProtocolBody_usesUTF16CodeUnits_whenFirstUserTextHasAstralRune(t *testing.T) {
	// Given: the astral rune spans UTF-16 positions 4 and 5; position 7 is y.
	input := `{"model":"claude-opus-5-5","messages":[{"role":"user","content":[{"type":"tool_result","content":"skip"},{"type":"text","text":"abcd😀xy"}]}]}`

	// When: billing is computed from the first non-meta user text.
	blocks := protocolBlocks(t, protocolBody(t, input))

	// Then: the high surrogate becomes U+FFFD when encoded alone for SHA-256.
	text := blocks[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "cc_version=2.1.330.a30;") {
		t.Errorf("UTF-16 billing fingerprint mismatch: %q", text)
	}
}

func TestProtocolBody_preservesFields_whenCallerHasStringSystem(t *testing.T) {
	// Given: caller-provided model, thinking, effort and a string system.
	input := `{"model":"claude-opus-5-5","max_tokens":16,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"system":"caller instruction","messages":[{"role":"user","content":"OK"}]}`

	// When: the request enters the native-compatible Opus profile.
	body := protocolBody(t, input)
	blocks := protocolBlocks(t, body)

	// Then: billing precedes identity and the intact caller system; settings survive.
	if len(blocks) != 3 || blocks[2].(map[string]any)["text"] != "caller instruction" {
		t.Errorf("caller system not preserved after two protocol blocks: %v", blocks)
	}
	if blocks[1].(map[string]any)["text"] == "" || body["model"] != "claude-opus-5-5" || body["max_tokens"] != float64(16) {
		t.Error("identity, model or token setting lost")
	}
	if body["thinking"].(map[string]any)["type"] != "adaptive" || body["output_config"].(map[string]any)["effort"] != "high" {
		t.Error("thinking or effort was dropped")
	}
}

func TestProtocolBody_preservesBlockAttributes_whenCallerHasArraySystem(t *testing.T) {
	// Given: cache-controlled caller system blocks and an already-native identity.
	input := `{"model":"claude-opus-5-5","system":[{"type":"text","text":"You are a Claude agent, built on Anthropic's Claude Agent SDK."},{"type":"text","text":"caller data","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"OK"}]}`

	// When: the body gets its billing block.
	blocks := protocolBlocks(t, protocolBody(t, input))

	// Then: the existing identity is not duplicated and caller attributes survive.
	if len(blocks) != 3 {
		t.Fatalf("system blocks = %d, want billing plus original two", len(blocks))
	}
	last := blocks[2].(map[string]any)
	if last["text"] != "caller data" || last["cache_control"].(map[string]any)["type"] != "ephemeral" {
		t.Errorf("system block attributes lost: %v", last)
	}
}

func TestProtocolBody_preservesCallerText_whenSystemContainsMarkup(t *testing.T) {
	// Given: a caller system block containing literal markup and cache controls.
	input := `{"model":"claude-opus-5-5","system":[{"type":"text","text":"<env>caller data</env>","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"OK"}]}`

	// When: protocol blocks are prepended.
	blocks := protocolBlocks(t, protocolBody(t, input))

	// Then: the caller's original text is not rewritten by compatibility conversion.
	if blocks[2].(map[string]any)["text"] != "<env>caller data</env>" {
		t.Errorf("caller text mutated: %v", blocks[2])
	}
}

func TestProtocolBody_removesOnlyOptionalSkillsCatalog_whenOpusSystemContainsOne(t *testing.T) {
	// Given: a synthetic optional catalog embedded between core caller instructions.
	input := `{"model":"claude-opus-5-5","system":[{"type":"text","text":"before <available_skills>synthetic-optional-content</available_skills> after","cache_control":{"type":"ephemeral"}},{"type":"text","text":"keep <env>core</env>"}],"messages":[{"role":"user","content":"OK"}]}`

	// When: the verified Opus protocol blocks are added.
	blocks := protocolBlocks(t, protocolBody(t, input))

	// Then: only the optional catalog is gone; billing, identity and caller fields survive.
	if len(blocks) != 4 {
		t.Fatalf("system blocks = %d, want protocol two plus original two", len(blocks))
	}
	if !strings.HasPrefix(blocks[0].(map[string]any)["text"].(string), "x-anthropic-billing-header:") || blocks[1].(map[string]any)["text"] == "" {
		t.Error("protocol billing or identity lost")
	}
	first := blocks[2].(map[string]any)
	if first["text"] != "before  after" || first["cache_control"].(map[string]any)["type"] != "ephemeral" {
		t.Errorf("caller system outside optional block mutated: %v", first)
	}
	if blocks[3].(map[string]any)["text"] != "keep <env>core</env>" {
		t.Error("unrelated system block changed")
	}
}

func TestProtocolBody_preservesUserContent_whenUserTextContainsCatalogMarkers(t *testing.T) {
	// Given: catalog-like delimiters in user content rather than system content.
	input := `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"<available_skills>user-owned text</available_skills>"}]}`

	// When: request system blocks are added.
	body := protocolBody(t, input)

	// Then: user messages are not subject to optional-system stripping.
	message := body["messages"].([]any)[0].(map[string]any)
	if message["content"] != "<available_skills>user-owned text</available_skills>" {
		t.Error("user message text was altered")
	}
}

func TestProtocolBody_addsNativeBlocks_whenModelIsFable(t *testing.T) {
	for _, model := range []string{"claude-fable-5-2", "claude-fable-5-1"} {
		t.Run(model, func(t *testing.T) {
			// Given: a Fable request, which upstream rejects without native system blocks.
			input := `{"model":"` + model + `","system":"caller instruction","messages":[{"role":"user","content":"OK"}]}`

			// When: the request body is transformed.
			blocks := protocolBlocks(t, protocolBody(t, input))

			// Then: billing and SDK identity lead the caller's system text.
			if len(blocks) != 3 ||
				blocks[0].(map[string]any)["text"] != "x-anthropic-billing-header: cc_version=2.1.330.d01; cc_entrypoint=sdk-cli;" ||
				blocks[1].(map[string]any)["text"] != sdkIdentity ||
				blocks[2].(map[string]any)["text"] != "caller instruction" {
				t.Fatalf("native blocks missing: %v", blocks)
			}
		})
	}
}
