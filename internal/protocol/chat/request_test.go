package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"nvidia-router/internal/apierror"
	"nvidia-router/internal/modelcatalog"
)

func TestParsePreservesUnknownFieldsAndMarshalForMapsModel(t *testing.T) {
	payload := []byte(`{
		"model":"public-model",
		"messages":[{"role":"user","content":"hello"}],
		"stream":true,
		"future_config":{"alpha": 1e+09, "nested": [true, null]}
	}`)
	request, err := Parse(payload)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if request.PublicModelID() != "public-model" || !request.Stream() {
		t.Fatalf("request route fields = %q/%v", request.PublicModelID(), request.Stream())
	}
	if got := request.Requirements(); got != (modelcatalog.Requirements{Kind: modelcatalog.KindChat}) {
		t.Fatalf("requirements = %+v", got)
	}

	body, err := request.MarshalFor(chatModel())
	if err != nil {
		t.Fatalf("MarshalFor: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode prepared body: %v", err)
	}
	if got := string(fields["model"]); got != `"vendor/model"` {
		t.Fatalf("mapped model = %s", got)
	}
	wantUnknown := `{"alpha": 1e+09, "nested": [true, null]}`
	if got := string(fields["future_config"]); got != wantUnknown {
		t.Fatalf("future_config = %s, want %s", got, wantUnknown)
	}
}

func TestParseRejectsUnsupportedBeforeMissingRequiredFields(t *testing.T) {
	_, err := Parse([]byte(`{"store":true}`))
	_ = requireRequestError(t, err, "unsupported_parameter", "store")
}

func TestParseValidatesRequiredAndKnownFieldTypes(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		code    string
		param   string
	}{
		{name: "model missing", payload: `{"messages":[]}`, code: "missing_required_parameter", param: "model"},
		{name: "model type", payload: `{"model":7,"messages":[]}`, code: "invalid_parameter", param: "model"},
		{name: "messages missing", payload: `{"model":"m"}`, code: "missing_required_parameter", param: "messages"},
		{name: "messages type", payload: `{"model":"m","messages":{}}`, code: "invalid_parameter", param: "messages"},
		{name: "messages empty", payload: `{"model":"m","messages":[]}`, code: "invalid_parameter", param: "messages"},
		{name: "role missing", payload: `{"model":"m","messages":[{"content":"x"}]}`, code: "missing_required_parameter", param: "messages[0].role"},
		{name: "role invalid", payload: `{"model":"m","messages":[{"role":"moderator","content":"x"}]}`, code: "invalid_parameter", param: "messages[0].role"},
		{name: "stream type", payload: `{"model":"m","messages":[{"role":"user","content":"x"}],"stream":"true"}`, code: "invalid_parameter", param: "stream"},
		{name: "stream null", payload: `{"model":"m","messages":[{"role":"user","content":"x"}],"stream":null}`, code: "invalid_parameter", param: "stream"},
		{name: "store null", payload: `{"model":"m","messages":[{"role":"user","content":"x"}],"store":null}`, code: "invalid_parameter", param: "store"},
		{name: "tools type", payload: `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":{}}`, code: "invalid_parameter", param: "tools"},
		{name: "tools null", payload: `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":null}`, code: "invalid_parameter", param: "tools"},
		{name: "tool type", payload: `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"code"}]}`, code: "invalid_parameter", param: "tools[0].type"},
		{name: "function name", payload: `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{}}]}`, code: "missing_required_parameter", param: "tools[0].function.name"},
		{name: "tool choice type", payload: `{"model":"m","messages":[{"role":"user","content":"x"}],"tool_choice":1}`, code: "invalid_parameter", param: "tool_choice"},
		{name: "tool choice value", payload: `{"model":"m","messages":[{"role":"user","content":"x"}],"tool_choice":"sometimes"}`, code: "invalid_parameter", param: "tool_choice"},
		{name: "max tokens negative", payload: `{"model":"m","messages":[{"role":"user","content":"x"}],"max_tokens":-1}`, code: "invalid_parameter", param: "max_tokens"},
		{name: "max tokens string", payload: `{"model":"m","messages":[{"role":"user","content":"x"}],"max_tokens":"many"}`, code: "invalid_parameter", param: "max_tokens"},
		{name: "max completion tokens zero", payload: `{"model":"m","messages":[{"role":"user","content":"x"}],"max_completion_tokens":0}`, code: "invalid_parameter", param: "max_completion_tokens"},
		{name: "temperature out of range", payload: `{"model":"m","messages":[{"role":"user","content":"x"}],"temperature":2.5}`, code: "invalid_parameter", param: "temperature"},
		{name: "top_p out of range", payload: `{"model":"m","messages":[{"role":"user","content":"x"}],"top_p":1.5}`, code: "invalid_parameter", param: "top_p"},
		{name: "n zero", payload: `{"model":"m","messages":[{"role":"user","content":"x"}],"n":0}`, code: "invalid_parameter", param: "n"},
		{name: "presence penalty out of range", payload: `{"model":"m","messages":[{"role":"user","content":"x"}],"presence_penalty":3}`, code: "invalid_parameter", param: "presence_penalty"},
		{name: "frequency penalty out of range", payload: `{"model":"m","messages":[{"role":"user","content":"x"}],"frequency_penalty":-3}`, code: "invalid_parameter", param: "frequency_penalty"},
		{name: "stream options not object", payload: `{"model":"m","messages":[{"role":"user","content":"x"}],"stream_options":"true"}`, code: "invalid_parameter", param: "stream_options"},
		{name: "include_usage not bool", payload: `{"model":"m","messages":[{"role":"user","content":"x"}],"stream_options":{"include_usage":"yes"}}`, code: "invalid_parameter", param: "stream_options.include_usage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.payload))
			_ = requireRequestError(t, err, tt.code, tt.param)
		})
	}
}

func TestToolChoiceNoneDoesNotRequireToolCapability(t *testing.T) {
	request, err := Parse([]byte(`{
		"model":"public-model",
		"messages":[{"role":"user","content":"hello"}],
		"tool_choice":"none"
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if request.Requirements().Tools {
		t.Fatal("tool_choice none unexpectedly requires tool capability")
	}
	if _, err := request.MarshalFor(chatModel()); err != nil {
		t.Fatalf("MarshalFor plain model: %v", err)
	}
}

func TestParseRejectsDuplicateToolStringsEndingInNull(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		param   string
	}{
		{
			name: "tool type",
			payload: `{"model":"m","messages":[{"role":"user","content":"x"}],` +
				`"tools":[{"type":"function","type":null,"function":{"name":"lookup"}}]}`,
			param: "tools[0].type",
		},
		{
			name: "function name",
			payload: `{"model":"m","messages":[{"role":"user","content":"x"}],` +
				`"tools":[{"type":"function","function":{"name":"lookup","name":null}}]}`,
			param: "tools[0].function.name",
		},
		{
			name: "tool choice type",
			payload: `{"model":"m","messages":[{"role":"user","content":"x"}],` +
				`"tool_choice":{"type":"function","type":null,"function":{"name":"lookup"}}}`,
			param: "tool_choice",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.payload))
			_ = requireRequestError(t, err, "invalid_parameter", tt.param)
		})
	}
}

func TestParseExtractsToolAndReasoningRequirements(t *testing.T) {
	request, err := Parse([]byte(`{
		"model":"public-model",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"lookup"}},
		"reasoning_effort":"high"
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := modelcatalog.Requirements{Kind: modelcatalog.KindChat, Tools: true, Reasoning: true}
	if got := request.Requirements(); got != want {
		t.Fatalf("requirements = %+v, want %+v", got, want)
	}
}

func TestParseExtractsToolRequirementsFromMessages(t *testing.T) {
	for _, payload := range []string{
		`{"model":"public-model","messages":[{"role":"tool","tool_call_id":"call-1","content":"done"}]}`,
		`{"model":"public-model","messages":[{"role":"assistant","content":null,` +
			`"tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}]}`,
	} {
		request, err := Parse([]byte(payload))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if !request.Requirements().Tools {
			t.Fatal("message tool protocol did not require tool capability")
		}
		if _, err := request.MarshalFor(chatModel()); err != nil {
			t.Fatalf("MarshalFor: %v", err)
		}
	}
}

func TestParseRejectsAmbiguousKnownToolKeys(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		param   string
	}{
		{
			name: "type case",
			payload: `{"model":"m","messages":[{"role":"user","content":"x"}],` +
				`"tools":[{"Type":"function","function":{"name":"lookup"}}]}`,
			param: "tools[0].type",
		},
		{
			name: "type duplicate",
			payload: `{"model":"m","messages":[{"role":"user","content":"x"}],` +
				`"tools":[{"type":"bad","type":"function","function":{"name":"lookup"}}]}`,
			param: "tools[0].type",
		},
		{
			name: "name case",
			payload: `{"model":"m","messages":[{"role":"user","content":"x"}],` +
				`"tools":[{"type":"function","function":{"Name":"lookup"}}]}`,
			param: "tools[0].function.name",
		},
		{
			name: "name duplicate",
			payload: `{"model":"m","messages":[{"role":"user","content":"x"}],` +
				`"tools":[{"type":"function","function":{"name":null,"name":"lookup"}}]}`,
			param: "tools[0].function.name",
		},
		{
			name: "tool choice case",
			payload: `{"model":"m","messages":[{"role":"user","content":"x"}],` +
				`"tool_choice":{"Type":"function","function":{"name":"lookup"}}}`,
			param: "tool_choice",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.payload))
			_ = requireRequestError(t, err, "invalid_parameter", tt.param)
		})
	}
}

func TestMarshalForPreservesCapabilitiesForModelsWithoutLocalHints(t *testing.T) {
	request, err := Parse([]byte(`{"model":"public-model","messages":[{"role":"user","content":"use the tool"}],"tools":[{"type":"function","function":{"name":"lookup"}}],"reasoning_effort":"low"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	body, err := request.MarshalFor(chatModel())
	if err != nil {
		t.Fatalf("MarshalFor: %v", err)
	}
	if !bytes.Contains(body, []byte(`"tools"`)) || !bytes.Contains(body, []byte(`"reasoning_effort"`)) {
		t.Fatalf("capability fields were not preserved: %s", body)
	}
}

func TestMarshalForPassesCapabilitiesToUpstream(t *testing.T) {
	request, err := Parse([]byte(`{
		"model":"public-model",
		"messages":[{"role":"user","content":"use the tool"}],
		"tools":[{"type":"function","function":{"name":"lookup"}}],
		"tool_choice":"auto",
		"reasoning_effort":"high",
		"thinking":{"type":"enabled","budget_tokens":24576}
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	body, err := request.MarshalFor(chatModel())
	if err != nil {
		t.Fatalf("MarshalFor: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode prepared body: %v", err)
	}
	if got := string(fields["model"]); got != `"vendor/model"` {
		t.Fatalf("mapped model = %s", got)
	}
	for _, field := range []string{"tools", "tool_choice", "reasoning_effort", "thinking"} {
		if _, ok := fields[field]; !ok {
			t.Fatalf("capability field %q was not preserved", field)
		}
	}
}
func TestMarshalForPreservesNativeThinking(t *testing.T) {
	request, err := Parse([]byte(`{
		"model":"public-model",
		"messages":[{"role":"user","content":"hello"}],
		"thinking":{"type":"enabled","budget_tokens":8192},
		"future_flag":"kept"
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	body, err := request.MarshalFor(chatModel())
	if err != nil {
		t.Fatalf("MarshalFor: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode prepared body: %v", err)
	}
	if _, exists := fields["thinking"]; !exists {
		t.Fatal("native thinking field was removed")
	}
	if _, exists := fields["reasoning_effort"]; exists {
		t.Fatal("reasoning_effort was synthesized")
	}
	if got := string(fields["future_flag"]); got != `"kept"` {
		t.Fatalf("future_flag = %s", got)
	}
}

func TestMarshalForPreservesConflictingReasoningFields(t *testing.T) {
	request, err := Parse([]byte(`{
		"model":"public-model",
		"messages":[{"role":"user","content":"hello"}],
		"reasoning_effort":"low",
		"thinking":{"type":"enabled","budget_tokens":24576}
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	body, err := request.MarshalFor(chatModel())
	if err != nil {
		t.Fatalf("MarshalFor: %v", err)
	}
	if !bytes.Contains(body, []byte(`"reasoning_effort":"low"`)) || !bytes.Contains(body, []byte(`"budget_tokens":24576`)) {
		t.Fatalf("conflicting reasoning aliases were not forwarded verbatim: %s", body)
	}
}

func chatModel() modelcatalog.Model {
	return modelcatalog.Model{
		ID: 1, PublicID: "public-model", UpstreamID: "vendor/model", DisplayName: "Model",
		Kind: modelcatalog.KindChat, Enabled: true,
	}
}

func TestModelMappingPreservesNativeReasoningFields(t *testing.T) {
	request, err := Parse([]byte(`{
		"model":"nvidia/deepseek-ai/deepseek-v4-flash",
		"messages":[{"role":"user","content":"long task"}],
		"thinking":{"type":"enabled","budget_tokens":8192}
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	model := chatModel()
	model.PublicID = "nvidia/deepseek-ai/deepseek-v4-flash"
	model.UpstreamID = "deepseek-ai/deepseek-v4-flash"
	body, err := request.MarshalFor(model)
	if err != nil {
		t.Fatalf("MarshalFor: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode prepared body: %v", err)
	}
	if got := string(fields["model"]); got != `"deepseek-ai/deepseek-v4-flash"` {
		t.Fatalf("upstream model = %s", got)
	}
	if _, exists := fields["thinking"]; !exists {
		t.Fatal("native thinking field was removed")
	}
}

func TestMarshalForNormalizesMaxCompletionTokens(t *testing.T) {
	request, err := Parse([]byte(`{
		"model":"public-model",
		"messages":[{"role":"user","content":"solve this"}],
		"max_completion_tokens":4096
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	body, err := request.MarshalFor(chatModel())
	if err != nil {
		t.Fatalf("MarshalFor: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := string(fields["max_tokens"]); got != "4096" {
		t.Fatalf("max_tokens = %s, want 4096", got)
	}
	if _, exists := fields["max_completion_tokens"]; exists {
		t.Fatal("max_completion_tokens should be stripped from upstream body")
	}
}

func TestMarshalForPreservesExistingMaxTokensWhenMaxCompletionTokensPresent(t *testing.T) {
	request, err := Parse([]byte(`{
		"model":"public-model",
		"messages":[{"role":"user","content":"solve this"}],
		"max_tokens":2048,
		"max_completion_tokens":4096
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	body, err := request.MarshalFor(chatModel())
	if err != nil {
		t.Fatalf("MarshalFor: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := string(fields["max_tokens"]); got != "2048" {
		t.Fatalf("max_tokens = %s, want 2048", got)
	}
	if _, exists := fields["max_completion_tokens"]; exists {
		t.Fatal("max_completion_tokens should be stripped from upstream body")
	}
}

func requireRequestError(t *testing.T, err error, code, param string) *apierror.Error {
	t.Helper()
	var publicError *apierror.Error
	if !errors.As(err, &publicError) {
		t.Fatalf("error = %T %v, want *apierror.Error", err, err)
	}
	if publicError.Status != 400 || publicError.Code != code {
		t.Fatalf("error = %+v, want status 400 code %q", publicError, code)
	}
	if publicError.Param == nil || *publicError.Param != param {
		t.Fatalf("error param = %v, want %q", publicError.Param, param)
	}
	return publicError
}
