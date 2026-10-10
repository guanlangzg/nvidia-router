package modelcatalog

import "strings"

// openCodeFreeHint is an operator-maintained starting point for candidates.
// OpenCodeFree's /models response contains no capability metadata, so the hint
// is deliberately corrected by the read-only probe before it is treated as fact.
type openCodeFreeHint struct {
	SupportsVision    bool
	SupportsTools     bool
	SupportsReasoning bool
	ReasoningWire     string
	ReasoningStatus   string
}

func openCodeFreeCapabilityHint(modelID string) openCodeFreeHint {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return openCodeFreeHint{ReasoningStatus: ReasoningStatusUnknown}
	}

	// These are the models already exercised in the live OpenCodeFree matrix.
	// Keep their positive defaults so a newly saved row is immediately useful;
	// subsequent probes can still correct the wire format or capability.
	if id == "deepseek-v4-flash-free" || id == "hy3-free" || id == "nemotron-3-ultra-free" {
		return openCodeFreeHint{
			SupportsTools:     true,
			SupportsReasoning: true,
			ReasoningWire:     "openai",
			ReasoningStatus:   ReasoningStatusInferred,
		}
	}

	// Modern reasoning families exposed by the gateway share the OpenAI-style
	// effort field at the gateway boundary. Closed models may hide the reasoning
	// text, but accepting the effort parameter is still useful for Codex callers.
	for _, prefix := range []string{
		"deepseek-", "glm-", "minimax-", "kimi-", "qwen3.",
		"gemini-3", "gpt-5", "grok-4", "step-",
	} {
		if strings.HasPrefix(id, prefix) {
			return openCodeFreeHint{
				SupportsTools:     true,
				SupportsReasoning: true,
				ReasoningWire:     "openai",
				ReasoningStatus:   ReasoningStatusInferred,
			}
		}
	}

	// Claude-family models are intentionally left for probing: the gateway may
	// accept the request while keeping reasoning entirely hidden.
	return openCodeFreeHint{ReasoningStatus: ReasoningStatusUnknown}
}
