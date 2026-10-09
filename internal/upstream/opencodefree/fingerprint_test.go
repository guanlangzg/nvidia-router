package opencodefree

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStripModelPrefix(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"opencodefree/step-5-preview-free", "step-5-preview-free"},
		{"opencode/step-5-preview-free", "step-5-preview-free"},
		{"oc/space-bunny-free", "space-bunny-free"},
		{"OPENCODEFREE/longcat-2.5-preview-free", "longcat-2.5-preview-free"},
		{"step-5-preview-free", "step-5-preview-free"},
	}
	for _, tt := range tests {
		if got := stripModelPrefix(tt.input); got != tt.want {
			t.Errorf("stripModelPrefix(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestNormalizeChatBodyInjectsToolsAndForcesStream(t *testing.T) {
	input := []byte(`{
		"model": "opencodefree/step-5-preview-free",
		"messages": [{"role": "user", "content": "ping"}],
		"stream": false
	}`)

	normalized, err := normalizeChatBody(input)
	if err != nil {
		t.Fatalf("normalizeChatBody: %v", err)
	}

	var parsed struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		Tools  []struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(normalized, &parsed); err != nil {
		t.Fatalf("unmarshal normalized: %v", err)
	}

	if parsed.Model != "step-5-preview-free" {
		t.Errorf("model = %q, want step-5-preview-free", parsed.Model)
	}
	if !parsed.Stream {
		t.Errorf("stream = %v, want true", parsed.Stream)
	}

	names := make(map[string]bool)
	for _, tool := range parsed.Tools {
		names[tool.Function.Name] = true
	}
	for _, want := range []string{"bash", "glob", "grep", "read"} {
		if !names[want] {
			t.Errorf("missing fingerprint tool %q in tools list: %+v", want, parsed.Tools)
		}
	}
}

func TestNormalizeChatBodyPreservesExistingCustomTools(t *testing.T) {
	input := []byte(`{
		"model": "step-5-preview-free",
		"messages": [{"role": "user", "content": "ping"}],
		"tools": [
			{"type": "function", "function": {"name": "custom_tool"}}
		]
	}`)

	normalized, err := normalizeChatBody(input)
	if err != nil {
		t.Fatalf("normalizeChatBody: %v", err)
	}

	if !strings.Contains(string(normalized), `"custom_tool"`) {
		t.Errorf("expected custom_tool preserved, got: %s", string(normalized))
	}
	if !strings.Contains(string(normalized), `"bash"`) {
		t.Errorf("expected bash injected, got: %s", string(normalized))
	}
}
