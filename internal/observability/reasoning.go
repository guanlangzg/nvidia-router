package observability

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"nvidia-router/internal/compat"
)

// reasoningWireFieldOrder is the fixed order in which reasoning field names are
// joined into ReasoningWireFields so the value is deterministic across requests.
var reasoningWireFieldOrder = []string{"reasoning_effort", "reasoning", "thinking"}

// ReasoningFieldsFromBody inspects only the top-level field names of a marshaled
// upstream body to decide whether reasoning was requested and which field names
// carried that intent. Values are never read or retained; a null-valued
// reasoning field still counts as requested because the client explicitly opted
// into thinking.
func ReasoningFieldsFromBody(body []byte) (requested bool, wireFields string) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return false, ""
	}
	names := make([]string, 0, len(reasoningWireFieldOrder))
	for _, name := range reasoningWireFieldOrder {
		if _, ok := fields[name]; ok {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return false, ""
	}
	return true, strings.Join(names, ",")
}

// ReasoningMetadataFromBody extracts the reasoning-level and wire-field-name
// metadata of a marshaled upstream body in a single pass. It is the hot-path
// replacement for calling ReasoningLevelFromBody plus ReasoningFieldsFromBody on
// the same bytes, which performed two full-body unmarshals (large image payloads
// amplified the cost). Semantics are identical to the two functions it merges.
func ReasoningMetadataFromBody(body []byte) (effectiveLevel string, requested bool, wireFields string) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return "", false, ""
	}
	names := make([]string, 0, len(reasoningWireFieldOrder))
	for _, name := range reasoningWireFieldOrder {
		if _, ok := fields[name]; ok {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "", false, ""
	}
	wireFields = strings.Join(names, ",")
	spec, err := compat.ParseReasoning(fields)
	if err != nil {
		return "", true, wireFields
	}
	if spec.Requested && spec.Level != "" {
		return string(spec.Level), true, wireFields
	}
	return "", true, wireFields
}

// ReasoningLevelFromBody extracts only the normalized reasoning level from a
// request body. It deliberately discards all reasoning values and text.
// Prefer ReasoningMetadataFromBody when the same bytes also need wire-field
// names; this function remains for callers that only need the level.
func ReasoningLevelFromBody(body []byte) (string, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return "", false
	}
	spec, err := compat.ParseReasoning(fields)
	if err != nil || !spec.Requested || spec.Level == "" {
		return "", false
	}
	return string(spec.Level), true
}

// ReasoningContentFromBody reports whether a non-stream chat body carries
// assistant reasoning_content, reasoning, or thinking and its total character count (runes).
// Only the length is returned; the reasoning text itself is never retained.
func ReasoningContentFromBody(body []byte) (present bool, chars int64) {
	var chat struct {
		Choices []struct {
			Message struct {
				ReasoningContent json.RawMessage `json:"reasoning_content"`
				Reasoning        json.RawMessage `json:"reasoning"`
				Thinking         json.RawMessage `json:"thinking"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &chat); err != nil {
		return false, 0
	}
	for _, choice := range chat.Choices {
		length, ok := firstReasoningLength(choice.Message.ReasoningContent, choice.Message.Reasoning, choice.Message.Thinking)
		if !ok {
			continue
		}
		chars += length
		present = true
	}
	return present, chars
}

// ReasoningStarvedFromBody reports whether a non-stream chat body is the
// pathological shape where reasoning consumed the whole completion allowance:
// empty assistant content, reasoning present, and the upstream stopping on the
// token limit. The response is protocol-valid, so nothing else flags it, yet the
// caller receives an empty answer.
func ReasoningStarvedFromBody(body []byte) bool {
	var chat struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent json.RawMessage `json:"reasoning_content"`
				Reasoning        json.RawMessage `json:"reasoning"`
				Thinking         json.RawMessage `json:"thinking"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &chat); err != nil {
		return false
	}
	for _, choice := range chat.Choices {
		if choice.FinishReason != "length" {
			continue
		}
		if _, ok := firstReasoningLength(choice.Message.ReasoningContent, choice.Message.Reasoning, choice.Message.Thinking); !ok {
			continue
		}
		if length, ok := firstReasoningLength(choice.Message.Content); ok && length > 0 {
			continue
		}
		return true
	}
	return false
}

// ReasoningDeltaChars reports whether a single SSE chat delta frame carries
// reasoning_content, reasoning, or thinking and its character count (runes). Only the length is
// returned; reasoning text is never retained.
func ReasoningDeltaChars(data []byte) (present bool, chars int64) {
	// Fast path: the overwhelming majority of streamed tokens are ordinary text
	// deltas with no reasoning field. A byte scan for the field names avoids a
	// full unmarshal per token. A false positive (content that happens to contain
	// a field-name substring) only falls through to the slow path; it can never
	// misclassify.
	if !hasReasoningField(data) {
		return false, 0
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				ReasoningContent json.RawMessage `json:"reasoning_content"`
				Reasoning        json.RawMessage `json:"reasoning"`
				Thinking         json.RawMessage `json:"thinking"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return false, 0
	}
	for _, choice := range chunk.Choices {
		length, ok := firstReasoningLength(choice.Delta.ReasoningContent, choice.Delta.Reasoning, choice.Delta.Thinking)
		if !ok {
			continue
		}
		chars += length
		present = true
	}
	return present, chars
}

var (
	reasoningContentField = []byte(`"reasoning_content"`)
	reasoningField        = []byte(`"reasoning":`)
	thinkingField         = []byte(`"thinking":`)
)

// hasReasoningField scans a serialized chat delta for any reasoning field name
// at the JSON key position. It is the fast-path gate before the full unmarshal.
func hasReasoningField(data []byte) bool {
	return bytes.Contains(data, reasoningContentField) ||
		bytes.Contains(data, reasoningField) ||
		bytes.Contains(data, thinkingField)
}

func firstReasoningLength(values ...json.RawMessage) (int64, bool) {
	for _, value := range values {
		if length, ok := reasoningContentLength(value); ok {
			return length, true
		}
	}
	return 0, false
}

// reasoningContentLength returns the rune count of a reasoning field
// that may be a string, structured object (thought/text), array of
// text parts, null or absent, and whether any text was present.
func reasoningContentLength(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil && text != "" {
		return int64(utf8.RuneCountInString(text)), true
	}
	var nested struct {
		Thought string `json:"thought"`
		Text    string `json:"text"`
	}
	if json.Unmarshal(raw, &nested) == nil {
		if nested.Thought != "" {
			return int64(utf8.RuneCountInString(nested.Thought)), true
		}
		if nested.Text != "" {
			return int64(utf8.RuneCountInString(nested.Text)), true
		}
	}
	// Some models emit reasoning as [{type:"text",text:"..."}] or
	// [{"text":"..."}]. Sum all text/thought fields.
	var array []struct {
		Thought string `json:"thought"`
		Text    string `json:"text"`
	}
	if json.Unmarshal(raw, &array) == nil && len(array) > 0 {
		var total int64
		found := false
		for _, item := range array {
			if item.Thought != "" {
				total += int64(utf8.RuneCountInString(item.Thought))
				found = true
			} else if item.Text != "" {
				total += int64(utf8.RuneCountInString(item.Text))
				found = true
			}
		}
		if found {
			return total, true
		}
	}
	return 0, false
}
