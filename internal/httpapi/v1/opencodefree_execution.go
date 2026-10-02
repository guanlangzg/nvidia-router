package v1

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"nvidia-router/internal/apierror"
	"nvidia-router/internal/fault"
)

const openCodeFreeRetryDelay = 500 * time.Millisecond

// openCodeFreeExecution owns the lifecycle shared by the Chat and Responses
// adapters. The adapter keeps protocol parsing and response writing; this
// component only decides whether an attempt can be replayed and closes every
// upstream response it receives.
type openCodeFreeExecution struct {
	call func(context.Context, bool) (*http.Response, error)
	wait func(context.Context) error
}

type openCodeFreeNonRetryable struct {
	err error
}

func (e openCodeFreeNonRetryable) Error() string { return e.err.Error() }

func (e openCodeFreeNonRetryable) Unwrap() error { return e.err }

func (e openCodeFreeNonRetryable) noOpenCodeFreeRetry() {}

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
		ctx, cancel := context.WithTimeout(parent, openCodeFreeRequestTimeout)
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
			retry, mapped := classifyOpenCodeFreeStatus(response, attempt == 0)
			_ = response.Body.Close()
			cancel()
			if retry && !tracker.wrote {
				if err := e.waitForRetry(parent); err != nil {
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
			if err := e.waitForRetry(parent); err != nil {
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

func (e openCodeFreeExecution) waitForRetry(ctx context.Context) error {
	if e.wait != nil {
		return e.wait(ctx)
	}
	timer := time.NewTimer(openCodeFreeRetryDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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

func classifyOpenCodeFreeStatus(response *http.Response, allowRetry bool) (bool, error) {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = fmt.Sprintf("OpenCodeFree upstream returned HTTP %d", response.StatusCode)
	} else if len(message) > 512 {
		message = message[:512]
	}
	if response.StatusCode == http.StatusNotFound {
		return false, &apierror.Error{
			Status: http.StatusBadGateway, Type: "server_error", Code: "upstream_model_not_found", Message: message,
		}
	}
	if response.StatusCode == http.StatusTooManyRequests || isOpenCodeFreeTransientStatus(response.StatusCode) {
		retry := allowRetry && response.StatusCode != http.StatusTooManyRequests
		if retry {
			return true, nil
		}
		status := response.StatusCode
		if status == http.StatusInternalServerError || status == 436 {
			status = http.StatusBadGateway
		}
		return false, fault.New(status, fault.ScopeUpstreamGlobal, "server_error", "upstream_unavailable", message, nil)
	}
	mapped := &apierror.Error{
		Status: http.StatusBadGateway, Type: "server_error", Code: "upstream_error", Message: message,
	}
	// The OpenCode provider layer wraps its own failures — transient ones
	// included — as a 400 invalid_request_error carrying "Error from provider".
	// Replaying the identical conversation succeeds moments later (measured on
	// the free tier: about one agent-loop turn in twelve), so this costs one
	// wasted call and saves the loop; the verdict stays upstream_error because a
	// genuinely rejected request must not read as retryable to the client.
	if response.StatusCode == http.StatusBadRequest && isRelayedProviderError(message) {
		return allowRetry, mapped
	}
	return false, mapped
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
