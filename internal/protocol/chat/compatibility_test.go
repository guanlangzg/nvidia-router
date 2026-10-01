package chat

import (
	"bytes"
	"encoding/json"
	"testing"

	"nvidia-router/internal/apierror"
	"nvidia-router/internal/modelcatalog"
)

func TestParseNormalizesFlatFunctionToolsForChatUpstream(t *testing.T) {
	request, err := Parse([]byte(`{"model":"public-model","messages":[{"role":"user","content":"lookup"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	body, err := request.MarshalFor(modelcatalog.Model{PublicID: "public-model", UpstreamID: "vendor/model", Kind: modelcatalog.KindChat, Enabled: true})
	if err != nil {
		t.Fatalf("MarshalFor: %v", err)
	}
	if !bytes.Contains(body, []byte(`"function":{"name":"lookup"`)) {
		t.Fatalf("flat tool was not normalized: %s", body)
	}
}

// Reasoning aliases are pass-through: conflicting or off-profile values are the
// upstream's problem, so Parse accepts them and MarshalFor forwards them.
func TestParseAcceptsConflictingReasoningAliases(t *testing.T) {
	payload := []byte(`{"model":"public-model","messages":[{"role":"user","content":"think"}],"reasoning_effort":"low","thinking":{"type":"enabled","budget_tokens":8192}}`)
	request, err := Parse(payload)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	body, err := request.MarshalFor(modelcatalog.Model{PublicID: "public-model", UpstreamID: "vendor/model", Kind: modelcatalog.KindChat, Enabled: true})
	if err != nil {
		t.Fatalf("MarshalFor: %v", err)
	}
	if !bytes.Contains(body, []byte(`"reasoning_effort":"low"`)) || !bytes.Contains(body, []byte(`"budget_tokens":8192`)) {
		t.Fatalf("reasoning aliases were not forwarded verbatim: %s", body)
	}
}

func TestMarshalForForwardsReasoningVerbatimRegardlessOfProfile(t *testing.T) {
	request, err := Parse([]byte(`{"model":"public-model","messages":[{"role":"user","content":"think"}],"reasoning_effort":"high"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// The profile disagrees with the request (thinking wire format, no high
	// level) — pass-through means the request still reaches the upstream as-is.
	model := modelcatalog.Model{
		PublicID: "public-model", UpstreamID: "vendor/model", Kind: modelcatalog.KindChat, Enabled: true,
		SupportsReasoning: true, ReasoningWireFormat: "thinking", ReasoningLevels: []string{"low", "medium"},
		ReasoningMaxBudget: 8192, ReasoningZeroAllowed: true,
	}
	body, err := request.MarshalFor(model)
	if err != nil {
		t.Fatalf("MarshalFor: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := string(fields["reasoning_effort"]); got != `"high"` {
		t.Fatalf("reasoning_effort = %s, want the client value forwarded verbatim", got)
	}
	if _, ok := fields["thinking"]; ok {
		t.Fatal("thinking was synthesized from the model profile")
	}
}

func TestMarshalForNormalizesLegacyFunctionCallAndToolResult(t *testing.T) {
	request, err := Parse([]byte(`{"model":"public-model","messages":[{"role":"assistant","function_call":{"name":"lookup","arguments":{"city":"NYC"}}},{"role":"function","name":"lookup","content":[{"type":"output_text","text":"sunny"},{"type":"input_image"},{"type":"future_part","value":1}]}]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	body, err := request.MarshalFor(modelcatalog.Model{PublicID: "public-model", UpstreamID: "vendor/model", Kind: modelcatalog.KindChat, Enabled: true})
	if err != nil {
		t.Fatalf("MarshalFor: %v", err)
	}
	var fields struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			Content    string `json:"content"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(fields.Messages) != 2 || fields.Messages[0].Role != "assistant" || len(fields.Messages[0].ToolCalls) != 1 || fields.Messages[0].ToolCalls[0].Function.Arguments != `{"city":"NYC"}` {
		t.Fatalf("assistant legacy call = %#v", fields.Messages[0])
	}
	if fields.Messages[1].Role != "tool" || fields.Messages[1].ToolCallID != fields.Messages[0].ToolCalls[0].ID || fields.Messages[1].Content != "sunny\n\n[image omitted: unsupported by upstream]\n\n{\"type\":\"future_part\",\"value\":1}" {
		t.Fatalf("tool result = %#v", fields.Messages[1])
	}
}

func containsChatError(err error, code, param string) bool {
	public, ok := err.(*apierror.Error)
	if !ok || public.Code != code || public.Param == nil || *public.Param != param {
		return false
	}
	return true
}

// The fast path returns the original bytes verbatim. Parse rewrites legacy
// message shapes into fields["messages"], so taking that path after a rewrite
// would ship the un-normalised payload and the upstream would reject it.
func TestFastPathDoesNotDiscardMessageNormalization(t *testing.T) {
	payload := []byte(`{"model":"vendor/model","messages":[` +
		`{"role":"user","content":"weather?"},` +
		`{"role":"assistant","function_call":{"name":"lookup","arguments":{"city":"NYC"}}},` +
		`{"role":"function","name":"lookup","content":[{"type":"output_text","text":"sunny"}]}]}`)
	request, err := Parse(payload)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// PublicID == UpstreamID is the normal NVIDIA shape and is exactly what arms
	// the fast path.
	encoded, err := request.MarshalFor(modelcatalog.Model{PublicID: "vendor/model", UpstreamID: "vendor/model", Kind: modelcatalog.KindChat, Enabled: true})
	if err != nil {
		t.Fatalf("MarshalFor: %v", err)
	}
	if bytes.Contains(encoded, []byte(`"function_call"`)) {
		t.Fatalf("legacy function_call survived normalization: %s", encoded)
	}
	if !bytes.Contains(encoded, []byte(`"tool_calls"`)) {
		t.Fatalf("normalized tool_calls missing: %s", encoded)
	}
	if !bytes.Contains(encoded, []byte(`"tool_call_id"`)) {
		t.Fatalf("normalized tool_call_id missing: %s", encoded)
	}
}
