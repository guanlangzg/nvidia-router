package responses

import (
	"encoding/json"
	"testing"
)

// The Responses surface shares the reasoning parser with Chat, so it inherited
// the same defect: a reasoning parameter that says "off" demanded the reasoning
// capability and produced 501 not_implemented on every non-reasoning model.
func TestParseDoesNotRequireReasoningCapabilityWhenReasoningIsOff(t *testing.T) {
	for _, body := range []string{
		`{"model":"public-chat","input":"hi","reasoning_effort":"none"}`,
		`{"model":"public-chat","input":"hi","reasoning":{"effort":"none"}}`,
		`{"model":"public-chat","input":"hi","thinking":false}`,
	} {
		request, err := Parse([]byte(body))
		if err != nil {
			t.Fatalf("Parse(%s): %v", body, err)
		}
		if request.Requirements().Reasoning {
			t.Errorf("Parse(%s) demands the reasoning capability; reasoning is switched off", body)
		}
	}
}

func TestParseStillRequiresReasoningCapabilityWhenReasoningIsOn(t *testing.T) {
	request, err := Parse([]byte(`{"model":"public-chat","input":"hi","reasoning":{"effort":"high"}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !request.Requirements().Reasoning {
		t.Fatal("reasoning:{effort:high} must demand the reasoning capability")
	}
}

// Reasoning is pass-through: the mapped aliases are forwarded verbatim even to
// a non-reasoning model. An upstream that does not declare the parameter will
// answer for itself; the router no longer strips or rewrites the request.
func TestToChatForwardsReasoningAliasesOnNonReasoningModel(t *testing.T) {
	for _, testCase := range []struct{ body, alias, want string }{
		{`{"model":"public-chat","input":"hi","reasoning_effort":"none"}`, "reasoning_effort", `"none"`},
		{`{"model":"public-chat","input":"hi","reasoning":{"effort":"none"}}`, "reasoning_effort", `"none"`},
		{`{"model":"public-chat","input":"hi","thinking":false}`, "thinking", `false`},
	} {
		encoded, err := ToChat([]byte(testCase.body), nonReasoningModel())
		if err != nil {
			t.Fatalf("ToChat(%s): %v", testCase.body, err)
		}
		var chat map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &chat); err != nil {
			t.Fatalf("decode body: %v; got=%s", err, encoded)
		}
		if got := string(chat[testCase.alias]); got != testCase.want {
			t.Errorf("ToChat(%s) forwarded %q as %s, want %s", testCase.body, testCase.alias, got, testCase.want)
		}
		if _, present := chat["messages"]; !present {
			t.Errorf("ToChat(%s) lost the messages field: %s", testCase.body, encoded)
		}
	}
}
