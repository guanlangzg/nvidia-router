package chat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"nvidia-router/internal/compat"
	"nvidia-router/internal/modelcatalog"
)

const MaxRequestBytes = 32 << 20

type Request struct {
	fields        map[string]json.RawMessage
	publicModel   string
	stream        bool
	requirements  modelcatalog.Requirements
	tools         []compat.ToolDefinition
	toolChoice    compat.ToolChoice
	toolChoiceSet bool
	reasoning     compat.ReasoningSpec
	raw           []byte
	// messagesNormalized records that Parse rewrote the message array (legacy
	// function_call/call_id shapes, object tool arguments, array tool results).
	// The raw payload no longer represents the request, so the fast path below
	// must not hand it to the upstream.
	messagesNormalized bool
	// rawUnsafe records that the request has duplicate top-level keys. The fast
	// path must not forward raw bytes in this state because the validated view
	// (fields map) naturally deduplicates, meaning "validated bytes" and
	// "forwarded bytes" would diverge — the upstream could receive duplicate
	// keys that the router never saw, potentially causing 422 on strict schemas.
	rawUnsafe bool
}

func Parse(payload []byte) (Request, error) {
	if len(payload) > MaxRequestBytes {
		return Request{}, invalidRequest("request_too_large", "body", "The request body exceeds the 32 MiB limit.")
	}
	fields, err := decodeFields(payload)
	if err != nil {
		return Request{}, err
	}
	// Detect duplicate top-level keys early. The fast path below forwards raw
	// bytes directly to the upstream; if those bytes contain duplicate keys that
	// decodeFields silently collapsed into the fields map, the router validates
	// one view of the request but sends a different one — the upstream may 422
	// on duplicate keys while the router never saw them. Mark such requests as
	// rawUnsafe so the fast path rebuilds from fields instead.
	rawUnsafe := hasDuplicateTopLevelKeys(payload)
	if err := rejectUnsupported(fields); err != nil {
		return Request{}, err
	}
	modelID, err := requiredModel(fields)
	if err != nil {
		return Request{}, err
	}
	vision, messageTools, err := validateMessages(fields)
	if err != nil {
		return Request{}, err
	}
	messagesNormalized := needsChatMessageNormalization(fields["messages"])
	if messagesNormalized {
		normalizedMessages, err := normalizeChatMessages(fields["messages"])
		if err != nil {
			return Request{}, err
		}
		fields["messages"] = normalizedMessages
	}
	stream, err := optionalStream(fields)
	if err != nil {
		return Request{}, err
	}
	tools, err := validateTools(fields)
	if err != nil {
		return Request{}, err
	}
	_, err = validateToolChoice(fields)
	if err != nil {
		return Request{}, err
	}
	normalizedTools, err := compat.NormalizeTools(fields["tools"], compat.ToolFormatChat, "tools")
	if err != nil {
		return Request{}, compatRequestError(err)
	}
	normalizedChoice, err := compat.NormalizeToolChoice(fields["tool_choice"], compat.ToolNames(normalizedTools), "tool_choice")
	if err != nil {
		return Request{}, compatRequestError(err)
	}
	reasoning, err := compat.ParseReasoning(fields)
	if err != nil {
		// Pass-through: the reasoning aliases are forwarded verbatim, so a
		// malformed or conflicting pair is the upstream's verdict, not ours.
		// The parse result feeds observability and the Requirements flag only.
		reasoning = compat.ReasoningSpec{}
	}
	if err := validateTokenLimits(fields); err != nil {
		return Request{}, err
	}
	if err := validateSampling(fields); err != nil {
		return Request{}, err
	}
	return Request{
		fields: fields, publicModel: modelID, stream: stream,
		requirements: modelcatalog.Requirements{
			Kind: modelcatalog.KindChat, Vision: vision, Tools: messageTools || tools || len(normalizedTools) > 0 || normalizedChoice.Mode == "function" || normalizedChoice.Mode == "required", Reasoning: reasoning.RequiresReasoning(),
		},
		tools: normalizedTools, toolChoice: normalizedChoice,
		toolChoiceSet: fields["tool_choice"] != nil, reasoning: reasoning,
		raw: payload, messagesNormalized: messagesNormalized, rawUnsafe: rawUnsafe,
	}, nil
}

func needsChatMessageNormalization(raw json.RawMessage) bool {
	for _, key := range []string{`"tool_calls"`, `"function_call"`, `"tool_call_id"`, `"call_id"`, `"function"`} {
		if bytes.Contains(raw, []byte(key)) {
			return true
		}
	}
	return false
}

func (r Request) PublicModelID() string {
	return r.publicModel
}

func (r Request) Stream() bool {
	return r.stream
}

func (r Request) Requirements() modelcatalog.Requirements {
	return r.requirements
}

// RequestedReasoningLevel returns the normalized reasoning level the caller
// asked for, or "" when reasoning was not requested. It is the zero-cost
// equivalent of re-parsing the original payload with ReasoningLevelFromBody:
// Parse already ran compat.ParseReasoning on the request fields.
func (r Request) RequestedReasoningLevel() string {
	if r.reasoning.Requested && r.reasoning.Level != "" {
		return string(r.reasoning.Level)
	}
	return ""
}

func (r Request) ReasoningRequested() bool {
	return r.reasoning.Requested
}

func (r Request) MarshalFor(model modelcatalog.Model) ([]byte, error) {
	return r.MarshalForWithOptions(model)
}

// MarshalForWithOptions renders the upstream payload. Reasoning aliases are
// pass-through: whatever the client sent reaches the upstream untouched, so the
// per-model reasoning profile is the operator's advisory metadata, never a
// rewrite authority.
func (r Request) MarshalForWithOptions(model modelcatalog.Model) ([]byte, error) {
	if err := validateModel(r, model); err != nil {
		return nil, err
	}
	// Fast path: 80% of requests have no tool mutation and the upstream model
	// equals the public model — reuse the original payload without
	// clone+sort+marshal. Reasoning aliases need no rewrite, so they never
	// block this path.
	if model.UpstreamID == r.publicModel && len(r.tools) == 0 && !r.toolChoiceSet && !r.messagesNormalized &&
		!r.rawUnsafe {
		if _, hasComp := r.fields["max_completion_tokens"]; !hasComp || isJSONNull(r.fields["max_completion_tokens"]) {
			if _, hasTokens := r.fields["max_tokens"]; hasTokens && isJSONNull(r.fields["max_tokens"]) {
				// max_tokens is null — still needs normalization, fall through
			} else {
				// No field needs rewriting; return original bytes directly.
				// The payload was already validated as JSON object by Parse.
				return r.raw, nil
			}
		}
	}
	fields := cloneFields(r.fields)
	mappedModel, err := json.Marshal(model.UpstreamID)
	if err != nil {
		return nil, fmt.Errorf("marshal upstream model ID: %w", err)
	}
	fields["model"] = mappedModel
	if len(r.tools) > 0 {
		encodedTools, err := compat.MarshalTools(r.tools, compat.ToolFormatChat)
		if err != nil {
			return nil, fmt.Errorf("marshal normalized chat tools: %w", err)
		}
		fields["tools"] = encodedTools
	}
	if r.toolChoiceSet {
		encodedChoice, err := compat.MarshalToolChoice(r.toolChoice)
		if err != nil {
			return nil, fmt.Errorf("marshal normalized chat tool choice: %w", err)
		}
		fields["tool_choice"] = encodedChoice
	}

	// Normalize max_completion_tokens to max_tokens for upstreams (like NVIDIA NIM)
	// that only accept max_tokens, and remove max_completion_tokens so strict upstream
	// schema validation does not fail with 422 extra fields not permitted.
	if maxComp, ok := fields["max_completion_tokens"]; ok && !isJSONNull(maxComp) {
		if maxTokens, hasMaxTokens := fields["max_tokens"]; !hasMaxTokens || isJSONNull(maxTokens) {
			fields["max_tokens"] = maxComp
		}
		delete(fields, "max_completion_tokens")
	}

	return marshalFields(fields)
}

func validateTokenLimits(fields map[string]json.RawMessage) error {
	if raw, ok := fields["max_tokens"]; ok && !isJSONNull(raw) {
		var maxTokens *int
		if json.Unmarshal(raw, &maxTokens) != nil || maxTokens == nil || *maxTokens <= 0 {
			return invalidRequest("invalid_parameter", "max_tokens", "The max_tokens parameter must be a positive integer.")
		}
	}
	if raw, ok := fields["max_completion_tokens"]; ok && !isJSONNull(raw) {
		var maxComp *int
		if json.Unmarshal(raw, &maxComp) != nil || maxComp == nil || *maxComp <= 0 {
			return invalidRequest("invalid_parameter", "max_completion_tokens", "The max_completion_tokens parameter must be a positive integer.")
		}
	}
	return nil
}

func validateSampling(fields map[string]json.RawMessage) error {
	if raw, ok := fields["temperature"]; ok && !isJSONNull(raw) {
		var temp *float64
		if json.Unmarshal(raw, &temp) != nil || temp == nil || *temp < 0 || *temp > 2 {
			return invalidRequest("invalid_parameter", "temperature", "The temperature parameter must be between 0 and 2.")
		}
	}
	if raw, ok := fields["top_p"]; ok && !isJSONNull(raw) {
		var topP *float64
		if json.Unmarshal(raw, &topP) != nil || topP == nil || *topP < 0 || *topP > 1 {
			return invalidRequest("invalid_parameter", "top_p", "The top_p parameter must be between 0 and 1.")
		}
	}
	if raw, ok := fields["n"]; ok && !isJSONNull(raw) {
		var n *int
		if json.Unmarshal(raw, &n) != nil || n == nil || *n <= 0 {
			return invalidRequest("invalid_parameter", "n", "The n parameter must be a positive integer.")
		}
	}
	if raw, ok := fields["presence_penalty"]; ok && !isJSONNull(raw) {
		var pp *float64
		if json.Unmarshal(raw, &pp) != nil || pp == nil || *pp < -2 || *pp > 2 {
			return invalidRequest("invalid_parameter", "presence_penalty", "The presence_penalty parameter must be between -2 and 2.")
		}
	}
	if raw, ok := fields["frequency_penalty"]; ok && !isJSONNull(raw) {
		var fp *float64
		if json.Unmarshal(raw, &fp) != nil || fp == nil || *fp < -2 || *fp > 2 {
			return invalidRequest("invalid_parameter", "frequency_penalty", "The frequency_penalty parameter must be between -2 and 2.")
		}
	}
	if raw, ok := fields["stream_options"]; ok && !isJSONNull(raw) {
		var options map[string]json.RawMessage
		if json.Unmarshal(raw, &options) != nil || options == nil {
			return invalidRequest("invalid_parameter", "stream_options", "The stream_options parameter must be an object.")
		}
		if rawUsage, hasUsage := options["include_usage"]; hasUsage && !isJSONNull(rawUsage) {
			var includeUsage *bool
			if json.Unmarshal(rawUsage, &includeUsage) != nil || includeUsage == nil {
				return invalidRequest("invalid_parameter", "stream_options.include_usage", "include_usage must be a boolean.")
			}
		}
	}
	return nil
}

func isJSONNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func decodeFields(payload []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil || fields == nil {
		return nil, invalidRequest("invalid_json", "", "The request body must be a JSON object.")
	}
	return fields, nil
}

func requiredModel(fields map[string]json.RawMessage) (string, error) {
	raw, ok := fields["model"]
	if !ok {
		return "", invalidRequest("missing_required_parameter", "model", "The model parameter is required.")
	}
	var modelID string
	if json.Unmarshal(raw, &modelID) != nil || modelID == "" || strings.TrimSpace(modelID) != modelID {
		return "", invalidRequest("invalid_parameter", "model", "The model parameter must be a non-empty string.")
	}
	return modelID, nil
}

func cloneFields(fields map[string]json.RawMessage) map[string]json.RawMessage {
	cloned := make(map[string]json.RawMessage, len(fields))
	for name, raw := range fields {
		cloned[name] = raw
	}
	return cloned
}

func marshalFields(fields map[string]json.RawMessage) ([]byte, error) {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	var output bytes.Buffer
	output.WriteByte('{')
	for index, name := range names {
		if index > 0 {
			output.WriteByte(',')
		}
		encodedName, _ := json.Marshal(name)
		output.Write(encodedName)
		output.WriteByte(':')
		output.Write(fields[name])
	}
	output.WriteByte('}')
	if !json.Valid(output.Bytes()) {
		return nil, fmt.Errorf("marshal chat request: invalid raw JSON")
	}
	return output.Bytes(), nil
}
