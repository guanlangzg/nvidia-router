package v1

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nvidia-router/internal/apierror"
	"nvidia-router/internal/fault"
)

// trackingOCFBody makes response ownership observable without retaining any
// upstream payload beyond the bounded error body read performed by the
// executor.
type trackingOCFBody struct {
	io.Reader
	closed bool
}

func (b *trackingOCFBody) Close() error {
	b.closed = true
	return nil
}

func newOCFResponse(status int, body string) (*http.Response, *trackingOCFBody) {
	tracked := &trackingOCFBody{Reader: strings.NewReader(body)}
	return &http.Response{StatusCode: status, Body: tracked}, tracked
}

func newOCFExecution(call func(context.Context, bool) (*http.Response, error), waits *int) openCodeFreeExecution {
	return openCodeFreeExecution{
		call: call,
		wait: func(context.Context) error {
			(*waits)++
			return nil
		},
	}
}

func TestOpenCodeFreeExecutionMaps404WithoutRetryAndClosesBody(t *testing.T) {
	var calls, waits int
	response, body := newOCFResponse(http.StatusNotFound, "model missing")
	execution := newOCFExecution(func(context.Context, bool) (*http.Response, error) {
		calls++
		return response, nil
	}, &waits)

	err := execution.run(context.Background(), false, &firstWriteTracker{}, func(context.Context, *http.Response) error {
		return nil
	})

	var publicErr *apierror.Error
	if !errors.As(err, &publicErr) {
		t.Fatalf("error = %T %v, want *apierror.Error", err, err)
	}
	if publicErr.Code != "upstream_model_not_found" {
		t.Fatalf("error code = %q, want upstream_model_not_found", publicErr.Code)
	}
	if calls != 1 || waits != 0 {
		t.Fatalf("calls = %d waits = %d, want 1/0", calls, waits)
	}
	if !body.closed {
		t.Fatal("404 response body was not closed")
	}
}

func TestOpenCodeFreeExecutionMaps429WithoutRetryAndClosesBody(t *testing.T) {
	var calls, waits int
	response, body := newOCFResponse(http.StatusTooManyRequests, "busy")
	execution := newOCFExecution(func(context.Context, bool) (*http.Response, error) {
		calls++
		return response, nil
	}, &waits)

	err := execution.run(context.Background(), false, &firstWriteTracker{}, func(context.Context, *http.Response) error {
		return nil
	})

	var classified fault.Fault
	if !errors.As(err, &classified) {
		t.Fatalf("error = %T %v, want fault.Fault", err, err)
	}
	if classified.PublicCode != "upstream_unavailable" || classified.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("fault = %#v, want 429 upstream_unavailable", classified)
	}
	if calls != 1 || waits != 0 {
		t.Fatalf("calls = %d waits = %d, want 1/0", calls, waits)
	}
	if !body.closed {
		t.Fatal("429 response body was not closed")
	}
}

func TestOpenCodeFreeExecutionMaps436ToBadGateway(t *testing.T) {
	var calls, waits int
	response, body := newOCFResponse(436, "gateway busy")
	execution := newOCFExecution(func(context.Context, bool) (*http.Response, error) {
		calls++
		return response, nil
	}, &waits)

	err := execution.run(context.Background(), false, &firstWriteTracker{}, func(context.Context, *http.Response) error {
		return nil
	})

	var classified fault.Fault
	if !errors.As(err, &classified) {
		t.Fatalf("error = %T %v, want fault.Fault", err, err)
	}
	if classified.HTTPStatus != http.StatusBadGateway || classified.PublicCode != "upstream_unavailable" {
		t.Fatalf("fault = %#v, want 502 upstream_unavailable", classified)
	}
	if calls != 2 || waits != 1 {
		t.Fatalf("calls = %d waits = %d, want 2/1", calls, waits)
	}
	if !body.closed {
		t.Fatal("436 response body was not closed")
	}
}

func TestOpenCodeFreeExecutionReplaysRelayedProviderErrorOnce(t *testing.T) {
	const relayed = `{"error":{"type":"invalid_request_error","message":"Error from provider (Console): Upstream request failed: [invalid_request_error] invalid request"}}`
	var calls, waits int
	execution := newOCFExecution(func(context.Context, bool) (*http.Response, error) {
		calls++
		if calls == 1 {
			response, _ := newOCFResponse(http.StatusBadRequest, relayed)
			return response, nil
		}
		response, _ := newOCFResponse(http.StatusOK, "ok")
		return response, nil
	}, &waits)

	err := execution.run(context.Background(), false, &firstWriteTracker{}, func(context.Context, *http.Response) error {
		return nil
	})
	if err != nil {
		t.Fatalf("run error = %v, want the replay to succeed", err)
	}
	if calls != 2 || waits != 1 {
		t.Fatalf("calls = %d waits = %d, want 2/1", calls, waits)
	}
}

// A plain client-style 400 is still terminal, and a replayed provider error
// that repeats must keep the original upstream_error verdict.
func TestOpenCodeFreeExecutionDoesNotReplayOrdinaryBadRequest(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		body      string
		wantCalls int
	}{
		{name: "provider envelope repeats", status: http.StatusBadRequest,
			body:      `{"error":{"message":"Error from provider (Console): Upstream request failed: [invalid_request_error] invalid request"}}`,
			wantCalls: 2},
		{name: "request complaint", status: http.StatusBadRequest,
			body:      `{"error":{"message":"tools must be an array"}}`,
			wantCalls: 1},
		{name: "unprocessable", status: http.StatusUnprocessableEntity,
			body:      `{"error":{"message":"model is unavailable"}}`,
			wantCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls, waits int
			execution := newOCFExecution(func(context.Context, bool) (*http.Response, error) {
				calls++
				response, _ := newOCFResponse(test.status, test.body)
				return response, nil
			}, &waits)

			err := execution.run(context.Background(), false, &firstWriteTracker{}, func(context.Context, *http.Response) error {
				return nil
			})
			var publicErr *apierror.Error
			if !errors.As(err, &publicErr) {
				t.Fatalf("error = %T %v, want *apierror.Error", err, err)
			}
			if publicErr.Status != http.StatusBadGateway || publicErr.Code != "upstream_error" {
				t.Fatalf("error = %#v, want 502 upstream_error", publicErr)
			}
			if calls != test.wantCalls {
				t.Fatalf("calls = %d, want %d", calls, test.wantCalls)
			}
		})
	}
}

func TestOpenCodeFreeExecutionRetries503OnceThenSucceeds(t *testing.T) {
	var calls, waits int
	first, firstBody := newOCFResponse(http.StatusServiceUnavailable, "retry")
	second, secondBody := newOCFResponse(http.StatusOK, "ok")
	execution := newOCFExecution(func(_ context.Context, _ bool) (*http.Response, error) {
		calls++
		if calls == 1 {
			return first, nil
		}
		return second, nil
	}, &waits)

	err := execution.run(context.Background(), false, &firstWriteTracker{}, func(_ context.Context, response *http.Response) error {
		if response != second {
			t.Fatalf("callback response = %p, want second response %p", response, second)
		}
		return nil
	})

	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if calls != 2 || waits != 1 {
		t.Fatalf("calls = %d waits = %d, want 2/1", calls, waits)
	}
	if !firstBody.closed || !secondBody.closed {
		t.Fatalf("response bodies closed = %v/%v, want true/true", firstBody.closed, secondBody.closed)
	}
}

func TestOpenCodeFreeExecutionStopsAfterSecond503(t *testing.T) {
	var calls, waits int
	responses := make([]*trackingOCFBody, 0, 2)
	execution := newOCFExecution(func(context.Context, bool) (*http.Response, error) {
		calls++
		response, body := newOCFResponse(http.StatusBadGateway, "retry")
		responses = append(responses, body)
		return response, nil
	}, &waits)

	err := execution.run(context.Background(), false, &firstWriteTracker{}, func(context.Context, *http.Response) error {
		return nil
	})

	var classified fault.Fault
	if !errors.As(err, &classified) {
		t.Fatalf("error = %T %v, want fault.Fault", err, err)
	}
	if classified.PublicCode != "upstream_unavailable" {
		t.Fatalf("fault code = %q, want upstream_unavailable", classified.PublicCode)
	}
	if calls != 2 || waits != 1 {
		t.Fatalf("calls = %d waits = %d, want 2/1", calls, waits)
	}
	for index, body := range responses {
		if !body.closed {
			t.Errorf("response body %d was not closed", index)
		}
	}
}

func TestOpenCodeFreeExecutionRetriesEmptyOrProtocolSuccessOnce(t *testing.T) {
	tests := []struct {
		name  string
		fault func() error
	}{
		{name: "empty", fault: func() error { return fault.EmptyResponse(io.ErrUnexpectedEOF) }},
		{name: "protocol", fault: func() error { return fault.Protocol(errors.New("malformed")) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls, waits int
			first, firstBody := newOCFResponse(http.StatusOK, "bad")
			second, secondBody := newOCFResponse(http.StatusOK, "good")
			execution := newOCFExecution(func(_ context.Context, _ bool) (*http.Response, error) {
				calls++
				if calls == 1 {
					return first, nil
				}
				return second, nil
			}, &waits)

			err := execution.run(context.Background(), false, &firstWriteTracker{}, func(_ context.Context, response *http.Response) error {
				if response == first {
					return test.fault()
				}
				return nil
			})

			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if calls != 2 || waits != 1 {
				t.Fatalf("calls = %d waits = %d, want 2/1", calls, waits)
			}
			if !firstBody.closed || !secondBody.closed {
				t.Fatalf("response bodies closed = %v/%v, want true/true", firstBody.closed, secondBody.closed)
			}
		})
	}
}

func TestOpenCodeFreeExecutionReturnsProtocolAfterRetryExhausted(t *testing.T) {
	var calls, waits int
	responses := make([]*trackingOCFBody, 0, 2)
	execution := newOCFExecution(func(context.Context, bool) (*http.Response, error) {
		calls++
		response, body := newOCFResponse(http.StatusOK, "malformed")
		responses = append(responses, body)
		return response, nil
	}, &waits)

	err := execution.run(context.Background(), false, &firstWriteTracker{}, func(context.Context, *http.Response) error {
		return fault.Protocol(errors.New("malformed"))
	})

	var classified fault.Fault
	if !errors.As(err, &classified) || classified.PublicCode != "upstream_protocol_error" {
		t.Fatalf("error = %T %#v, want upstream_protocol_error fault", err, err)
	}
	if calls != 2 || waits != 1 {
		t.Fatalf("calls = %d waits = %d, want 2/1", calls, waits)
	}
	for index, body := range responses {
		if !body.closed {
			t.Errorf("response body %d was not closed", index)
		}
	}
}

func TestOpenCodeFreeExecutionRetriesStreamProtocolBeforeFirstWrite(t *testing.T) {
	var calls, waits int
	response, firstBody := newOCFResponse(http.StatusOK, "partial")
	second, secondBody := newOCFResponse(http.StatusOK, "complete")
	tracker := &firstWriteTracker{ResponseWriter: httptest.NewRecorder()}
	execution := newOCFExecution(func(_ context.Context, _ bool) (*http.Response, error) {
		calls++
		if calls == 1 {
			return response, nil
		}
		return second, nil
	}, &waits)

	err := execution.run(context.Background(), true, tracker, func(_ context.Context, upstream *http.Response) error {
		if upstream == response {
			return fault.Protocol(errors.New("no semantic event"))
		}
		tracker.WriteHeader(http.StatusOK)
		return nil
	})

	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if calls != 2 || waits != 1 {
		t.Fatalf("calls = %d waits = %d, want 2/1", calls, waits)
	}
	if !firstBody.closed || !secondBody.closed {
		t.Fatalf("response bodies closed = %v/%v, want true/true", firstBody.closed, secondBody.closed)
	}
}

func TestOpenCodeFreeExecutionStopsQuietlyWhenRetryContextCancels(t *testing.T) {
	var calls int
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	response, body := newOCFResponse(http.StatusServiceUnavailable, "retry")
	execution := openCodeFreeExecution{
		call: func(context.Context, bool) (*http.Response, error) {
			calls++
			return response, nil
		},
		wait: func(context.Context) error {
			cancel()
			return context.Canceled
		},
	}

	err := execution.run(parent, false, &firstWriteTracker{}, func(context.Context, *http.Response) error {
		return nil
	})

	if err != nil {
		t.Fatalf("run: %v, want quiet cancellation", err)
	}
	if calls != 1 || !body.closed {
		t.Fatalf("calls = %d body closed = %v, want 1/true", calls, body.closed)
	}
}

func TestOpenCodeFreeExecutionDoesNotRetryStreamAfterWrite(t *testing.T) {
	var calls, waits int
	response, body := newOCFResponse(http.StatusOK, "partial")
	tracker := &firstWriteTracker{ResponseWriter: httptest.NewRecorder()}
	execution := newOCFExecution(func(context.Context, bool) (*http.Response, error) {
		calls++
		return response, nil
	}, &waits)

	err := execution.run(context.Background(), true, tracker, func(_ context.Context, _ *http.Response) error {
		tracker.WriteHeader(http.StatusOK)
		return fault.Protocol(errors.New("stream interrupted"))
	})

	var classified fault.Fault
	if !errors.As(err, &classified) || classified.PublicCode != "upstream_protocol_error" {
		t.Fatalf("error = %T %#v, want upstream_protocol_error fault", err, err)
	}
	if calls != 1 || waits != 0 {
		t.Fatalf("calls = %d waits = %d, want 1/0", calls, waits)
	}
	if !body.closed {
		t.Fatal("stream response body was not closed")
	}
}
