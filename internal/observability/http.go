package observability

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"nvidia-router/internal/clock"
)

// usageCaptureLimit bounds the buffered copy of a non-streaming JSON body.
// The whole body is needed because the usage object may appear anywhere in it.
const usageCaptureLimit = 2 << 20

// usageTailCaptureLimit bounds the buffered tail of an SSE stream. Usage only
// ever arrives in a trailing event and lastSSEEventData scans backwards, so
// retaining the whole stream was pure overhead: a long generation held up to
// usageCaptureLimit of heap per in-flight request purely to read two integers,
// and any stream that exceeded that limit dropped its usage entirely. A bounded
// tail is both far cheaper and strictly more capable.
const usageTailCaptureLimit = 64 << 10
const usageTailCaptureThreshold = 2 * usageTailCaptureLimit

var usageCaptureEndpoints = map[string]struct{}{
	"/v1/chat/completions": {},
	"/v1/embeddings":       {},
	"/v1/responses":        {},
}

type RequestRecorder interface {
	Record(context.Context, RequestRecord) error
}

// EventSink receives every assembled RequestRecord for real-time fan-out (e.g.
// the admin live view). A nil sink is a no-op; on error the record is dropped
// without affecting the request path.
type EventSink func(RequestRecord) error

func HTTPMiddleware(recorder RequestRecorder, source clock.Clock, logger *slog.Logger, next http.Handler, sinks ...EventSink) http.Handler {
	if source == nil {
		source = clock.RealClock{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := source.Now()
		ctx, state := WithRequestState(request.Context())
		// Carry the request logger on the context so handlers deep in the chain
		// can log request-scoped events without threading a logger through every
		// constructor. RequestLogger falls back to the default logger when no
		// handler wrapped the request.
		ctx = WithRequestLogger(ctx, logger)
		requestID := newRequestID()
		writer.Header().Set("X-Request-ID", requestID)
		tracked := newTrackingWriter(writer, ctx, started, source, request.URL.Path)
		next.ServeHTTP(tracked, request.WithContext(ctx))
		metadata := state.Snapshot()
		// Run the captured body through bearer-token redaction before usage
		// parsing as a defensive safety net: NVIDIA responses never carry
		// `Bearer <token>` text, so redaction is a no-op on legitimate content
		// (the fast path returns early when "bearer" is absent). If an
		// upstream error echo somehow leaked an Authorization-like body here,
		// the JSON parse would no-op usage (already the failure-shaped branch)
		// without persisting the token.
		prompt, completion := parseUsage([]byte(RedactBearerToken(tracked.body.String())), tracked.captureComplete, tracked.captureEnabled, tracked.status, tracked.Header().Get("Content-Type"))
		if metadata.PromptTokens == nil {
			metadata.PromptTokens = prompt
		}
		if metadata.CompletionTokens == nil {
			metadata.CompletionTokens = completion
		}
		RecordUsage(ctx, metadata.PromptTokens, metadata.CompletionTokens)
		status := tracked.status
		if status == 0 {
			status = http.StatusOK
		}
		outcome := OutcomeFailure
		if status == 499 {
			outcome = OutcomeCanceled
		} else if status >= http.StatusOK && status < http.StatusMultipleChoices {
			outcome = OutcomeSuccess
		}
		if (outcome == OutcomeFailure || outcome == OutcomeCanceled) && metadata.ErrorCode == nil {
			code := fallbackHTTPErrorCode(status)
			metadata.ErrorCode = &code
		}
		var firstByteMS *int64
		if metadata.IsStream && !tracked.firstBodyAt.IsZero() {
			value := tracked.firstBodyAt.Sub(started).Milliseconds()
			firstByteMS = &value
		}
		var firstTokenMS *int64
		if metadata.IsStream && !metadata.FirstTokenAt.IsZero() {
			value := metadata.FirstTokenAt.Sub(started).Milliseconds()
			firstTokenMS = &value
		}
		record := RequestRecord{
			RequestID: requestID, Endpoint: request.URL.Path, ModelID: metadata.ModelID,
			AccessKeyID: metadata.AccessKeyID, NVIDIAKeyID: metadata.NVIDIAKeyID,
			HTTPStatus: status, Outcome: outcome, ErrorCode: metadata.ErrorCode,
			IsStream: metadata.IsStream, QueueMS: metadata.QueueMS, FirstByteMS: firstByteMS,
			FirstTokenMS: firstTokenMS,
			DurationMS:   source.Now().Sub(started).Milliseconds(), AttemptCount: metadata.AttemptCount,
			PromptTokens: metadata.PromptTokens, CompletionTokens: metadata.CompletionTokens,
			UpstreamRequestID: metadata.UpstreamRequestID, RequestedCapabilities: metadata.RequestedCapabilities, CreatedAt: started,
			ReasoningRequested: metadata.ReasoningRequested, ReasoningWireFields: metadata.ReasoningWireFields,
			ReasoningSource:         metadata.ReasoningSource,
			ReasoningRequestedLevel: metadata.ReasoningRequestedLevel, ReasoningEffectiveLevel: metadata.ReasoningEffectiveLevel,
			ReasoningPresent: metadata.ReasoningPresent, ReasoningChars: metadata.ReasoningChars,
			StreamDone: metadata.StreamDone, RouteMode: metadata.RouteMode,
		}
		if err := recorder.Record(context.WithoutCancel(ctx), record); err != nil {
			logger.Error("record request metadata failed", "request_id", requestID, "error", err)
		}
		for _, sink := range sinks {
			if sink == nil {
				continue
			}
			if err := sink(record); err != nil {
				logger.Error("publish request event failed", "request_id", requestID, "error", err)
			}
		}
	})
}

func fallbackHTTPErrorCode(status int) string {
	switch {
	case status == 499:
		return "request_canceled"
	case status >= http.StatusInternalServerError:
		return "http_5xx"
	case status >= http.StatusBadRequest:
		return "http_4xx"
	case status >= http.StatusMultipleChoices:
		return "http_3xx"
	default:
		return "http_error"
	}
}

type trackingWriter struct {
	http.ResponseWriter
	ctx             context.Context
	clock           clock.Clock
	started         time.Time
	firstBodyAt     time.Time
	status          int
	body            bytes.Buffer
	captureEnabled  bool
	captureComplete bool
	// tailCapture keeps only the trailing window of an SSE body instead of the
	// whole stream. It is decided from Content-Type once the status is known.
	tailCapture bool
}

func newTrackingWriter(writer http.ResponseWriter, ctx context.Context, started time.Time, source clock.Clock, endpoint string) *trackingWriter {
	_, captureEnabled := usageCaptureEndpoints[endpoint]
	return &trackingWriter{
		ResponseWriter:  writer,
		ctx:             ctx,
		clock:           source,
		started:         started,
		captureEnabled:  captureEnabled,
		captureComplete: captureEnabled,
	}
}

func (w *trackingWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.disableCaptureIfIneligible()
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *trackingWriter) Write(payload []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
		w.disableCaptureIfIneligible()
	}
	if w.firstBodyAt.IsZero() && len(payload) > 0 {
		w.firstBodyAt = w.clock.Now()
	}
	if w.captureComplete {
		if w.tailCapture {
			w.appendTail(payload)
		} else {
			remaining := usageCaptureLimit - w.body.Len()
			if len(payload) <= remaining {
				_, _ = w.body.Write(payload)
			} else {
				// Non-stream JSON also carries its usage block at the end of the
				// body. Once the window is full, switch to tail retention (the
				// same mode SSE uses) so a long response still yields its usage
				// instead of losing it entirely (audit #19/N4): parseUsage only
				// needs the trailing usage object. captureComplete stays true so
				// the retained tail is parsed.
				w.body.Reset()
				w.tailCapture = true
				w.appendTail(payload)
			}
		}
	}
	return w.ResponseWriter.Write(payload)
}

func (w *trackingWriter) disableCaptureIfIneligible() {
	if w.status < http.StatusOK || w.status >= http.StatusMultipleChoices {
		w.captureEnabled = false
		w.captureComplete = false
		w.body.Reset()
		return
	}
	mediaType, _, err := mime.ParseMediaType(w.Header().Get("Content-Type"))
	if err != nil || !isUsageMediaType(mediaType) {
		w.captureEnabled = false
		w.captureComplete = false
		w.body.Reset()
		return
	}
	w.tailCapture = strings.EqualFold(mediaType, "text/event-stream")
}

// appendTail keeps the last usageTailCaptureLimit bytes of the stream. It trims
// at an event boundary so the retained window starts on a whole event; a
// half-event at the front would be skipped by lastSSEEventData anyway, but
// trimming keeps the buffer's contents meaningful on inspection.
// Trimming triggers only once the buffer exceeds usageTailCaptureThreshold,
// amortizing the allocation and copy cost so long streams do not churn heap on every chunk.
func (w *trackingWriter) appendTail(payload []byte) {
	_, _ = w.body.Write(payload)
	if w.body.Len() <= usageTailCaptureThreshold {
		return
	}
	retained := w.body.Bytes()[w.body.Len()-usageTailCaptureLimit:]
	if boundary := bytes.Index(retained, []byte("\n\n")); boundary >= 0 {
		retained = retained[boundary+2:]
	}
	// Copy before Reset: retained aliases the buffer's storage.
	kept := make([]byte, len(retained))
	copy(kept, retained)
	w.body.Reset()
	_, _ = w.body.Write(kept)
}

func isUsageMediaType(mediaType string) bool {
	return strings.EqualFold(mediaType, "application/json") ||
		strings.EqualFold(mediaType, "text/event-stream")
}

func (w *trackingWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *trackingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *trackingWriter) SetErrorCode(code string) {
	SetErrorCode(w.ctx, code)
}

func parseUsage(payload []byte, complete, enabled bool, status int, contentType string) (*int64, *int64) {
	if !enabled || !complete || len(payload) == 0 || status < http.StatusOK || status >= http.StatusMultipleChoices {
		return nil, nil
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, nil
	}
	var body []byte
	switch {
	case strings.EqualFold(mediaType, "application/json"):
		body = payload
		// A large non-stream response is captured only as a tail window that may
		// start mid-JSON; a direct unmarshal then fails. Fall back to extracting
		// the trailing "usage" object so the retained tail still meters tokens.
		if !json.Valid(body) {
			if extracted := extractUsageFromJSON(body); extracted != nil {
				body = extracted
			}
		}
	case strings.EqualFold(mediaType, "text/event-stream"):
		body = lastSSEEventData(payload)
	default:
		return nil, nil
	}
	var envelope struct {
		Usage struct {
			PromptTokens     *int64 `json:"prompt_tokens"`
			CompletionTokens *int64 `json:"completion_tokens"`
			InputTokens      *int64 `json:"input_tokens"`
			OutputTokens     *int64 `json:"output_tokens"`
		} `json:"usage"`
		Response struct {
			Usage struct {
				InputTokens  *int64 `json:"input_tokens"`
				OutputTokens *int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return nil, nil
	}
	if envelope.Usage.PromptTokens == nil {
		envelope.Usage.PromptTokens = envelope.Usage.InputTokens
	}
	if envelope.Usage.CompletionTokens == nil {
		envelope.Usage.CompletionTokens = envelope.Usage.OutputTokens
	}
	if envelope.Usage.PromptTokens == nil {
		envelope.Usage.PromptTokens = envelope.Response.Usage.InputTokens
	}
	if envelope.Usage.CompletionTokens == nil {
		envelope.Usage.CompletionTokens = envelope.Response.Usage.OutputTokens
	}
	return envelope.Usage.PromptTokens, envelope.Usage.CompletionTokens
}

// extractUsageFromJSON pulls the trailing "usage" object out of a JSON fragment
// that may start mid-document (a large non-stream response captured only as a
// tail window). It scans backwards for the last `"usage"` key, balances braces
// to return the complete object, and wraps it in a fresh envelope so the caller
// can unmarshal it with the same Usage struct. Returns nil when no well-formed
// usage object is present.
func extractUsageFromJSON(payload []byte) []byte {
	index := bytes.LastIndex(payload, []byte(`"usage"`))
	if index < 0 {
		return nil
	}
	open := bytes.IndexByte(payload[index+len(`"usage"`):], '{')
	if open < 0 {
		return nil
	}
	start := index + len(`"usage"`) + open
	depth := 0
	for cursor := start; cursor < len(payload); cursor++ {
		switch payload[cursor] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				object := payload[start : cursor+1]
				envelope := make([]byte, 0, len(object)+len(`{"usage":}`))
				envelope = append(envelope, `{"usage":`...)
				envelope = append(envelope, object...)
				envelope = append(envelope, '}')
				return envelope
			}
		}
	}
	return nil
}

// lastSSEEventData returns the JSON payload of the final data: event in an SSE
// stream, skipping empty and [DONE] termination events. Chat-completions and
// Responses streams both carry token usage in a trailing event.
func lastSSEEventData(payload []byte) []byte {
	trimmed := bytes.TrimRight(payload, "\r\n")
	events := bytes.Split(trimmed, []byte("\n\n"))
	for index := len(events) - 1; index >= 0; index-- {
		var data []byte
		for _, line := range bytes.Split(events[index], []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				data = append(data, bytes.TrimPrefix(line, []byte("data:"))...)
			}
		}
		event := bytes.TrimSpace(data)
		if len(event) == 0 || bytes.Equal(event, []byte("[DONE]")) {
			continue
		}
		return event
	}
	return nil
}

func newRequestID() string {
	var value [16]byte
	if _, err := io.ReadFull(rand.Reader, value[:]); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return "req-" + hex.EncodeToString(value[:])
}
