package observability

import "testing"

func TestReasoningFieldsFromBodyReportsRequestedAndWireNames(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		requested bool
		fields    string
	}{
		{"no reasoning fields", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, false, ""},
		{"single reasoning_effort", `{"reasoning_effort":"high","model":"m"}`, true, "reasoning_effort"},
		{"thinking object", `{"thinking":{"type":"enabled"},"model":"m"}`, true, "thinking"},
		{"reasoning field", `{"reasoning":{"effort":"high"},"model":"m"}`, true, "reasoning"},
		{"fixed deterministic order", `{"thinking":{"type":"disabled"},"reasoning_effort":"low","reasoning":{},"model":"m"}`, true, "reasoning_effort,reasoning,thinking"},
		{"null-valued reasoning still requested", `{"reasoning_effort":null,"model":"m"}`, true, "reasoning_effort"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requested, fields := ReasoningFieldsFromBody([]byte(tc.body))
			if requested != tc.requested || fields != tc.fields {
				t.Fatalf("ReasoningFieldsFromBody = requested %v fields %q, want %v %q", requested, fields, tc.requested, tc.fields)
			}
		})
	}
}

func TestReasoningFieldsFromBodyHandlesInvalidJSON(t *testing.T) {
	requested, fields := ReasoningFieldsFromBody([]byte(`not json`))
	if requested || fields != "" {
		t.Fatalf("invalid body = requested %v fields %q, want false empty", requested, fields)
	}
}

func TestReasoningMetadataFromBodyReportsFieldsWhenValuesAreInvalidOrConflict(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		fields string
	}{
		{"invalid effort", `{"reasoning_effort":42}`, "reasoning_effort"},
		{"conflicting aliases", `{"reasoning_effort":"low","reasoning":{"effort":"high"}}`, "reasoning_effort,reasoning"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			level, requested, fields := ReasoningMetadataFromBody([]byte(tc.body))
			if level != "" || !requested || fields != tc.fields {
				t.Fatalf("ReasoningMetadataFromBody = level %q requested %v fields %q, want empty true %q", level, requested, fields, tc.fields)
			}
		})
	}
}

func TestReasoningLevelFromBodyNormalizesAliases(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		level string
		ok    bool
	}{
		{"openai effort alias", `{"reasoning_effort":"HIGH","private_reasoning":"do not retain"}`, "high", true},
		{"thinking budget", `{"thinking":{"type":"enabled","budget_tokens":8192}}`, "medium", true},
		{"responses reasoning object", `{"reasoning":{"effort":"low"}}`, "low", true},
		{"no reasoning", `{"model":"m"}`, "", false},
		{"invalid json", `not json`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			level, ok := ReasoningLevelFromBody([]byte(tc.body))
			if level != tc.level || ok != tc.ok {
				t.Fatalf("ReasoningLevelFromBody = %q/%v, want %q/%v", level, ok, tc.level, tc.ok)
			}
		})
	}
}

func TestReasoningContentFromBodyCountsCharacters(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		present bool
		chars   int64
	}{
		{"no choices", `{}`, false, 0},
		{"empty reasoning", `{"choices":[{"message":{"reasoning_content":"","content":"ok"}}]}`, false, 0},
		{"null reasoning", `{"choices":[{"message":{"reasoning_content":null}}]}`, false, 0},
		{"ascii reasoning", `{"choices":[{"message":{"reasoning_content":"deep-thought"}}]}`, true, 12},
		{"reasoning alias", `{"choices":[{"message":{"reasoning":"deep"}}]}`, true, 4},
		{"thinking alias", `{"choices":[{"message":{"thinking":"思考"}}]}`, true, 2},
		{"multi-byte runes counted not bytes", `{"choices":[{"message":{"reasoning_content":"思考abc"}}]}`, true, 5},
		{"multiple choices summed", `{"choices":[{"message":{"reasoning_content":"ab"}},{"message":{"reasoning_content":"cd"}}]}`, true, 4},
		{"reasoning field alternative", `{"choices":[{"message":{"reasoning":"thinking_step"}}]}`, true, 13},
		{"thinking object with thought field", `{"choices":[{"message":{"thinking":{"thought":"internal_reasoning"}}}]}`, true, 18},
		{"thinking object with text field", `{"choices":[{"message":{"thinking":{"text":"thought_text"}}}]}`, true, 12},
		{"thinking text array", `{"choices":[{"message":{"reasoning_content":[{"type":"text","text":"step_a"},{"type":"text","text":"step_b"}]}}]}`, true, 12},
		{"thinking plain text array", `{"choices":[{"message":{"thinking":[{"text":"ab"},{"thought":"cd"}]}}]}`, true, 4},
		{"empty text array skipped", `{"choices":[{"message":{"thinking":[]}}]}`, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			present, chars := ReasoningContentFromBody([]byte(tc.body))
			if present != tc.present || chars != tc.chars {
				t.Fatalf("ReasoningContentFromBody = present %v chars %d, want %v %d", present, chars, tc.present, tc.chars)
			}
		})
	}
}

func TestReasoningDeltaCharsParsesStreamChunk(t *testing.T) {
	cases := []struct {
		name    string
		chunk   string
		present bool
		chars   int64
	}{
		{"no reasoning delta", `{"choices":[{"delta":{"content":"hi"}}]}`, false, 0},
		{"reasoning delta", `{"choices":[{"delta":{"reasoning_content":"chain"}}]}`, true, 5},
		{"reasoning alias delta", `{"choices":[{"delta":{"reasoning":"chain"}}]}`, true, 5},
		{"thinking alias delta", `{"choices":[{"delta":{"thinking":"思考"}}]}`, true, 2},
		{"null reasoning delta", `{"choices":[{"delta":{"reasoning_content":null}}]}`, false, 0},
		{"multiple choices summed", `{"choices":[{"delta":{"reasoning_content":"a"}},{"delta":{"reasoning_content":"bc"}}]}`, true, 3},
		{"reasoning field delta", `{"choices":[{"delta":{"reasoning":"step"}}]}`, true, 4},
		{"thinking object delta", `{"choices":[{"delta":{"thinking":{"thought":"thought_delta"}}}]}`, true, 13},
		{"thinking array delta", `{"choices":[{"delta":{"reasoning_content":[{"text":"arr"}]}}]}`, true, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			present, chars := ReasoningDeltaChars([]byte(tc.chunk))
			if present != tc.present || chars != tc.chars {
				t.Fatalf("ReasoningDeltaChars = present %v chars %d, want %v %d", present, chars, tc.present, tc.chars)
			}
		})
	}
}
