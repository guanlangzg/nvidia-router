package opencodefree

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAggregateSseCompletion(t *testing.T) {
	sseData := `
data: {"id":"gen-123","created":1791539688,"model":"step-5-preview-free","choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":"Hel"}}]}

data: {"id":"gen-123","created":1791539688,"model":"step-5-preview-free","choices":[{"index":0,"finish_reason":null,"delta":{"content":"lo, "}}]}

data: {"id":"gen-123","created":1791539688,"model":"step-5-preview-free","choices":[{"index":0,"finish_reason":null,"delta":{"reasoning_content":"thought..."}}]}

data: {"id":"gen-123","created":1791539688,"model":"step-5-preview-free","choices":[{"index":0,"finish_reason":"stop","delta":{"content":"world!"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

data: [DONE]
`

	result, err := aggregateSseCompletion(strings.NewReader(sseData))
	if err != nil {
		t.Fatalf("aggregateSseCompletion failed: %v", err)
	}

	var parsed struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("unmarshal aggregate output: %v, raw: %s", err, string(result))
	}

	if parsed.ID != "gen-123" {
		t.Errorf("id = %q, want gen-123", parsed.ID)
	}
	if parsed.Model != "step-5-preview-free" {
		t.Errorf("model = %q, want step-5-preview-free", parsed.Model)
	}
	if len(parsed.Choices) != 1 {
		t.Fatalf("choices len = %d, want 1", len(parsed.Choices))
	}
	if parsed.Choices[0].Message.Content != "Hello, world!" {
		t.Errorf("content = %q, want Hello, world!", parsed.Choices[0].Message.Content)
	}
	if parsed.Choices[0].Message.ReasoningContent != "thought..." {
		t.Errorf("reasoning = %q, want thought...", parsed.Choices[0].Message.ReasoningContent)
	}
	if parsed.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", parsed.Choices[0].FinishReason)
	}
	if parsed.Usage.TotalTokens != 15 {
		t.Errorf("total_tokens = %d, want 15", parsed.Usage.TotalTokens)
	}
}

func TestAggregateSseCompletionWithToolCalls(t *testing.T) {
	sseData := `
data: {"id":"gen-tool","created":1791539688,"model":"step-5-preview-free","choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"pa"}}]}}]}

data: {"id":"gen-tool","created":1791539688,"model":"step-5-preview-free","choices":[{"index":0,"finish_reason":null,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"calc.py\"}"}}]}}]}

data: {"id":"gen-tool","created":1791539688,"model":"step-5-preview-free","choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}

data: [DONE]
`

	result, err := aggregateSseCompletion(strings.NewReader(sseData))
	if err != nil {
		t.Fatalf("aggregateSseCompletion failed: %v", err)
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("unmarshal aggregate output: %v, raw: %s", err, string(result))
	}

	if len(parsed.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls len = %d, want 1", len(parsed.Choices[0].Message.ToolCalls))
	}
	tc := parsed.Choices[0].Message.ToolCalls[0]
	if tc.ID != "call_1" {
		t.Errorf("tool_call id = %q, want call_1", tc.ID)
	}
	if tc.Function.Name != "read_file" {
		t.Errorf("tool_call name = %q, want read_file", tc.Function.Name)
	}
	if tc.Function.Arguments != `{"path":"calc.py"}` {
		t.Errorf("tool_call arguments = %q, want %q", tc.Function.Arguments, `{"path":"calc.py"}`)
	}
		if parsed.Choices[0].FinishReason != "tool_calls" {
			t.Errorf("finish_reason = %q, want tool_calls", parsed.Choices[0].FinishReason)
		}
	}

	func TestAggregateSseCompletionUsageMemoryIsolation(t *testing.T) {
		// Verify that scanner buffer reuse in subsequent frames cannot corrupt usage data
		sseData := "data: {\"id\":\"gen-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n" +
			"data: {\"id\":\"gen-1\",\"choices\":[{\"index\":0,\"finish_reason\":\"stop\",\"delta\":{\"content\":\"!\"}}]}\n\n" +
			"data: [DONE]\n\n"

		result, err := aggregateSseCompletion(strings.NewReader(sseData))
		if err != nil {
			t.Fatalf("aggregateSseCompletion failed: %v", err)
		}

		var parsed struct {
			Usage struct {
				TotalTokens int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(result, &parsed); err != nil {
			t.Fatalf("unmarshal aggregate output: %v", err)
		}
		if parsed.Usage.TotalTokens != 15 {
			t.Errorf("total_tokens = %d, want 15", parsed.Usage.TotalTokens)
		}
	}
