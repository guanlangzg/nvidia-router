package v1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"nvidia-router/internal/apierror"
	"nvidia-router/internal/fault"
	"nvidia-router/internal/upstream/opencodefree"
)

const openCodeFreeRetryDelay = 500 * time.Millisecond

// openCodeFreeExecution owns the lifecycle shared by the Chat and Responses
// adapters. The adapter keeps protocol parsing and response writing; this
// component only decides whether an attempt can be replayed and closes every
// upstream response it receives.
type openCodeFreeExecution struct {
	call      func(context.Context, bool) (*http.Response, error)
	wait      func(context.Context) error
	waitDelay func(context.Context, time.Duration) error
}

type openCodeFreeNonRetryable struct {
	err error
}

func (e openCodeFreeNonRetryable) Error() string { return e.err.Error() }

func (e openCodeFreeNonRetryable) Unwrap() error { return e.err }

// run makes at most one replay. A status or callback fault is returned to the
// adapter so it can preserve the existing public error mapping. The callback
// receives the per-attempt context and must write successful output through the
// supplied tracker; returning a retryable fault after that write never replays.
func (e openCodeFreeExecution) run(parent context.Context, stream bool, tracker *firstWriteTracker, callback func(context.Context, *http.Response) error) error {
	if e.call == nil {
		return errors.New("OpenCodeFree execution has no provider call")
	}
	if tracker == nil {
		tracker = &firstWriteTracker{}
	}
	for attempt := 0; attempt < 2; attempt++ {
		attemptCtx := opencodefree.WithRetryAttempt(parent, attempt)
		ctx, cancel := context.WithTimeout(attemptCtx, openCodeFreeRequestTimeout)
		response, err := e.call(ctx, stream)
		if err != nil {
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			cancel()
			return err
		}
		if response == nil || response.Body == nil {
			cancel()
			return fault.EmptyResponse(errors.New("OpenCodeFree returned no response"))
		}
		response.Body = &openCodeFreeBody{ReadCloser: response.Body}

		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			retry, delay, mapped := classifyOpenCodeFreeStatus(response, attempt == 0)
			_ = response.Body.Close()
			cancel()
			if retry && !tracker.wrote {
				if err := e.waitForRetry(parent, delay); err != nil {
					if parent.Err() != nil {
						return nil
					}
					return err
				}
				continue
			}
			return mapped
		}

		callbackErr := callback(ctx, response)
		_ = response.Body.Close()
		cancel()
		if callbackErr == nil {
			return nil
		}
		if attempt == 0 && !tracker.wrote && openCodeFreeRetryableCallbackError(callbackErr) {
			if err := e.waitForRetry(parent, openCodeFreeRetryDelay); err != nil {
				if parent.Err() != nil {
					return nil
				}
				return err
			}
			continue
		}
		return callbackErr
	}
	return errors.New("OpenCodeFree execution exhausted retry budget")
}

// openCodeFreeBody makes response ownership idempotent across protocol
// callbacks and the executor. Stream callbacks may close their body when a
// cancelled context unblocks a read; the executor still owns the final close,
// but the underlying transport must only see one Close call.
type openCodeFreeBody struct {
	io.ReadCloser
	once sync.Once
	err  error
}

func (b *openCodeFreeBody) Close() error {
	b.once.Do(func() { b.err = b.ReadCloser.Close() })
	return b.err
}

func (b *openCodeFreeBody) MarkComplete() {
	if marker, ok := b.ReadCloser.(interface{ MarkComplete() }); ok {
		marker.MarkComplete()
	}
}

func (b *openCodeFreeBody) RequireSemanticCompletion() {
	if marker, ok := b.ReadCloser.(interface{ RequireSemanticCompletion() }); ok {
		marker.RequireSemanticCompletion()
	}
}

func (e openCodeFreeExecution) waitForRetry(ctx context.Context, delay time.Duration) error {
	if e.waitDelay != nil {
		return e.waitDelay(ctx, delay)
	}
	if e.wait != nil {
		return e.wait(ctx)
	}
	if delay <= 0 {
		delay = openCodeFreeRetryDelay
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func parseRetryAfter(header string) (time.Duration, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseFloat(header, 64); err == nil {
		if seconds <= 0 {
			return 0, false
		}
		return time.Duration(seconds * float64(time.Second)), true
	}
	if parsedTime, err := http.ParseTime(header); err == nil {
		delay := time.Until(parsedTime)
		if delay <= 0 {
			return 0, false
		}
		return delay, true
	}
	return 0, false
}

func isQuotaExhaustion(message string) bool {
	lower := strings.ToLower(message)
	for _, term := range []string{"insufficient_quota", "quota_exceeded", "exceeded your current quota", "out of credit", "billing"} {
		if strings.Contains(lower, term) {
			return true
		}
	}
	return false
}

// isRetryable429 reports whether a 429 status code is a retryable transient failure
// (e.g. carrying Retry-After or indicating server-side unavailable/busy/capacity)
// rather than a quota/credit exhaustion or standard concurrency cap.
func isRetryable429(message string, hasRetryAfter bool) bool {
	if isQuotaExhaustion(message) {
		return false
	}
	if hasRetryAfter {
		return true
	}
	lower := strings.ToLower(message)
	for _, term := range []string{"endpoint is unavailable", "busy", "capacity"} {
		if strings.Contains(lower, term) {
			return true
		}
	}
	return false
}

func openCodeFreeRetryableCallbackError(err error) bool {
	var noRetry openCodeFreeNonRetryable
	if errors.As(err, &noRetry) {
		return false
	}
	var classified fault.Fault
	if !errors.As(err, &classified) {
		return false
	}
	return classified.PublicCode == "upstream_empty_response" || classified.PublicCode == "upstream_protocol_error"
}

func classifyOpenCodeFreeStatus(response *http.Response, allowRetry bool) (bool, time.Duration, error) {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
	// Two views of the same body: the raw text drives the local heuristics below
	// (retry wording, relayed-provider detection), while the public message is
	// scrubbed. An upstream error body can carry internal hostnames, request URLs
	// with query credentials, or provider stack traces, so it is never forwarded
	// verbatim to a client.
	message := strings.TrimSpace(string(body))
	publicMessage := sanitizeUpstreamErrorBody(body, response.StatusCode)
	if response.StatusCode == http.StatusNotFound {
		return false, 0, &apierror.Error{
			Status: http.StatusBadGateway, Type: "server_error", Code: "upstream_model_not_found", Message: publicMessage,
		}
	}
	if response.StatusCode == http.StatusTooManyRequests || isOpenCodeFreeTransientStatus(response.StatusCode) {
		delay, hasDelay := parseRetryAfter(response.Header.Get("Retry-After"))
		if !hasDelay {
			delay = openCodeFreeRetryDelay
		}
		retry := allowRetry
		if response.StatusCode == http.StatusTooManyRequests {
			retry = allowRetry && isRetryable429(message, hasDelay)
		}
		status := response.StatusCode
		if status == http.StatusInternalServerError || status == 436 {
			status = http.StatusBadGateway
		}
		mapped := fault.New(status, fault.ScopeUpstreamGlobal, "server_error", "upstream_unavailable", publicMessage, nil)
		if retry {
			return true, delay, mapped
		}
		return false, 0, mapped
	}
	mapped := &apierror.Error{
		Status: http.StatusBadGateway, Type: "server_error", Code: "upstream_error", Message: publicMessage,
	}
	// The OpenCode provider layer wraps its own failures — transient ones
	// included — as a 400 invalid_request_error carrying "Error from provider".
	// Replaying the identical conversation succeeds moments later (measured on
	// the free tier: about one agent-loop turn in twelve), so this costs one
	// wasted call and saves the loop; the verdict stays upstream_error because a
	// genuinely rejected request must not read as retryable to the client.
	if response.StatusCode == http.StatusBadRequest && isRelayedProviderError(message) {
		delay, hasDelay := parseRetryAfter(response.Header.Get("Retry-After"))
		if !hasDelay {
			delay = openCodeFreeRetryDelay
		}
		return allowRetry, delay, mapped
	}
	return false, 0, mapped
}

// isRelayedProviderError reports whether an error body is the provider layer's
// own failure envelope rather than a complaint about the request.
func isRelayedProviderError(message string) bool {
	return strings.Contains(strings.ToLower(message), "error from provider")
}

func isOpenCodeFreeTransientStatus(status int) bool {
	switch status {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529, 436:
		return true
	default:
		return false
	}
}

// maxPublicUpstreamMessage bounds how much upstream error text reaches a client.
const maxPublicUpstreamMessage = 200

// sanitizeUpstreamErrorBody converts an upstream error body into a short, safe
// description. Only an explicit message field is extracted and scrubbed; a body
// that looks like a URL, a markup dump, or anything else unrecognized collapses
// to a generic status line rather than being echoed.
func sanitizeUpstreamErrorBody(body []byte, status int) string {
	fallback := fmt.Sprintf("OpenCodeFree upstream returned HTTP %d", status)
	text := extractUpstreamMessage(body)
	if text == "" {
		return fallback
	}
	if scrubbed := scrubUpstreamMessage(text); scrubbed != "" {
		return scrubbed
	}
	return fallback
}

func extractUpstreamMessage(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || !utf8.Valid(trimmed) {
		return ""
	}
	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(trimmed, &envelope) == nil {
		if envelope.Message != "" {
			return envelope.Message
		}
		if len(envelope.Error) > 0 {
			var nested struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(envelope.Error, &nested) == nil && nested.Message != "" {
				return nested.Message
			}
			var flat string
			if json.Unmarshal(envelope.Error, &flat) == nil && flat != "" {
				return flat
			}
		}
		return ""
	}
	// A plain-text body is only usable when it looks like prose rather than a
	// serialized structure.
	if bytes.ContainsAny(trimmed, "<>{}") {
		return ""
	}
	return string(trimmed)
}

// scrubUpstreamMessage normalizes whitespace and caps the length, rejecting
// anything that still carries a URL shape (which may embed credentials).
func scrubUpstreamMessage(message string) string {
	var out strings.Builder
	for _, character := range message {
		if character < 0x20 || character == 0x7f {
			out.WriteByte(' ')
			continue
		}
		out.WriteRune(character)
	}
	scrubbed := strings.Join(strings.Fields(out.String()), " ")
	if len(scrubbed) > maxPublicUpstreamMessage {
		scrubbed = scrubbed[:maxPublicUpstreamMessage]
	}
	if strings.Contains(scrubbed, "://") {
		return ""
	}
	return scrubbed
}
