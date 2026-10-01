package compat

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestNormalizeToolDefinitionsAcceptsChatAndResponsesShapes(t *testing.T) {
	nested := []byte(`[{"type":"function","function":{"name":"lookup","description":"Look up","parameters":{"type":"object"},"strict":true}}]`)
	flat := []byte(`[{"type":"function","name":"lookup","description":"Look up","parameters":{"type":"object"},"strict":true}]`)

	want, err := NormalizeTools(nested, ToolFormatChat, "tools")
	if err != nil {
		t.Fatalf("NormalizeTools nested: %v", err)
	}
	got, err := NormalizeTools(flat, ToolFormatResponses, "tools")
	if err != nil {
		t.Fatalf("NormalizeTools flat: %v", err)
	}
	if len(want) != 1 || len(got) != 1 || !reflect.DeepEqual(want[0], got[0]) {
		t.Fatalf("normalized tools differ: nested=%#v flat=%#v", want, got)
	}
}

func TestFlattenToolOutputUsesSafeCompatibleText(t *testing.T) {
	raw := []byte(`[{"type":"output_text","text":"first"},{"type":"input_image","image_url":{"url":"https://example.test/x.png"}},{"type":"unknown_part","value":7}]`)
	got, err := FlattenToolOutput(raw, "output")
	if err != nil {
		t.Fatalf("FlattenToolOutput: %v", err)
	}
	want := "first\n\n[image omitted: unsupported by upstream]\n\n{\"type\":\"unknown_part\",\"value\":7}"
	if got != want {
		t.Fatalf("flattened output = %q, want %q", got, want)
	}
}

func TestParseReasoningRejectsConflictingAliases(t *testing.T) {
	_, err := ParseReasoning(map[string]json.RawMessage{
		"reasoning_effort": json.RawMessage(`"low"`),
		"thinking":         json.RawMessage(`{"type":"enabled","budget_tokens":8192}`),
	})
	if !errors.Is(err, ErrAmbiguousReasoning) {
		t.Fatalf("error = %v, want ErrAmbiguousReasoning", err)
	}
}

func TestToolCallAccumulatorPairsStreamingArgumentsByIndex(t *testing.T) {
	var accumulator ToolCallAccumulator
	for _, delta := range []ToolCallDelta{
		{Index: 1, ID: "call-2", Name: "send", Arguments: `{"b":`},
		{Index: 0, ID: "call-1", Name: "lookup", Arguments: `{"a":`},
		{Index: 1, Arguments: `2}`},
		{Index: 0, Arguments: `1}`},
	} {
		if err := accumulator.Add(delta); err != nil {
			t.Fatalf("Add(%+v): %v", delta, err)
		}
	}
	got, err := accumulator.Calls()
	if err != nil {
		t.Fatalf("Calls: %v", err)
	}
	if len(got) != 2 || got[0].ID != "call-1" || got[0].Arguments != `{"a":1}` || got[1].ID != "call-2" || got[1].Arguments != `{"b":2}` {
		t.Fatalf("calls = %#v", got)
	}
}
