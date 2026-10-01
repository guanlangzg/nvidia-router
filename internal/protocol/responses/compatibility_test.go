package responses

import (
	"encoding/json"
	"testing"

	"nvidia-router/internal/modelcatalog"
)

func TestResponsesRequestReportsCapabilityRequirements(t *testing.T) {
	request, err := Parse([]byte(`{"model":"public-chat","input":"use lookup","tools":[{"type":"function","name":"lookup"}],"reasoning":{"effort":"high"}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := request.Requirements()
	want := modelcatalog.Requirements{Kind: modelcatalog.KindChat, Tools: true, Reasoning: true}
	if got != want {
		t.Fatalf("requirements = %+v, want %+v", got, want)
	}
}

func TestResponsesAcceptsReasoningCompatibilityAliases(t *testing.T) {
	for _, body := range []string{
		`{"model":"public-chat","input":"think","reasoning":"high"}`,
		`{"model":"public-chat","input":"think","reasoning":{"budget_tokens":8192}}`,
	} {
		t.Run(body, func(t *testing.T) {
			if _, err := Parse([]byte(body)); err != nil {
				t.Fatalf("Parse(%s): %v", body, err)
			}
		})
	}
}

func TestResponsesFlattensCompatibleToolOutputParts(t *testing.T) {
	body := `{"model":"public-chat","input":[{"type":"function_call","name":"lookup","arguments":"{}","call_id":"fc_1"},{"type":"function_call_output","call_id":"fc_1","output":[{"type":"output_text","text":"sunny"},{"type":"input_image"},{"type":"future_part","value":1}]}]}`
	got, err := ToChat([]byte(body), chatModel())
	if err != nil {
		t.Fatalf("ToChat: %v", err)
	}
	var chat struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &chat); err != nil {
		t.Fatalf("decode Chat body: %v", err)
	}
	if len(chat.Messages) != 2 || chat.Messages[1].Role != "tool" || chat.Messages[1].Content != "sunny\n\n[image omitted: unsupported by upstream]\n\n{\"type\":\"future_part\",\"value\":1}" {
		t.Fatalf("messages = %#v", chat.Messages)
	}
}

func TestResponsesAcceptsNullUnsupportedToolExtensions(t *testing.T) {
	body := `{"model":"public-chat","input":"lookup","tools":[{"type":"function","name":"lookup","allowed_callers":null,"defer_loading":null,"output_schema":null}]}`
	if _, err := ToChat([]byte(body), chatModel()); err != nil {
		t.Fatalf("ToChat: %v", err)
	}
}

// Reasoning aliases are pass-through: conflicting values are the upstream's
// verdict, so Parse accepts them and the mapped chat body keeps both fields.
func TestResponsesAcceptsConflictingReasoningAliases(t *testing.T) {
	request, err := Parse([]byte(`{"model":"public-chat","input":"think","reasoning_effort":"low","reasoning":{"effort":"high"}}`))
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
	if got := string(fields["reasoning_effort"]); got != `"high"` {
		t.Fatalf("reasoning_effort = %s, want the reasoning.effort mapping forwarded verbatim", got)
	}
}

// The Responses reasoning parameter maps to the chat field name only; its value
// is never rewritten to match the model profile.
func TestResponsesForwardsReasoningVerbatimRegardlessOfProfile(t *testing.T) {
	request, err := Parse([]byte(`{"model":"public-chat","input":"think","reasoning":{"effort":"high"}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	model := chatModel()
	model.ReasoningWireFormat = "thinking"
	model.ReasoningLevels = []string{"low", "medium"}
	model.ReasoningMaxBudget = 8192
	model.ReasoningZeroAllowed = true
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

func TestFromChatNormalizesToolArgumentsAndReasoningAliases(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"done","thinking":"deep","tool_calls":[{"type":"function","function":{"name":"lookup","arguments":{"city":"NYC"}}}]}}]}`)
	got, err := FromChat(body, "resp_1", chatModel())
	if err != nil {
		t.Fatalf("FromChat: %v", err)
	}
	var response struct {
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Arguments string `json:"arguments"`
			Summary   []struct {
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"output"`
	}
	if err := json.Unmarshal(got, &response); err != nil {
		t.Fatalf("decode Responses body: %v", err)
	}
	if len(response.Output) != 3 || response.Output[0].Type != "reasoning" || response.Output[0].Summary[0].Text != "deep" || response.Output[1].Type != "function_call" || response.Output[1].CallID == "" || response.Output[1].Arguments != `{"city":"NYC"}` {
		t.Fatalf("output = %#v", response.Output)
	}
}

func TestParseChatDeltaAcceptsThinkingReasoningAlias(t *testing.T) {
	delta, done, err := ParseChatDelta([]byte(`{"choices":[{"delta":{"thinking":"step"}}]}`))
	if err != nil || done || delta.Reasoning != "step" {
		t.Fatalf("delta = %#v done=%v err=%v", delta, done, err)
	}
}
