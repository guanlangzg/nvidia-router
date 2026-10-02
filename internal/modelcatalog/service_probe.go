package modelcatalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"nvidia-router/internal/observability"
	"nvidia-router/internal/runtimeconfig"
	"nvidia-router/internal/upstream/nvidia"
	"nvidia-router/internal/xkproxy"
)

type probeChatFunc func([]byte) (*http.Response, error)

type probeHTTPClass string

const (
	probeHTTPSuccess     probeHTTPClass = "success"
	probeHTTPUnsupported probeHTTPClass = "unsupported"
	probeHTTPTransient   probeHTTPClass = "transient"
	probeHTTPInvalid     probeHTTPClass = "invalid"
)

func (s *Service) TestModelAutoDetailed(ctx context.Context, modelID int64) (ProbeSummary, error) {
	model, err := s.repository.Get(ctx, modelID)
	if err != nil {
		return ProbeSummary{}, fmt.Errorf("load model for detailed test: %w", err)
	}
	if model.Provider == "" {
		model.Provider = defaultModelProvider
	}
	if model.Kind != KindChat {
		summary := ProbeSummary{Base: ProbeStatusFailed, Reasoning: ProbeStatusUnknown, Tools: ProbeStatusUnknown}
		var probeErr error
		switch model.Provider {
		case ProviderNVIDIA:
			probeErr = s.probeNVIDIAModel(ctx, model)
		case ProviderOpenCodeFree:
			testCtx, cancel := context.WithTimeout(ctx, modelVerificationTimeoutFor(model))
			probeErr = s.testOpenCodeFreeModel(testCtx, model)
			cancel()
		default:
			probeErr = fmt.Errorf("%w: %s", ErrProviderNotRoutable, model.Provider)
		}
		if probeErr == nil {
			summary.Base = ProbeStatusSuccess
		}
		return summary, probeErr
	}

	switch model.Provider {
	case ProviderNVIDIA:
		return s.probeNVIDIAModelDetailed(ctx, model)
	case ProviderOpenCodeFree:
		testCtx, cancel := context.WithTimeout(ctx, modelVerificationTimeoutFor(model))
		defer cancel()
		return s.probeOpenCodeFreeModelDetailed(testCtx, model)
	default:
		return ProbeSummary{}, fmt.Errorf("%w: %s", ErrProviderNotRoutable, model.Provider)
	}
}

func (s *Service) probeNVIDIAModelDetailed(ctx context.Context, model Model) (ProbeSummary, error) {
	keyIDs, err := s.secrets.AvailableIDsShuffled(ctx)
	if err != nil {
		return ProbeSummary{}, fmt.Errorf("select NVIDIA key for detailed test: %w", err)
	}
	if len(keyIDs) == 0 {
		return ProbeSummary{}, ErrNVIDIAKeyRequired
	}
	if len(keyIDs) > probeAttempts {
		keyIDs = keyIDs[:probeAttempts]
	}
	var lastErr error
	for _, keyID := range keyIDs {
		var summary ProbeSummary
		err := s.secrets.WithSecret(ctx, keyID, func(secret []byte) error {
			tester, ok := s.discoverer.(chatModelTester)
			if !ok {
				return ErrManualTestRequired
			}
			snapshot := runtimeconfig.Snapshot{FirstByteTimeoutMS: int(modelVerificationTimeoutFor(model) / time.Millisecond)}
			summary, err = s.probeChatCapabilities(ctx, model, func(body []byte) (*http.Response, error) {
				return tester.Chat(ctx, snapshot, string(secret), body, false)
			})
			return err
		})
		if err == nil {
			return summary, nil
		}
		if ctx.Err() != nil {
			return summary, ctx.Err()
		}
		lastErr = err
		if !worthAnotherKey(err) {
			return summary, err
		}
	}
	return ProbeSummary{}, lastErr
}

func (s *Service) probeOpenCodeFreeModelDetailed(ctx context.Context, model Model) (ProbeSummary, error) {
	if isNilOpenCodeFreeClient(s.opencodefree) {
		return ProbeSummary{}, ErrProviderNotConfigured
	}
	return s.probeChatCapabilities(ctx, model, func(body []byte) (*http.Response, error) {
		return s.opencodefree.Chat(ctx, runtimeconfig.Snapshot{}, body, false)
	})
}

func (s *Service) probeChatCapabilities(ctx context.Context, model Model, call probeChatFunc) (ProbeSummary, error) {
	summary := ProbeSummary{Base: ProbeStatusFailed, Reasoning: ProbeStatusUnknown, Tools: ProbeStatusUnknown}
	baseBody, err := marshalProbeBodyForProvider(model.Provider, model.UpstreamID, nil, nil)
	if err != nil {
		return summary, err
	}
	baseResponse, err := call(baseBody)
	if err != nil {
		return summary, classifyProbeTransport(err)
	}
	basePayload, class, err := readProbeChat(baseResponse)
	if err != nil {
		return summary, err
	}
	if class != probeHTTPSuccess {
		return summary, probeClassError(class)
	}
	if _, _, err := validateProbePayload(basePayload); err != nil {
		return summary, err
	}
	summary.Base = ProbeStatusSuccess

	reasoning, wire, reasoningErr := s.probeReasoning(ctx, model, call)
	if reasoningErr != nil {
		return summary, reasoningErr
	}
	summary.Reasoning = reasoning.status
	summary.ReasoningWireFormat = wire
	if err := s.applyProbeReasoning(ctx, model.ID, reasoning, wire); err != nil {
		return summary, err
	}

	tools, toolsErr := s.probeTools(ctx, model, call)
	if toolsErr != nil {
		return summary, toolsErr
	}
	summary.Tools = tools
	if err := s.applyProbeTools(ctx, model.ID, tools); err != nil {
		return summary, err
	}
	return summary, nil
}

type reasoningProbe struct {
	status  string
	support bool
	wire    string
}

func (s *Service) probeReasoning(ctx context.Context, model Model, call probeChatFunc) (reasoningProbe, string, error) {
	wires := []string{model.ReasoningWireFormat, "openai", "thinking"}
	seen := make(map[string]struct{}, len(wires))
	transient := false
	for _, wire := range wires {
		if wire == "" || wire == "none" {
			continue
		}
		if _, ok := seen[wire]; ok {
			continue
		}
		seen[wire] = struct{}{}
		body, err := marshalProbeReasoningBodyForProvider(model.Provider, model.UpstreamID, wire)
		if err != nil {
			return reasoningProbe{}, "", err
		}
		response, err := call(body)
		if err != nil {
			if isProbeTransient(err) {
				transient = true
				continue
			}
			return reasoningProbe{}, "", err
		}
		payload, class, err := readProbeChat(response)
		if err != nil {
			return reasoningProbe{}, "", err
		}
		switch class {
		case probeHTTPSuccess:
			if _, _, err := validateProbePayload(payload); err != nil {
				if isProbeTransient(err) {
					transient = true
					continue
				}
				return reasoningProbe{}, "", err
			}
			visible, _ := observability.ReasoningContentFromBody(payload)
			status := ReasoningStatusHidden
			if visible {
				status = ReasoningStatusVisible
			}
			return reasoningProbe{status: status, support: true, wire: wire}, wire, nil
		case probeHTTPUnsupported:
			// Only a refusal that names reasoning proves the model lacks it; a
			// 403 free-tier or auth refusal proves the request never ran, so it
			// must leave the stored verdict untouched instead of writing
			// supports_reasoning=false.
			if rejectsCapability(payload, reasoningRejectionMarkers) {
				continue
			}
			transient = true
		case probeHTTPTransient:
			transient = true
		}
	}
	if transient {
		return reasoningProbe{status: ProbeStatusUnknown}, "", nil
	}
	return reasoningProbe{status: ReasoningStatusUnsupported, support: false, wire: "none"}, "none", nil
}

func (s *Service) applyProbeReasoning(ctx context.Context, id int64, probe reasoningProbe, wire string) error {
	if probe.status == ProbeStatusUnknown {
		return nil
	}
	support := probe.support
	status := probe.status
	return s.repository.applyProbe(ctx, id, probeCapabilityUpdate{
		SupportsReasoning:   &support,
		ReasoningStatus:     &status,
		ReasoningWireFormat: &wire,
	}, s.clock.Now())
}

func (s *Service) probeTools(ctx context.Context, model Model, call probeChatFunc) (string, error) {
	// Two forms because several gateway models accept tool_choice:"required"
	// but ignore its semantics: only "auto" plus an explicit instruction draws
	// a tool_call from them. Any form producing a valid call is enough for
	// supported; unsupported requires BOTH forms to be definitively negative,
	// so a gateway that merely rejects the required spelling is not condemned
	// by its first answer.
	required, err := s.attemptToolsProbe(ctx, model, call, "required")
	if err != nil {
		return ProbeStatusUnknown, err
	}
	switch {
	case required.supported:
		return "supported", nil
	case required.transient:
		// The upstream is visibly failing; firing the second form at it only
		// doubles the load without improving the verdict.
		return ProbeStatusUnknown, nil
	}
	auto, err := s.attemptToolsProbe(ctx, model, call, "auto")
	if err != nil {
		return ProbeStatusUnknown, err
	}
	switch {
	case auto.supported:
		return "supported", nil
	case auto.transient:
		return ProbeStatusUnknown, nil
	}
	return ProbeStatusUnsupported, nil
}

// toolsAttempt is one tools-probe form's outcome. negative covers both a 200
// without valid tool_calls and an explicit 4xx rejection of that form.
type toolsAttempt struct {
	supported bool
	negative  bool
	transient bool
}

func (s *Service) attemptToolsProbe(_ context.Context, model Model, call probeChatFunc, toolChoice string) (toolsAttempt, error) {
	body, err := marshalProbeToolsBody(model.UpstreamID, toolChoice)
	if err != nil {
		return toolsAttempt{}, err
	}
	response, err := call(body)
	if err != nil {
		if isProbeTransient(err) {
			return toolsAttempt{transient: true}, nil
		}
		return toolsAttempt{}, err
	}
	payload, class, err := readProbeChat(response)
	if err != nil {
		// readProbeChat returns an error alongside a class for transient and
		// invalid answers; the class decides. A nil payload with a transient
		// class is just an unreachable upstream.
		if class == probeHTTPTransient {
			return toolsAttempt{transient: true}, nil
		}
		return toolsAttempt{}, err
	}
	switch class {
	case probeHTTPUnsupported:
		// A 4xx only speaks to tool support when it complains about the
		// tools themselves. Auth, free-tier policy, region and
		// model-not-found answers say whether the request may run at all,
		// and counting them as capability evidence stamped permanent
		// "unsupported" rows on every OpenCodeFree model during the 403
		// free-tier window (2026-10-01).
		if rejectsCapability(payload, toolRejectionMarkers) {
			return toolsAttempt{negative: true}, nil
		}
		return toolsAttempt{transient: true}, nil
	case probeHTTPTransient:
		return toolsAttempt{transient: true}, nil
	case probeHTTPSuccess:
		if _, calls, err := validateProbePayload(payload); err != nil {
			if isProbeTransient(err) {
				return toolsAttempt{transient: true}, nil
			}
			return toolsAttempt{}, err
		} else if calls {
			return toolsAttempt{supported: true}, nil
		}
		return toolsAttempt{negative: true}, nil
	default:
		return toolsAttempt{}, nil
	}
}

func (s *Service) applyProbeTools(ctx context.Context, id int64, status string) error {
	if status != "supported" && status != ProbeStatusUnsupported {
		return nil
	}
	if status == "supported" {
		status = ToolsStatusSupported
	} else {
		status = ToolsStatusUnsupported
	}
	verifiedAt := s.clock.Now()
	return s.repository.applyProbe(ctx, id, probeCapabilityUpdate{ToolsStatus: &status, ToolsVerifiedAt: &verifiedAt}, verifiedAt)
}

func marshalProbeBodyForProvider(provider, model string, tools, reasoning map[string]any) ([]byte, error) {
	maxTokens := modelProbeMaxTokens
	if provider == ProviderOpenCodeFree {
		maxTokens = ocfProbeMaxTokens
	}
	return marshalProbeBodyWithLimit(model, tools, reasoning, maxTokens,
		[]map[string]string{{"role": "user", "content": "Reply with exactly OK."}})
}

func marshalProbeBodyWithLimit(model string, tools, reasoning map[string]any, maxTokens int, messages []map[string]string) ([]byte, error) {
	body := map[string]any{
		"model":      model,
		"messages":   messages,
		"max_tokens": maxTokens,
	}
	for key, value := range tools {
		body[key] = value
	}
	for key, value := range reasoning {
		body[key] = value
	}
	return json.Marshal(body)
}

func marshalProbeReasoningBodyForProvider(provider, model, wire string) ([]byte, error) {
	if wire == "thinking" {
		return marshalProbeBodyForProvider(provider, model, nil, map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": 128}})
	}
	return marshalProbeBodyForProvider(provider, model, nil, map[string]any{"reasoning_effort": "high"})
}

// probeToolsInstruction is the explicit prompt paired with tool_choice:"auto".
// It has to be a concrete task that already carries the tool's required
// argument: "You must call the weather tool." reads as a request for
// confirmation, and models that do support tool calling answer it in prose
// ("Which city?"). Both forms then looked silent, the probe wrote
// ToolsStatusUnsupported, and every later agentic request failed with 501 for
// models that had been calling tools all along (verified on four OpenCodeFree
// gateway models, 2026-10-02).
const probeToolsInstruction = "Use the weather tool to get the current weather in Paris, then summarize it in one sentence."

func marshalProbeToolsBody(model, toolChoice string) ([]byte, error) {
	message := "Reply with exactly OK."
	if toolChoice != "required" {
		message = probeToolsInstruction
	}
	return marshalProbeBodyWithLimit(model,
		map[string]any{
			"tools": []map[string]any{{
				"type": "function",
				"function": map[string]any{
					"name":        "weather",
					"description": "Return the weather for a city.",
					"parameters": map[string]any{
						"type":       "object",
						"properties": map[string]any{"city": map[string]string{"type": "string"}},
						"required":   []string{"city"},
					},
				},
			}},
			"tool_choice": toolChoice,
		}, nil, toolsProbeMaxTokens,
		[]map[string]string{{"role": "user", "content": message}})
}

// Capability rejection markers. A 4xx body is only evidence about the probed
// capability when it names that capability; everything else describes the
// request's permission to run.
var (
	toolRejectionMarkers      = []string{"tool", "function_call", "function call"}
	reasoningRejectionMarkers = []string{"reasoning", "thinking", "effort"}
)

// rejectsCapability reports whether a 4xx body explicitly refuses the probed
// capability. Treating every 4xx as a capability answer is what wrote permanent
// "unsupported" verdicts for models that were only temporarily unauthorized:
// the OpenCodeFree gateway answers an unknown or free-tier-blocked model with
// 401/403, and the OpenCode free tier answers legitimate calls with 403 while
// refusing nothing about their abilities.
func rejectsCapability(body []byte, markers []string) bool {
	text := strings.ToLower(string(body))
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func readProbeChat(response *http.Response) ([]byte, probeHTTPClass, error) {
	if response == nil || response.Body == nil {
		return nil, probeHTTPTransient, ErrUpstreamUnreachable
	}
	defer func() { _ = response.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20+1))
	if readErr != nil {
		return nil, probeHTTPTransient, ErrUpstreamUnreachable
	}
	if len(body) > 4<<20 {
		return nil, probeHTTPInvalid, fmt.Errorf("%w: probe response is too large", ErrManualTestRequired)
	}
	switch {
	case response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices:
		return body, probeHTTPSuccess, nil
	case response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError || response.StatusCode == 436:
		return body, probeHTTPTransient, ErrUpstreamUnreachable
	case response.StatusCode >= http.StatusBadRequest && response.StatusCode < http.StatusInternalServerError:
		return body, probeHTTPUnsupported, nil
	default:
		return body, probeHTTPInvalid, ErrManualTestRequired
	}
}

func validateProbePayload(body []byte) ([]byte, bool, error) {
	response := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}
	validated, err := nvidia.ValidateNonstreamChat(response)
	if err != nil {
		return nil, false, probeValidationError(err)
	}
	return validated.Body, hasValidToolCalls(validated.Body), nil
}

func hasValidToolCalls(body []byte) bool {
	var payload struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	for _, choice := range payload.Choices {
		for _, call := range choice.Message.ToolCalls {
			if call.Function.Name != "" && json.Valid([]byte(call.Function.Arguments)) {
				return true
			}
		}
	}
	return false
}

func classifyProbeTransport(err error) error {
	var proxyErr *xkproxy.Error
	if errors.As(err, &proxyErr) {
		return err
	}
	return ErrUpstreamUnreachable
}

func probeClassError(class probeHTTPClass) error {
	if class == probeHTTPTransient {
		return ErrUpstreamUnreachable
	}
	return ErrManualTestRequired
}

func isProbeTransient(err error) bool {
	return errors.Is(err, ErrUpstreamUnreachable) || errors.Is(err, nvidia.ErrEmptyResponse)
}
