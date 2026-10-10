package opencodefree

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const (
	maxAggregatePayloadBytes = 32 << 20
)

type sseChunk struct {
	ID      string           `json:"id"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []sseChunkChoice `json:"choices"`
	Usage   json.RawMessage  `json:"usage"`
}

type sseChunkChoice struct {
	Index        int             `json:"index"`
	Delta        sseChunkDelta   `json:"delta"`
	FinishReason *string         `json:"finish_reason"`
}

type sseChunkDelta struct {
	Role             string             `json:"role"`
	Content          string             `json:"content"`
	ReasoningContent string             `json:"reasoning_content"`
	// Upstreams in this family are inconsistent about the thinking field name:
	// the stream can carry "reasoning" or "thinking" for content the protocol
	// layer already treats as equivalent aliases. Reading only reasoning_content
	// silently drops the whole reasoning trace on a non-streaming call while the
	// streaming path keeps it.
	Reasoning string             `json:"reasoning"`
	Thinking  string             `json:"thinking"`
	ToolCalls []sseChunkToolCall `json:"tool_calls"`
}

type sseChunkToolCall struct {
	Index    int                      `json:"index"`
	ID       string                   `json:"id"`
	Type     string                   `json:"type"`
	Function sseChunkToolCallFunction `json:"function"`
}

type sseChunkToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolCallSlot struct {
	id        string
	callType  string
	name      string
	arguments strings.Builder
}

type sseAggregate struct {
	id           string
	created      int64
	model        string
	content      strings.Builder
	reasoning    strings.Builder
	toolCalls    map[int]*toolCallSlot
	finishReason string
	usage        json.RawMessage
	sawFrame     bool
}

func newSseAggregate() *sseAggregate {
	return &sseAggregate{
		toolCalls: make(map[int]*toolCallSlot),
	}
}

func (agg *sseAggregate) absorb(data []byte) {
	var chunk sseChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		return
	}
	agg.sawFrame = true
	if agg.id == "" && chunk.ID != "" {
		agg.id = chunk.ID
	}
	if agg.created == 0 && chunk.Created != 0 {
		agg.created = chunk.Created
	}
	if agg.model == "" && chunk.Model != "" {
		agg.model = chunk.Model
	}
		if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
			agg.usage = bytes.Clone(chunk.Usage)
		}
	for _, choice := range chunk.Choices {
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			agg.finishReason = *choice.FinishReason
		}
		if choice.Delta.Content != "" {
			agg.content.WriteString(choice.Delta.Content)
		}
		// Alias priority matches the streaming decoder: reasoning_content first,
		// then reasoning, then thinking. A frame normally carries only one.
		switch {
		case choice.Delta.ReasoningContent != "":
			agg.reasoning.WriteString(choice.Delta.ReasoningContent)
		case choice.Delta.Reasoning != "":
			agg.reasoning.WriteString(choice.Delta.Reasoning)
		case choice.Delta.Thinking != "":
			agg.reasoning.WriteString(choice.Delta.Thinking)
		}
		for _, tc := range choice.Delta.ToolCalls {
			slot, exists := agg.toolCalls[tc.Index]
			if !exists {
				slot = &toolCallSlot{}
				agg.toolCalls[tc.Index] = slot
			}
			if tc.ID != "" {
				slot.id = tc.ID
			}
			if tc.Type != "" {
				slot.callType = tc.Type
			}
			if tc.Function.Name != "" {
				slot.name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				slot.arguments.WriteString(tc.Function.Arguments)
			}
		}
	}
}

func (agg *sseAggregate) toCompletion() ([]byte, error) {
	if !agg.sawFrame {
		return nil, errors.New("upstream stream contained no chat completion frames")
	}

	indices := make([]int, 0, len(agg.toolCalls))
	for idx := range agg.toolCalls {
		indices = append(indices, idx)
	}
	sort.Ints(indices)

	var toolCallsList []map[string]any
	for _, idx := range indices {
		slot := agg.toolCalls[idx]
		callID := slot.id
		if callID == "" {
			callID = fmt.Sprintf("call_agg_%d", idx)
		}
		callType := slot.callType
		if callType == "" {
			callType = "function"
		}
		args := slot.arguments.String()
		if args == "" {
			args = "{}"
		}
		toolCallsList = append(toolCallsList, map[string]any{
			"id":   callID,
			"type": callType,
			"function": map[string]any{
				"name":      slot.name,
				"arguments": args,
			},
		})
	}

	message := map[string]any{
		"role": "assistant",
	}
	if agg.content.Len() > 0 {
		message["content"] = agg.content.String()
	}
	if agg.reasoning.Len() > 0 {
		message["reasoning_content"] = agg.reasoning.String()
	}
	if len(toolCallsList) > 0 {
		message["tool_calls"] = toolCallsList
	}
	if agg.content.Len() == 0 && agg.reasoning.Len() == 0 && len(toolCallsList) == 0 {
		message["content"] = ""
	}

	finishReason := agg.finishReason
	if finishReason == "" {
		finishReason = "stop"
	}

	created := agg.created
	if created == 0 {
		created = time.Now().Unix()
	}

	id := agg.id
	if id == "" {
		id = fmt.Sprintf("gen-%d", created)
	}

	completion := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   agg.model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if len(agg.usage) > 0 {
		var usageMap any
		if json.Unmarshal(agg.usage, &usageMap) == nil {
			completion["usage"] = usageMap
		}
	}

	return json.Marshal(completion)
}

// aggregateSseCompletion reads an SSE event stream from reader and reconstructs
// a single non-streaming chat.completion JSON object.
func aggregateSseCompletion(reader io.Reader) ([]byte, error) {
	agg := newSseAggregate()
	scanner := bufio.NewScanner(reader)
	// Allocate generous buffer for large frames
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 4*1024*1024)

	totalBytes := 0
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		totalBytes += len(line)
		if totalBytes > maxAggregatePayloadBytes {
			return nil, fmt.Errorf("aggregate OpenCodeFree SSE: stream exceeded max size limit of %d bytes", maxAggregatePayloadBytes)
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			payload := bytes.TrimSpace(line[5:])
			if len(payload) == 0 {
				continue
			}
			if bytes.Equal(payload, []byte("[DONE]")) {
				break
			}
			agg.absorb(payload)
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read SSE stream: %w", err)
	}
	return agg.toCompletion()
}
