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
	"time"
	"unicode/utf8"

	"nvidia-router/internal/apierror"
	"nvidia-router/internal/fault"
	"nvidia-router/internal/modelcatalog"
	"nvidia-router/internal/observability"
	responsesprotocol "nvidia-router/internal/protocol/responses"
	"nvidia-router/internal/provider"
	"nvidia-router/internal/router"
	"nvidia-router/internal/runtimeconfig"
	"nvidia-router/internal/sse"
	"nvidia-router/internal/upstream/nvidia"
)

// Responses proxies OpenAI Responses API requests by translating them to
// Chat Completions upstream and back. It shares the same ModelCatalog, Pool,
// Attempt orchestrator and NVIDIA Chat provider as the Chat handler, so no
// parallel key scheduling path exists.
type Responses struct {
	models       ModelResolver
	attempts     AttemptRunner
	client       provider.Provider
	openCodeFree OpenCodeFreeChat
	settings     runtimeconfig.Provider
}

func NewResponses(models ModelResolver, attempts AttemptRunner, client provider.Provider) *Responses {
	return &Responses{models: models, attempts: attempts, client: client}
}

func (h *Responses) WithOpenCodeFree(client OpenCodeFreeChat) *Responses {
	h.openCodeFree = client
	return h
}

func (h *Responses) WithRuntimeConfig(settings runtimeconfig.Provider) *Responses {
	h.settings = settings
	return h
}

func (h *Responses) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
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
	parsed, err := responsesprotocol.Parse(payload)
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
	id, err := responsesprotocol.NewResponseID()
	if err != nil {
		writeChatError(writer, err)
		return
	}
	config := parsed.ResponseConfig()
	if model.Provider == modelcatalog.ProviderOpenCodeFree {
		h.serveOpenCodeFreeResponses(writer, request, upstreamBody, id, model, config, stream)
		return
	}
	if !requireNVIDIAProvider(writer, model) {
		return
	}
	result, err := h.attempts.Run(applyModelTimeouts(nvidia.WithForwardedHeaders(request.Context(), request.Header), model), model.ID, stream, h.executeWithConfig(upstreamBody, id, model, config, stream))
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
		h.streamResponseWithConfig(ctx, writer, result.Response, id, model.PublicID, config)
		return
	}

	copyResponseHeaders(writer.Header(), result.Response.Header)
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(result.Response.StatusCode)
	_, _ = io.Copy(writer, result.Response.Body)
}

func (h *Responses) execute(body []byte, responsesID string, model modelcatalog.Model, stream bool) router.ExecuteFunc {
	return h.executeWithConfig(body, responsesID, model, responsesprotocol.ResponseConfig{}, stream)
}

func (h *Responses) executeWithConfig(body []byte, responsesID string, model modelcatalog.Model, config responsesprotocol.ResponseConfig, stream bool) router.ExecuteFunc {
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
			if observability.ReasoningStarvedFromBody(validated.Body) {
				slog.Warn("reasoning_starved_response",
					"model", upstreamModelFromBody(body),
					"reasoning_chars", chars,
				)
				return response, fault.EmptyResponse(errors.New("reasoning consumed completion budget"))
			}
		}
		converted, err := responsesprotocol.FromChatWithConfig(validated.Body, responsesID, model, config)
		if err != nil {
			_ = response.Body.Close()
			return response, fault.Protocol(fmt.Errorf("convert NVIDIA chat response: %w", err))
		}
		markResponseComplete(response)
		_ = response.Body.Close()
		response.Body = io.NopCloser(bytes.NewReader(converted))
		response.ContentLength = int64(len(converted))
		return response, nil
	}
}

// streamResponse drives the Responses state machine over the upstream Chat SSE
// and writes the translated Responses event sequence to the client. The first
// event commits the response so later upstream failures cannot trigger a key
// switch; an interruption after commit emits a stable response.failed terminal.
func (h *Responses) streamResponse(ctx context.Context, writer http.ResponseWriter, upstream *http.Response, responseID, model string) {
	h.streamResponseWithConfig(ctx, writer, upstream, responseID, model, responsesprotocol.ResponseConfig{})
}

func (h *Responses) streamResponseWithConfig(ctx context.Context, writer http.ResponseWriter, upstream *http.Response, responseID, model string, config responsesprotocol.ResponseConfig) {
	commit := &router.CommitState{}
	emitter := &responsesSSEEmitter{
		ctx:     ctx,
		encoder: sse.NewEncoder(commit.Wrap(writer)),
		commit:  commit,
		flusher: writer,
		header:  false,
		onComplete: func() {
			observability.SetStreamDone(ctx, true)
			if marker, ok := upstream.Body.(interface{ MarkComplete() }); ok {
				marker.MarkComplete()
			}
		},
		// TTFT for Responses streams: the first emitted event is the first data
		// event reaching the client (response.created or the first delta).
		onFirstData: func() { observability.SetFirstTokenAt(ctx, time.Now()) },
	}
	writeTimeout := streamWriteDeadline(ctx)
	emitter.writeTimeout = writeTimeout
	if writeTimeout > 0 {
		emitter.writeWatchdog = sse.NewWriteWatchdog(writeTimeout, func() { _ = upstream.Body.Close() })
		defer emitter.writeWatchdog.Stop()
	}
	source := &chatDeltaSource{decoder: sse.NewDecoder(upstream.Body)}
	cancelDone := make(chan struct{})
	defer close(cancelDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = upstream.Body.Close()
		case <-cancelDone:
		}
	}()
	interrupted, err := responsesprotocol.StreamWithConfig(source, emitter, responseID, model, config)
	// Reasoning presence/length were accumulated by the source across all deltas;
	// the state machine drives it to completion on both success and fault paths.
	observability.SetReasoningResponse(ctx, source.reasoningPresent, source.reasoningChars)
	if err != nil {
		// After commit the terminal is already written; before commit surface a
		// public error since nothing has reached the client.
		if !commit.Committed() {
			writeChatError(writer, &apierror.Error{
				Status: http.StatusInternalServerError, Type: "server_error", Code: "internal_error",
				Message: "The server could not complete the stream.",
			})
			return
		}
		// The stream committed but ended in a fault: the client already has
		// events, so nothing more can be written. Log so a truncated generation
		// is traceable; a client disconnect (context cancellation) is expected
		// churn and stays at Debug, upstream faults after commit are Warn.
		if ctx.Err() != nil {
			observability.RequestLogger(ctx).Debug("stream_context_cancelled_after_commit", "error", err)
			return
		}
		observability.RequestLogger(ctx).Warn("stream_truncated_after_commit", "error", err)
		return
	}
	// ErrStreamInterrupted was already mapped by the state machine to a stable
	// response.failed terminal plus a single [DONE], so nothing more to do.
	_ = interrupted
}

// chatDeltaSource adapts the upstream SSE decoder to the state machine's
// ChatDeltaSource by decoding each event's data payload with ParseChatDelta.
// The terminal [DONE] marker is reported as end-of-stream so the state machine
// finalises. It also accumulates reasoning_content character counts for
// observability without retaining reasoning text.
type chatDeltaSource struct {
	decoder *sse.Decoder
	done    bool
	// reasoningPresent / reasoningChars accumulate across deltas; only the
	// lengths are kept, never the reasoning text itself.
	reasoningPresent bool
	reasoningChars   int64
}

func (c *chatDeltaSource) Next() (responsesprotocol.ChatDelta, error) {
	if c.done {
		return responsesprotocol.ChatDelta{}, responsesprotocol.ErrStreamCompleted
	}
	for {
		event, err := c.decoder.Decode()
		if err == io.EOF {
			return responsesprotocol.ChatDelta{}, responsesprotocol.ErrStreamInterrupted
		}
		if err != nil {
			return responsesprotocol.ChatDelta{}, fmt.Errorf("decode upstream SSE event: %w", err)
		}
		if len(event.Data) > 0 {
			delta, done, perr := responsesprotocol.ParseChatDelta([]byte(sse.JoinData(event.Data)))
			if perr != nil {
				return responsesprotocol.ChatDelta{}, fmt.Errorf("parse chat delta: %w", perr)
			}
			if done {
				// [DONE] is the authoritative normal completion marker. Keep it
				// distinct from EOF/interruption, and ignore duplicate markers.
				c.done = true
				return responsesprotocol.ChatDelta{}, responsesprotocol.ErrStreamCompleted
			}
			if delta.Reasoning != "" {
				c.reasoningPresent = true
				c.reasoningChars += int64(utf8.RuneCountInString(delta.Reasoning))
			}
			return delta, nil
		}
		// Comments or events with no data frames: skip and read the next.
	}
}

// responsesSSEEmitter writes Responses events to the HTTP response and commits
// on the first event. The done event is rendered as the final [DONE] marker.
type responsesSSEEmitter struct {
	ctx                 context.Context
	encoder             *sse.Encoder
	commit              *router.CommitState
	flusher             http.ResponseWriter
	header              bool
	notified            bool
	deadlineUnsupported bool
	onFirstData         func()
	onComplete          func()
	// writeWatchdog guards the flush against a client that stops reading (audit
	// H6); when it fires, onStall closes the upstream body so the state machine
	// loop ends and the lease is released.
	writeWatchdog *sse.WriteWatchdog
	writeTimeout  time.Duration
}

func (e *responsesSSEEmitter) Emit(event responsesprotocol.EmittedEvent) error {
	if e.ctx != nil && e.commit.Committed() && e.ctx.Err() != nil {
		return e.ctx.Err()
	}
	if err := e.setWriteDeadline(); err != nil {
		return err
	}
	if !e.header {
		writeSSEHeaders(e.flusher)
		e.commit.Wrap(e.flusher).WriteHeader(http.StatusOK)
		e.header = true
	}
	if event.Event == "done" {
		if err := e.encoder.Encode(sse.Event{Data: []string{"[DONE]"}}); err != nil {
			return fmt.Errorf("encode done: %w", err)
		}
	} else {
		payload, err := json.Marshal(event.Payload())
		if err != nil {
			return fmt.Errorf("marshal responses event: %w", err)
		}
		if err := e.encoder.Encode(sse.Event{Event: event.Event, Data: []string{string(payload)}}); err != nil {
			return fmt.Errorf("encode responses event: %w", err)
		}
	}
	if flusher, ok := e.flusher.(http.Flusher); ok {
		if e.writeWatchdog != nil {
			e.writeWatchdog.Arm()
		}
		if err := e.flush(flusher); err != nil {
			return err
		}
		if e.writeWatchdog != nil {
			e.writeWatchdog.Disarm()
		}
	}
	if event.Event == "done" && e.onComplete != nil {
		e.onComplete()
	}
	if !e.notified {
		e.notified = true
		if e.onFirstData != nil {
			e.onFirstData()
		}
	}
	if e.writeWatchdog != nil && e.writeWatchdog.Fired() {
		return sse.ErrStreamWriteStalled
	}
	return nil
}

func (e *responsesSSEEmitter) Commit() error {
	if !e.header {
		if err := e.setWriteDeadline(); err != nil {
			return err
		}
		writeSSEHeaders(e.flusher)
		e.header = true
		e.commit.Wrap(e.flusher).WriteHeader(http.StatusOK)
	}
	return nil
}

// setWriteDeadline keeps a watchdog-only fallback for ResponseWriters that do
// not expose a cancellable write deadline. This mirrors sse.Proxy: lack of the
// optional net/http capability must not turn a valid stream into a 500.
func (e *responsesSSEEmitter) setWriteDeadline() error {
	if e.deadlineUnsupported || e.writeTimeout <= 0 {
		return nil
	}
	if err := sse.SetWriteDeadline(e.flusher, e.writeTimeout); err != nil {
		if errors.Is(err, sse.ErrWriteDeadlineUnsupported) {
			e.deadlineUnsupported = true
			return nil
		}
		return err
	}
	return nil
}

// flush uses the write deadline when available and falls back to a plain flush
// when the ResponseWriter only supports http.Flusher. The watchdog remains
// armed by Emit around either path, so unsupported writers still have a bound
// on the upstream lease even though the synchronous flush itself is not
// cancellable by net/http.
func (e *responsesSSEEmitter) flush(flusher http.Flusher) error {
	if e.deadlineUnsupported {
		flusher.Flush()
		return nil
	}
	if err := sse.FlushWithDeadline(e.flusher, flusher, e.writeTimeout); err != nil {
		if errors.Is(err, sse.ErrWriteDeadlineUnsupported) {
			e.deadlineUnsupported = true
			flusher.Flush()
			return nil
		}
		return err
	}
	return nil
}

func writeSSEHeaders(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	// Disable nginx-style reverse proxy buffering for SSE so chunks reach the
	// client live (audit B6). Non-nginx proxies ignore the header.
	writer.Header().Set("X-Accel-Buffering", "no")
}
