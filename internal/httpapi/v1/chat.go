package v1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"nvidia-router/internal/apierror"
	"nvidia-router/internal/fault"
	"nvidia-router/internal/modelcatalog"
	"nvidia-router/internal/observability"
	chatprotocol "nvidia-router/internal/protocol/chat"
	"nvidia-router/internal/provider"
	"nvidia-router/internal/router"
	"nvidia-router/internal/runtimeconfig"
	"nvidia-router/internal/sse"
	"nvidia-router/internal/upstream/nvidia"
	"nvidia-router/internal/xkproxy"
)

type ModelResolver interface {
	Resolve(context.Context, string, modelcatalog.Requirements) (modelcatalog.Model, error)
}

type AttemptRunner interface {
	Run(context.Context, int64, bool, router.ExecuteFunc) (router.AttemptResult, error)
}

// OpenCodeFreeChat is the chat surface of the optional OpenCodeFree gateway. It
// has no NVIDIA key pool, so the router talks to it directly instead of through
// Attempt; the shared HTTP transport still bounds connect and response windows.
type OpenCodeFreeChat interface {
	Chat(context.Context, runtimeconfig.Snapshot, []byte, bool) (*http.Response, error)
}

type Chat struct {
	models       ModelResolver
	attempts     AttemptRunner
	client       provider.Provider
	openCodeFree OpenCodeFreeChat // optional; nil keeps NVIDIA-only routing
	settings     runtimeconfig.Provider
}

func NewChat(models ModelResolver, attempts AttemptRunner, client provider.Provider) *Chat {
	return &Chat{models: models, attempts: attempts, client: client}
}

// WithOpenCodeFree attaches the optional OpenCodeFree gateway client. Models
// whose provider is opencodefree are routed here instead of the NVIDIA key pool.
func (h *Chat) WithOpenCodeFree(client OpenCodeFreeChat) *Chat {
	h.openCodeFree = client
	return h
}

func (h *Chat) WithRuntimeConfig(settings runtimeconfig.Provider) *Chat {
	h.settings = settings
	return h
}

func (h *Chat) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeChatError(writer, &apierror.Error{
			Status: http.StatusMethodNotAllowed, Type: "invalid_request_error", Code: "method_not_allowed",
			Message: "Only POST is supported for this endpoint.",
		})
		return
	}
	payload, bodyLease, err := readBodyWithLease(request, bodyReadLimitForJSON(), jsonBodyReadTimeout)
	if bodyLease != nil {
		defer bodyLease.Release()
	}
	if err != nil {
		writeChatError(writer, err)
		return
	}
	parsed, err := chatprotocol.Parse(payload)
	if err != nil {
		writeChatError(writer, err)
		return
	}
	prepared, err := prepareModelRequest(request.Context(), parsed, h.models)
	if err != nil {
		writeChatError(writer, err)
		return
	}
	model := prepared.Model
	upstreamBody := prepared.Body
	stream := prepared.Stream
	// OpenCodeFree models route to the optional gateway directly: no NVIDIA key
	// pool, no failover, one upstream. Every other provider keeps the Attempt
	// path (key pool, retry budget, sticky sessions).
	if model.Provider == modelcatalog.ProviderOpenCodeFree {
		h.serveOpenCodeFree(writer, request, upstreamBody, stream)
		return
	}
	// Propagate per-model streaming timeout overrides so the budget builder
	// inside Attempt.Run uses the model's configured windows instead of the
	// global defaults. Matters most for deepseek-v4-flash, whose TTFT on
	// NVIDIA infrastructure routinely exceeds the fleet-wide default.
	runCtx := applyModelTimeouts(nvidia.WithForwardedHeaders(request.Context(), request.Header), model)
	result, err := h.attempts.Run(runCtx, model.ID, stream, h.execute(upstreamBody, stream))
	if err != nil {
		writeChatError(writer, err)
		return
	}
	defer result.Release()
	defer func() { _ = result.Response.Body.Close() }()

	if stream {
		ctx := result.Context
		if ctx == nil {
			ctx = request.Context()
		}
		h.streamResponse(ctx, writer, result.Response)
		return
	}

	copyResponseHeaders(writer.Header(), result.Response.Header)
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(result.Response.StatusCode)
	_, _ = io.Copy(writer, result.Response.Body)
}

func (h *Chat) execute(body []byte, stream bool) router.ExecuteFunc {
	return func(ctx context.Context, keyID int64, secret []byte, _ *router.CommitState) (*http.Response, error) {
		ctx = nvidia.WithStickySession(ctx, keyID)
		response, err := h.client.Chat(ctx, snapshotFromBudget(ctx), string(secret), body, stream)
		if err != nil {
			return nil, err
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return response, nil
		}
		if stream {
			if err := primeSSE(ctx, response); err != nil {
				if errors.Is(err, sse.ErrNoSemanticData) {
					return response, fault.EmptyResponse(err)
				}
				if errors.Is(err, sse.ErrEventTooLarge) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					// A 200 with an empty/non-SSE body is an upstream protocol
					// defect, not a connection blip; classify it as Protocol so
					// the attempt loop does not cool down every healthy key.
					return response, fault.Protocol(err)
				}
				return response, err
			}
			return response, nil
		}
		validated, err := nvidia.ValidateNonstreamChat(response)
		if err != nil {
			if errors.Is(err, nvidia.ErrEmptyResponse) {
				return response, fault.EmptyResponse(err)
			}
			if errors.Is(err, nvidia.ErrProtocol) {
				return response, fault.Protocol(err)
			}
			return response, err
		}
		if present, chars := observability.ReasoningContentFromBody(validated.Body); present {
			observability.SetReasoningResponse(ctx, present, chars)
			// Protocol-valid but useless to the caller: reasoning ate the whole
			// completion allowance and the answer came back empty. Nothing else in
			// the pipeline flags it, so surface it here rather than let an empty
			// string pass for a successful response.
			if observability.ReasoningStarvedFromBody(validated.Body) {
				slog.Warn("reasoning_starved_response",
					"model", upstreamModelFromBody(body),
					"reasoning_chars", chars,
				)
				return response, fault.EmptyResponse(errors.New("reasoning consumed completion budget"))
			}
		}
		markResponseComplete(response)
		_ = response.Body.Close()
		response.Body = io.NopCloser(bytes.NewReader(validated.Body))
		response.ContentLength = int64(len(validated.Body))
		return response, nil
	}
}

// openCodeFreeRequestTimeout bounds a single OpenCodeFree gateway call. The
// gateway is a local/trusted endpoint with no key pool, so a hung upstream is
// bounded by an absolute window instead of the NVIDIA retry machinery.
const openCodeFreeRequestTimeout = 10 * time.Minute

// serveOpenCodeFree runs a chat request against the optional OpenCodeFree
// gateway. The shared executor owns retry/status/body lifecycle; this method
// only adapts Chat validation and stream writing to that lifecycle.
func (h *Chat) serveOpenCodeFree(writer http.ResponseWriter, request *http.Request, body []byte, stream bool) {
	if h.openCodeFree == nil {
		writeChatError(writer, &apierror.Error{
			Status: http.StatusServiceUnavailable, Type: "server_error", Code: "provider_unconfigured",
			Message: "The OpenCodeFree gateway is not configured.",
		})
		return
	}
	tracked := &firstWriteTracker{ResponseWriter: writer}
	execution := openCodeFreeExecution{
		call: func(ctx context.Context, stream bool) (*http.Response, error) {
			return h.openCodeFree.Chat(ctx, runtimeconfig.Snapshot{}, body, stream)
		},
	}
	err := execution.run(nvidia.WithForwardedHeaders(request.Context(), request.Header), stream, tracked,
		func(ctx context.Context, response *http.Response) error {
			if stream {
				h.streamResponse(ctx, tracked, response)
				return nil
			}
			validated, err := nvidia.ValidateNonstreamChat(response)
			if err != nil {
				if errors.Is(err, nvidia.ErrEmptyResponse) {
					return fault.EmptyResponse(err)
				}
				if errors.Is(err, nvidia.ErrProtocol) {
					return fault.Protocol(err)
				}
				return err
			}
			if present, chars := observability.ReasoningContentFromBody(validated.Body); present {
				observability.SetReasoningResponse(ctx, present, chars)
				if observability.ReasoningStarvedFromBody(validated.Body) {
					slog.Warn("reasoning_starved_response",
						"model", upstreamModelFromBody(body),
						"reasoning_chars", chars,
					)
					return fault.EmptyResponse(errors.New("reasoning consumed completion budget"))
				}
			}
			markResponseComplete(response)
			copyResponseHeaders(tracked.Header(), response.Header)
			tracked.Header().Set("Content-Type", "application/json")
			tracked.WriteHeader(response.StatusCode)
			_, _ = tracked.Write(validated.Body)
			return nil
		})
	if err != nil {
		writeChatError(writer, fmt.Errorf("OpenCodeFree chat: %w", err))
	}
}

// firstWriteTracker records whether any byte (including headers) reached the
// client, so a streaming retry can be proven safe before it happens.
type firstWriteTracker struct {
	http.ResponseWriter
	wroteHeader bool
	wrote       bool
}

// Unwrap preserves optional ResponseWriter capabilities (for example
// SetWriteDeadline) when the tracker is used as a transparent adapter.
func (t *firstWriteTracker) Unwrap() http.ResponseWriter {
	return t.ResponseWriter
}

func (t *firstWriteTracker) WriteHeader(status int) {
	if !t.wrote {
		t.wrote = true
		t.wroteHeader = true
	}
	t.ResponseWriter.WriteHeader(status)
}

func (t *firstWriteTracker) Write(payload []byte) (int, error) {
	if !t.wrote {
		t.wrote = true
		t.wroteHeader = true
	}
	return t.ResponseWriter.Write(payload)
}

func (t *firstWriteTracker) Flush() {
	if !t.wrote {
		t.wrote = true
		t.wroteHeader = true
	}
	if flusher, ok := t.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

type chatStreamCompletion struct {
	contentPresent  bool
	toolCallPresent bool
	finishLength    bool
}

func (s *chatStreamCompletion) observe(data string) {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content   json.RawMessage   `json:"content"`
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return
	}
	for _, choice := range chunk.Choices {
		s.contentPresent = s.contentPresent || hasSSETextValue(choice.Delta.Content)
		s.toolCallPresent = s.toolCallPresent || len(choice.Delta.ToolCalls) > 0
		s.finishLength = s.finishLength || choice.FinishReason == "length"
	}
}

func (s chatStreamCompletion) isEmpty() bool {
	return s.finishLength && !s.contentPresent && !s.toolCallPresent
}
func (h *Chat) streamResponse(ctx context.Context, writer http.ResponseWriter, upstream *http.Response) {
	commit := &router.CommitState{}
	// Reasoning length sampling is gated on the request having asked for it;
	// non-reasoning streams never pay per-event JSON parsing. The gate is read
	// once so the snapshot mutex is not taken per SSE event.
	trackReasoning := observability.ReasoningRequested(ctx)
	var reasoningPresent bool
	var reasoningChars int64
	completion := &chatStreamCompletion{}
	opts := sse.ProxyOptions{
		CommitState: commit,
		OnComplete: func() {
			observability.SetStreamDone(ctx, true)
			if marker, ok := upstream.Body.(interface{ MarkComplete() }); ok {
				marker.MarkComplete()
			}
		},
		BeforeComplete: func() error {
			if completion.isEmpty() {
				return sse.ErrStreamEmptyResponse
			}
			return nil
		},
		// TTFT is the first SSE data event reaching the client, distinct from
		// the first-byte metric which also fires for error bodies before commit.
		OnFirstData: func() { observability.SetFirstTokenAt(ctx, time.Now()) },
		// Bound how long a flush may block on a client that stopped reading; the
		// request context only fires on disconnect, not on a connected-but-stalled
		// consumer, so without this a stuck client pins the credential slot until
		// the upstream closes (audit H6).
		WriteIdleTimeout: streamWriteDeadline(ctx),
	}
	// Observe terminal metadata for the empty-answer guard and, when requested,
	// accumulate reasoning character counts without retaining reasoning text.
	opts.OnData = func(data string) {
		completion.observe(data)
		if trackReasoning {
			if present, chars := observability.ReasoningDeltaChars([]byte(data)); present {
				reasoningPresent = true
				reasoningChars += chars
			}
		}
	}
	err := sse.Proxy(ctx, commit.Wrap(writer), upstream, opts)
	observability.SetReasoningResponse(ctx, reasoningPresent, reasoningChars)
	if err == sse.ErrStreamEmptyResponse && commit.Committed() {
		writeStreamEmpty(writer)
		observability.RequestLogger(ctx).Warn("stream_empty_response_after_commit", "error", err)
		return
	}
	if err == nil || (err == sse.ErrStreamWriteStalled && commit.Committed()) {
		// Clean completion, or a write stall after commit (the client stopped
		// reading and the stream was torn down to release the lease): the client
		// is not consuming, so nothing more can be delivered either way.
		return
	}
	if err == sse.ErrStreamInterrupted && commit.Committed() {
		if ctx.Err() != nil {
			observability.RequestLogger(ctx).Debug("stream_context_cancelled_after_commit", "error", err)
			return
		}
		// EOF before [DONE] with bytes already delivered is a truncated
		// generation, not a completion. The status can no longer change, but
		// silently closing made agents treat the partial output — an empty
		// reply in the worst case — as a successful completion. Append the
		// OpenAI mid-stream error shape so SDKs surface the failure and the
		// caller can retry. Observability keeps stream_done=0 because the
		// upstream never completed.
		writeStreamTruncated(writer)
		observability.RequestLogger(ctx).Warn("stream_truncated_after_commit", "error", err)
		return
	}
	// Context cancelled or other error after commit - nothing we can do.
	// Before commit, writeChatError handles it. An interrupted stream that
	// never committed is an upstream protocol defect (200 with no data events):
	// surface an error rather than the empty 200 the client would take as a
	// successful completion.
	if !commit.Committed() {
		if errors.Is(err, sse.ErrStreamInterrupted) {
			writeChatError(writer, &apierror.Error{
				Status: http.StatusBadGateway, Type: "server_error", Code: "upstream_protocol_error",
				Message: "The upstream service ended the stream before sending any data.",
			})
			return
		}
		writeChatError(writer, &apierror.Error{
			Status: http.StatusInternalServerError, Type: "server_error", Code: "internal_error",
			Message: "The server could not complete the stream.",
		})
		return
	}
	// The stream committed but ended with an error outside the benign set above.
	// The client already has headers, so nothing more can be written; log so a
	// truncated generation is traceable instead of silently disappearing. A
	// client disconnect (context cancellation) is expected churn and stays at
	// Debug; upstream stalls and decode errors after commit are genuine faults.
	if ctx.Err() != nil {
		observability.RequestLogger(ctx).Debug("stream_context_cancelled_after_commit", "error", err)
		return
	}
	writeStreamTruncated(writer)
	observability.RequestLogger(ctx).Warn("stream_truncated_after_commit", "error", err)
}

// writeStreamTruncated appends the in-stream error event for a committed SSE
// stream whose upstream ended before [DONE]. The trailing [DONE] lets event
// loops that wait for the marker terminate instead of hanging until their idle
// timeout; SDKs that inspect chunk contents raise on the error payload first.
func writeStreamTruncated(writer http.ResponseWriter) {
	writeStreamError(
		writer,
		"upstream_stream_truncated",
		"The upstream service ended the stream before completion; the response is truncated. Retry the request.",
	)
}

func writeStreamEmpty(writer http.ResponseWriter) {
	writeStreamError(
		writer,
		"upstream_empty_response",
		"The upstream service completed without a user-visible answer. Retry the request.",
	)
}

func writeStreamError(writer http.ResponseWriter, code, message string) {
	payload, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "server_error",
			"code":    code,
		},
	})
	if err != nil {
		return
	}
	encoder := sse.NewEncoder(writer)
	_ = encoder.Encode(sse.Event{Data: []string{string(payload)}})
	_ = encoder.Encode(sse.Event{Data: []string{"[DONE]"}})
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

func snapshotFromBudget(ctx context.Context) runtimeconfig.Snapshot {
	budget, ok := router.BudgetFromContext(ctx)
	if !ok {
		return runtimeconfig.Snapshot{}
	}
	return runtimeconfig.Snapshot{
		ConnectTimeoutMS:   int(budget.ConnectTimeout() / time.Millisecond),
		FirstByteTimeoutMS: int(budget.FirstByteTimeout() / time.Millisecond),
		FirstByteDeadline:  budget.FirstByteDeadline(),
	}
}

// streamWriteIdleTimeout returns the write-side stall window from the request
// budget. It mirrors the read-side idle timeout so a client that stops reading
// is torn down on the same cadence as an upstream that stops sending. The read
// and write windows are deliberately coupled: a long-running generation needs
// both sides to tolerate the same inter-token gaps.
func streamWriteIdleTimeout(ctx context.Context) time.Duration {
	budget, ok := router.BudgetFromContext(ctx)
	if !ok {
		return 0
	}
	return budget.StreamIdleTimeout()
}

func streamWriteDeadline(ctx context.Context) time.Duration {
	deadlineRemaining := time.Duration(0)
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 {
			deadlineRemaining = remaining
		} else {
			deadlineRemaining = time.Nanosecond
		}
	}
	if idle := streamWriteIdleTimeout(ctx); idle > 0 {
		if deadlineRemaining > 0 && deadlineRemaining < idle {
			return deadlineRemaining
		}
		return idle
	}
	return deadlineRemaining
}

// applyModelTimeouts returns a context with per-model streaming timeout hints
// when the model has non-nil override columns. The router's Attempt.Run merges
// these into the budget so the model's configured windows take precedence over
// the global runtime_settings defaults.
func applyModelTimeouts(ctx context.Context, model modelcatalog.Model) context.Context {
	if model.StreamFirstTokenTimeoutMS == nil && model.StreamIdleTimeoutMS == nil {
		return ctx
	}
	hints := runtimeconfig.ModelTimeouts{}
	if model.StreamFirstTokenTimeoutMS != nil {
		hints.StreamFirstTokenTimeoutMS = *model.StreamFirstTokenTimeoutMS
	}
	if model.StreamIdleTimeoutMS != nil {
		hints.StreamIdleTimeoutMS = *model.StreamIdleTimeoutMS
	}
	return runtimeconfig.WithModelTimeouts(ctx, hints)
}

// upstreamModelFromBody reads the model ID out of an already-marshaled upstream
// request body. It runs only on the rare starved-response path, so the extra
// parse never touches the hot path.
func upstreamModelFromBody(body []byte) string {
	var payload struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	return payload.Model
}

func modelError(err error) error {
	if errors.Is(err, modelcatalog.ErrModelNotFound) {
		return &apierror.Error{
			Status: http.StatusNotFound, Type: "invalid_request_error", Code: "model_not_found",
			Message: "The requested model is not available.",
		}
	}
	var unsupported *modelcatalog.UnsupportedCapabilityError
	if errors.As(err, &unsupported) {
		param := unsupported.Capability
		message := "The selected model does not support the requested capability."
		switch unsupported.Capability {
		case modelcatalog.CapabilityTools:
			message = "The selected model does not support tools. Remove tools and tool_choice or choose a model that supports tools."
		case modelcatalog.CapabilityReasoning:
			message = "The selected model does not support the requested reasoning mode."
		case modelcatalog.CapabilityVision:
			message = "The selected model does not support vision inputs."
		}
		return &apierror.Error{
			Status: http.StatusNotImplemented, Type: "invalid_request_error", Code: "model_capability_unsupported",
			Message: message, Param: &param, Cause: err,
		}
	}
	if errors.Is(err, modelcatalog.ErrCapabilityUnverified) {
		return &apierror.Error{
			Status: http.StatusNotImplemented, Type: "invalid_request_error", Code: "capability_unverified",
			Message: "The requested model capability has not been verified.",
		}
	}
	if errors.Is(err, modelcatalog.ErrModelKindMismatch) || errors.Is(err, modelcatalog.ErrCapabilityUnsupported) {
		return &apierror.Error{
			Status: http.StatusNotImplemented, Type: "invalid_request_error", Code: "not_implemented",
			Message: "The requested model capability is not implemented.",
		}
	}
	return err
}

// recordCapabilityErrorCode tags the request-log record with a specific
// error_code for capability rejections; the middleware's fallback would
// otherwise lump them into the generic http_4xx bucket.
func recordCapabilityErrorCode(ctx context.Context, err error) {
	var unsupported *modelcatalog.UnsupportedCapabilityError
	if errors.As(err, &unsupported) {
		observability.SetErrorCode(ctx, "model_capability_unsupported")
		return
	}
	switch {
	case errors.Is(err, modelcatalog.ErrCapabilityUnverified):
		observability.SetErrorCode(ctx, "capability_unverified")
	case errors.Is(err, modelcatalog.ErrCapabilityUnsupported), errors.Is(err, modelcatalog.ErrModelKindMismatch):
		observability.SetErrorCode(ctx, "capability_unsupported")
	}
}

func writeChatError(writer http.ResponseWriter, err error) {
	var proxyErr *xkproxy.Error
	if errors.As(err, &proxyErr) {
		apierror.Error{
			Status: http.StatusBadGateway, Type: "server_error", Code: "upstream_proxy_unavailable",
			Message: "The upstream proxy is temporarily unavailable.",
		}.Write(writer)
		return
	}
	var publicError *apierror.Error
	if errors.As(err, &publicError) {
		publicError.Write(writer)
		return
	}
	var upstreamFault fault.Fault
	if errors.As(err, &upstreamFault) {
		apierror.Error{
			Status: upstreamFault.HTTPStatus, Type: upstreamFault.PublicType, Code: upstreamFault.PublicCode,
			Message: upstreamFault.PublicMessage, RetryAfter: upstreamFault.RetryAfter,
		}.Write(writer)
		return
	}
	if router.BodyTooLarge(err) {
		apierror.Error{
			Status: http.StatusRequestEntityTooLarge, Type: "invalid_request_error", Code: "request_too_large",
			Message: "The request body exceeds the 25 MiB replay limit.",
		}.Write(writer)
		return
	}
	apierror.Error{
		Status: http.StatusInternalServerError, Type: "server_error", Code: "internal_error",
		Message: "The server could not complete the request.",
	}.Write(writer)
}

func primeSSE(ctx context.Context, response *http.Response) error {
	primeCtx := ctx
	cancel := func() {}
	var idle time.Duration
	if budget, ok := router.BudgetFromContext(ctx); ok {
		// The prime phase waits for the first SSE data event, so it is bounded by
		// the first-token window, not the transport-level first-byte window. The
		// idle guard that wraps the body below is the separate in-stream window.
		primeCtx, cancel = context.WithDeadline(ctx, budget.FirstTokenDeadline())
		idle = budget.StreamIdleTimeout()
	}
	defer cancel()
	if err := sse.PrimeUntil(primeCtx, response, semanticChatEvent); err != nil {
		return err
	}
	// After the headers are committed a stalled upstream would pin the lease
	// forever; wrap the body so silence beyond the idle window returns
	// ErrStreamIdle instead of blocking the decode loop indefinitely.
	response.Body = sse.WithIdleTimeout(response.Body, idle)
	return nil
}

func semanticChatEvent(event sse.Event) (bool, error) {
	data := strings.TrimSpace(sse.JoinData(event.Data))
	if data == "" {
		return false, nil
	}
	if data == "[DONE]" {
		return false, sse.ErrNoSemanticData
	}

	// Fast path: check if the data contains any reasoning-related fields before
	// full JSON parsing. This avoids expensive unmarshaling for most chunks that
	// only contain content deltas.
	dataBytes := []byte(data)
	hasReasoningFields := bytes.Contains(dataBytes, []byte(`"reasoning_content"`)) ||
		bytes.Contains(dataBytes, []byte(`"reasoning"`)) ||
		bytes.Contains(dataBytes, []byte(`"thinking"`)) ||
		bytes.Contains(dataBytes, []byte(`"tool_calls"`))

	// If no reasoning fields are present, parse content so empty/null deltas do
	// not make the attempt look semantic and commit a response prematurely.
	if !hasReasoningFields {
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content json.RawMessage `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(dataBytes, &chunk); err != nil {
			return false, fmt.Errorf("decode upstream chat event: %w", err)
		}
		for _, choice := range chunk.Choices {
			if hasSSETextValue(choice.Delta.Content) {
				return true, nil
			}
		}
		return false, nil
	}

	// Slow path: full JSON parsing when reasoning fields are present
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content          json.RawMessage   `json:"content"`
				ReasoningContent json.RawMessage   `json:"reasoning_content"`
				Reasoning        json.RawMessage   `json:"reasoning"`
				Thinking         json.RawMessage   `json:"thinking"`
				ToolCalls        []json.RawMessage `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(dataBytes, &chunk); err != nil {
		return false, fmt.Errorf("decode upstream chat event: %w", err)
	}
	for _, choice := range chunk.Choices {
		if hasSSETextValue(choice.Delta.Content) ||
			hasSSETextValue(choice.Delta.ReasoningContent) ||
			hasSSETextValue(choice.Delta.Reasoning) ||
			hasSSETextValue(choice.Delta.Thinking) ||
			len(choice.Delta.ToolCalls) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func hasSSETextValue(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	var text string
	if json.Unmarshal(trimmed, &text) == nil {
		return text != ""
	}
	var nested struct {
		Thought string `json:"thought"`
		Text    string `json:"text"`
	}
	if json.Unmarshal(trimmed, &nested) == nil {
		if nested.Thought != "" || nested.Text != "" {
			return true
		}
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(trimmed, &parts) != nil {
		return false
	}
	for _, part := range parts {
		if part.Text != "" {
			return true
		}
	}
	return false
}

func markResponseComplete(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	if marker, ok := response.Body.(interface{ MarkComplete() }); ok {
		marker.MarkComplete()
	}
}
