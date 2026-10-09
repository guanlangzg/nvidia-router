package opencodefree

import (
	"encoding/json"
	"fmt"
	"strings"
)

var defaultFingerprintTools = []map[string]any{
	{
		"type": "function",
		"function": map[string]any{
			"name":        "bash",
			"description": "Built-in bash tool provided by the OpenCode client.",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	},
	{
		"type": "function",
		"function": map[string]any{
			"name":        "glob",
			"description": "Built-in glob tool provided by the OpenCode client.",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	},
	{
		"type": "function",
		"function": map[string]any{
			"name":        "grep",
			"description": "Built-in grep tool provided by the OpenCode client.",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	},
	{
		"type": "function",
		"function": map[string]any{
			"name":        "read",
			"description": "Built-in read tool provided by the OpenCode client.",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	},
}

// stripModelPrefix normalizes model names (e.g. opencodefree/step-5-preview-free -> step-5-preview-free).
func stripModelPrefix(model string) string {
	model = strings.TrimSpace(model)
	lower := strings.ToLower(model)
	switch {
	case strings.HasPrefix(lower, "opencodefree/"):
		return model[len("opencodefree/"):]
	case strings.HasPrefix(lower, "opencode/"):
		return model[len("opencode/"):]
	case strings.HasPrefix(lower, "oc/"):
		return model[len("oc/"):]
	default:
		return model
	}
}

// normalizeChatBody ensures the payload matches the OpenCode Free upstream requirements:
// 1. Strips provider prefixes from model.
// 2. Enforces stream: true on the wire.
// 3. Injects the client fingerprint quartet (bash, glob, grep, read) into tools.
func normalizeChatBody(body []byte) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, nil // Pass unparsed body through if not valid JSON object
	}

	if modelVal, ok := payload["model"].(string); ok {
		payload["model"] = stripModelPrefix(modelVal)
	}

	// Upstream Zen free tier gate strictly enforces stream: true on the wire.
	payload["stream"] = true

	existingTools, _ := payload["tools"].([]any)
	seen := make(map[string]bool)
	for _, tool := range existingTools {
		if toolMap, ok := tool.(map[string]any); ok {
			if fn, ok := toolMap["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok && name != "" {
					seen[name] = true
				}
			} else if name, ok := toolMap["name"].(string); ok && name != "" {
				seen[name] = true
			}
		}
	}

	for _, fpTool := range defaultFingerprintTools {
		fn := fpTool["function"].(map[string]any)
		name := fn["name"].(string)
		if !seen[name] {
			existingTools = append(existingTools, fpTool)
		}
	}
	payload["tools"] = existingTools

	result, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal normalized OpenCodeFree payload: %w", err)
	}
	return result, nil
}
