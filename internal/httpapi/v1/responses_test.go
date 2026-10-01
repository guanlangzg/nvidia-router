package v1

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nvidia-router/internal/clock"
	"nvidia-router/internal/fault"
	"nvidia-router/internal/modelcatalog"
	"nvidia-router/internal/observability"
	responsesprotocol "nvidia-router/internal/protocol/responses"
	"nvidia-router/internal/router"
	"nvidia-router/internal/runtimeconfig"
	"nvidia-router/internal/sse"
	"nvidia-router/internal/upstream/nvidia"
)

// TestResponsesStreamTruncationAfterCommitLogsWarn mirrors the Chat contract:
// a Responses stream that committed its first event and then died on an
// upstream error must be logged at Warn instead of silently disappearing.
func TestResponsesStreamTruncationAfterCommitLogsWarn(t *testing.T) {
	upstream := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: &scriptedBody{
			chunks: [][]byte{[]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")},
			err:    errors.New("boom"),
		},
	}
	logger := &recordingLogHandler{}
	ctx := observability.WithRequestLogger(context.Background(), slog.New(logger))
	response := httptest.NewRecorder()
	NewResponses(nil, nil, nil).streamResponse(ctx, response, upstream, "resp_1", "public-chat")
	if !logger.contains("stream_truncated_after_commit") {
		t.Fatalf("post-commit truncation was not logged; got %v", logger.messages())
	}
}

func TestResponsesNonStreamText(t *testing.T) {
	upstreamChat := []byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(upstreamChat)
	}))
	t.Cleanup(upstream.Close)

	descriptor := nvidia.DefaultDescriptor()
	descriptor.Chat.URL = upstream.URL + "/v1/chat/completions"
	client, err := nvidia.NewClient(upstream.Client(), descriptor, testNVIDIASettings{}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	lease := &releaseTrackingLease{id: 5}
	runner := attemptRunnerFunc(func(ctx context.Context, _ int64, _ bool, execute router.ExecuteFunc) (router.AttemptResult, error) {
		response, err := execute(ctx, lease.id, []byte("upstream-secret"), &router.CommitState{})
		return router.AttemptResult{Response: response, Lease: lease, Attempts: 1}, err
	})
	resolver := modelResolverFunc(func(_ context.Context, publicID string, req modelcatalog.Requirements) (modelcatalog.Model, error) {
		if publicID != "public-chat" || req.Kind != modelcatalog.KindChat {
			t.Fatalf("resolve = %q, %#v", publicID, req)
		}
		return modelcatalog.Model{ID: 3, PublicID: "public-chat", UpstreamID: "vendor/chat", Kind: modelcatalog.KindChat, Enabled: true}, nil
	})
	handler := NewResponses(resolver, runner, client)
	body := `{"model":"public-chat","input":"hi"}`
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !lease.released {
		t.Fatal("attempt lease was not released")
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode responses body: %v; body=%s", err, response.Body.String())
	}
	if !strings.HasPrefix(result["id"].(string), "resp_") {
		t.Fatalf("id = %v, want resp_ prefix", result["id"])
	}
	if result["object"] != "response" {
		t.Fatalf("object = %v", result["object"])
	}
	if result["status"] != "completed" {
		t.Fatalf("status = %v", result["status"])
	}
	if result["model"] != "public-chat" {
		t.Fatalf("model = %v, want public-chat", result["model"])
	}
	output := result["output"].([]any)
	first := output[0].(map[string]any)
	if first["type"] != "message" {
		t.Fatalf("output type = %v", first["type"])
	}
	content := first["content"].([]any)
	if content[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("text = %v", content[0].(map[string]any)["text"])
	}
	usage := result["usage"].(map[string]any)
	if usage["input_tokens"] != float64(3) || usage["output_tokens"] != float64(2) {
		t.Fatalf("usage = %v", usage)
	}
}

func TestResponsesPassesParsedCapabilityRequirementsToModelResolver(t *testing.T) {
	var resolved modelcatalog.Requirements
	resolver := modelResolverFunc(func(_ context.Context, _ string, requirements modelcatalog.Requirements) (modelcatalog.Model, error) {
		resolved = requirements
		return modelcatalog.Model{
			ID: 3, PublicID: "public-chat", UpstreamID: "vendor/chat", Kind: modelcatalog.KindChat,
			Enabled: true, SupportsTools: true, SupportsReasoning: true, ReasoningWireFormat: "openai",
		}, nil
	})
	called := false
	runner := attemptRunnerFunc(func(context.Context, int64, bool, router.ExecuteFunc) (router.AttemptResult, error) {
		called = true
		return router.AttemptResult{Response: &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"resp_1","object":"response","status":"completed"}`))}}, nil
	})
	handler := NewResponses(resolver, runner, nil)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public-chat","input":"use lookup","tools":[{"type":"function","name":"lookup"}],"reasoning":{"effort":"low"}}`))

	handler.ServeHTTP(response, request)

	want := modelcatalog.Requirements{Kind: modelcatalog.KindChat, Tools: true, Reasoning: true}
	if resolved != want {
		t.Fatalf("resolved requirements = %+v, want %+v", resolved, want)
	}
	if !called || response.Code != http.StatusOK {
		t.Fatalf("provider path called=%v status=%d body=%s", called, response.Code, response.Body.String())
	}
}

// Reasoning is pass-through: a request that names reasoning aliases resolves and
// routes upstream even on a model the profile marks as non-reasoning or
// off-profile, so the llama-shape profile no longer produces a local 400/501.
func TestResponsesRoutesReasoningRequestWithoutProfileGate(t *testing.T) {
	called := false
	runner := attemptRunnerFunc(func(context.Context, int64, bool, router.ExecuteFunc) (router.AttemptResult, error) {
		called = true
		return router.AttemptResult{Response: &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"id":"resp_1","object":"response","status":"completed"}`))}}, nil
	})
	resolver := modelResolverFunc(func(_ context.Context, _ string, _ modelcatalog.Requirements) (modelcatalog.Model, error) {
		return modelcatalog.Model{
			ID: 3, PublicID: "public-chat", UpstreamID: "vendor/chat", Kind: modelcatalog.KindChat,
			Enabled: true, SupportsReasoning: true, ReasoningWireFormat: "openai",
			ReasoningLevels: []string{"none"}, ReasoningZeroAllowed: false,
		}, nil
	})
	handler := NewResponses(resolver, runner, nil)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(
		`{"model":"public-chat","input":"think","reasoning":{"effort":"high"}}`,
	))

	handler.ServeHTTP(response, request)

	if !called || response.Code != http.StatusOK {
		t.Fatalf("provider path called=%v status=%d body=%s", called, response.Code, response.Body.String())
	}
}

func TestResponsesNonstreamExecutionMarksSemanticCompletionAfterConversion(t *testing.T) {
	body := &semanticMarkBody{ReadCloser: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`))}
	client := &semanticMarkProvider{response: &http.Response{StatusCode: http.StatusOK, Body: body}}
	model := modelcatalog.Model{ID: 3, PublicID: "public-chat", UpstreamID: "vendor/chat", Kind: modelcatalog.KindChat, Enabled: true}
	response, err := NewResponses(nil, nil, client).execute([]byte(`{"model":"vendor/chat"}`), "resp_1", model, false)(
		context.Background(), 1, nil, &router.CommitState{},
	)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !body.completed {
		t.Fatal("valid Responses conversion did not mark semantic completion")
	}
	_ = response.Body.Close()
}

func TestResponsesNonstreamEmptySemanticOutputReturnsRetryableFault(t *testing.T) {
	client := &semanticMarkProvider{response: &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":""}}]}`)),
	}}
	model := modelcatalog.Model{ID: 3, PublicID: "public-chat", UpstreamID: "vendor/chat", Kind: modelcatalog.KindChat}
	response, err := NewResponses(nil, nil, client).execute([]byte(`{"model":"vendor/chat"}`), "resp_1", model, false)(
		context.Background(), 1, nil, &router.CommitState{},
	)
	if response != nil && response.Body != nil {
		defer func() { _ = response.Body.Close() }()
	}
	var classified fault.Fault
	if !errors.As(err, &classified) {
		t.Fatalf("execute error = %T %v, want fault.Fault", err, err)
	}
	if classified.HTTPStatus != http.StatusTooManyRequests || !classified.Retryable || classified.RetryAfter <= 0 {
		t.Fatalf("classified = %#v, want retryable 429 with Retry-After", classified)
	}
}

func TestResponsesRetriesProtocolFailureFromNonstreamConversion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Header.Get("Authorization") == "Bearer first-secret" {
			_, _ = writer.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":{"unexpected":true}}}]}`))
			return
		}
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(upstream.Close)
	descriptor := nvidia.DefaultDescriptor()
	descriptor.Chat.URL = upstream.URL + "/v1/chat/completions"
	client, err := nvidia.NewClient(upstream.Client(), descriptor, testNVIDIASettings{}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	lease := &releaseTrackingLease{id: 2}
	attempts := 0
	protocolFailures := 0
	runner := attemptRunnerFunc(func(ctx context.Context, _ int64, _ bool, execute router.ExecuteFunc) (router.AttemptResult, error) {
		var lastErr error
		for index, secret := range []string{"first-secret", "second-secret"} {
			attempts++
			response, err := execute(ctx, int64(index+1), []byte(secret), &router.CommitState{})
			if err == nil {
				return router.AttemptResult{Response: response, Lease: lease, Attempts: attempts}, nil
			}
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			var classified fault.Fault
			if !errors.As(err, &classified) || !classified.Retryable || classified.PublicCode != "upstream_protocol_error" {
				return router.AttemptResult{}, err
			}
			protocolFailures++
			lastErr = err
		}
		return router.AttemptResult{}, lastErr
	})
	resolver := modelResolverFunc(func(_ context.Context, _ string, _ modelcatalog.Requirements) (modelcatalog.Model, error) {
		return modelcatalog.Model{ID: 3, PublicID: "public-chat", UpstreamID: "vendor/chat", Kind: modelcatalog.KindChat, Enabled: true}, nil
	})
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public-chat","input":"x"}`))

	NewResponses(resolver, runner, client).ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"text":"ok"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if attempts != 2 || protocolFailures != 1 {
		t.Fatalf("attempts = %d, protocol failures = %d; want 2, 1", attempts, protocolFailures)
	}
	if !lease.released {
		t.Fatal("successful attempt lease was not released")
	}
}

func TestResponsesDoesNotRetryClientRequestError(t *testing.T) {
	attempts := 0
	runner := attemptRunnerFunc(func(context.Context, int64, bool, router.ExecuteFunc) (router.AttemptResult, error) {
		attempts++
		return router.AttemptResult{}, nil
	})
	resolver := modelResolverFunc(func(_ context.Context, _ string, _ modelcatalog.Requirements) (modelcatalog.Model, error) {
		return modelcatalog.Model{ID: 3, PublicID: "public-chat", UpstreamID: "vendor/chat", Kind: modelcatalog.KindChat, Enabled: true}, nil
	})
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public-chat","input":42}`))

	NewResponses(resolver, runner, nil).ServeHTTP(response, request)

	assertChatError(t, response, http.StatusBadRequest, "invalid_parameter")
	if attempts != 0 {
		t.Fatalf("attempts = %d, want 0", attempts)
	}
}

func TestResponsesReusesAttemptForResolvedModel(t *testing.T) {
	upstreamChat := []byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(upstreamChat)
	}))
	t.Cleanup(upstream.Close)
	descriptor := nvidia.DefaultDescriptor()
	descriptor.Chat.URL = upstream.URL + "/v1/chat/completions"
	client, _ := nvidia.NewClient(upstream.Client(), descriptor, testNVIDIASettings{}, nil)

	var resolvedModelID int64 = 42
	lease := &releaseTrackingLease{id: 9}
	runner := attemptRunnerFunc(func(ctx context.Context, modelID int64, _ bool, execute router.ExecuteFunc) (router.AttemptResult, error) {
		if modelID != resolvedModelID {
			t.Fatalf("attempt model id = %d, want %d", modelID, resolvedModelID)
		}
		response, err := execute(ctx, lease.id, []byte("secret"), &router.CommitState{})
		return router.AttemptResult{Response: response, Lease: lease, Attempts: 1}, err
	})
	resolver := modelResolverFunc(func(_ context.Context, _ string, _ modelcatalog.Requirements) (modelcatalog.Model, error) {
		return modelcatalog.Model{ID: resolvedModelID, PublicID: "public-chat", UpstreamID: "vendor/chat", Kind: modelcatalog.KindChat, Enabled: true}, nil
	})

	response := httptest.NewRecorder()
	NewResponses(resolver, runner, client).ServeHTTP(response,
		httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public-chat","input":"x"}`)))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !lease.released {
		t.Fatal("attempt lease was not released")
	}
}

func TestResponsesStreamEmitsResponsesEventSequence(t *testing.T) {
	sseBody := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"He\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	response, lease := serveStreamResponses(t, sseBody)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !lease.released {
		t.Fatal("stream attempt lease was not released")
	}
	body := response.Body.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.in_progress",
		"event: response.output_item.added",
		"event: response.content_part.added",
		"event: response.output_text.delta",
		"event: response.output_text.done",
		"event: response.content_part.done",
		"event: response.output_item.done",
		"event: response.completed",
		"data: [DONE]",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in stream response:\n%s", want, body)
		}
	}
	if strings.Count(body, "data: [DONE]") != 1 {
		t.Fatalf("[DONE] appeared %d times, want 1", strings.Count(body, "data: [DONE]"))
	}
	if !strings.Contains(body, `"input_tokens":3`) || !strings.Contains(body, `"output_tokens":2`) {
		t.Fatalf("stream usage missing from response.completed: %s", body)
	}
	if strings.Contains(body, "vendor/secret-id") || strings.Contains(body, "upstream-secret") {
		t.Fatalf("upstream secret or id leaked into stream response:\n%s", body)
	}
	ct := response.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if got := response.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want %q (audit B6: nginx must not buffer SSE)", got, "no")
	}
}

func TestResponsesDoneCompletionCallbackRunsAfterFlush(t *testing.T) {
	recorder := httptest.NewRecorder()
	called := false
	emitter := &responsesSSEEmitter{
		encoder: sse.NewEncoder(recorder),
		commit:  &router.CommitState{},
		flusher: recorder,
		onComplete: func() {
			called = true
			if !strings.Contains(recorder.Body.String(), "[DONE]") {
				t.Error("completion callback ran before [DONE] was written")
			}
		},
	}
	if err := emitter.Emit(responsesprotocol.EmittedEvent{Event: "done"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !called {
		t.Fatal("completion callback did not run")
	}
}

func TestResponsesEmitterFallsBackWhenWriteDeadlineUnsupported(t *testing.T) {
	writer := &responseDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	emitter := &responsesSSEEmitter{
		encoder:      sse.NewEncoder(writer),
		commit:       &router.CommitState{},
		flusher:      writer,
		writeTimeout: time.Second,
	}
	if err := emitter.Emit(responsesprotocol.EmittedEvent{Event: "done"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if writer.Code != http.StatusOK || !strings.Contains(writer.Body.String(), "[DONE]") {
		t.Fatalf("fallback response = %d %q, want 200 with terminal event", writer.Code, writer.Body.String())
	}
}

func TestResponsesOpenCodeFreeStreamSupportsDeadlineWriter(t *testing.T) {
	writer := &deadlineRequiredResponseWriter{ResponseRecorder: httptest.NewRecorder()}
	serveOpenCodeFreeResponsesStreamForTest(t, writer)
	if writer.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", writer.Code, writer.Body.String())
	}
	if writer.headerWithoutDeadline {
		t.Fatal("response headers were committed without a write deadline")
	}
	if writer.writes == 0 || !strings.Contains(writer.Body.String(), "event: response.completed") || !strings.Contains(writer.Body.String(), "data: [DONE]") {
		t.Fatalf("deadline-capable stream = writes:%d body:%q, want completed SSE", writer.writes, writer.Body.String())
	}
}

func TestResponsesOpenCodeFreeStreamFallsBackWithoutDeadlineWriter(t *testing.T) {
	writer := &responseDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	serveOpenCodeFreeResponsesStreamForTest(t, writer)
	if writer.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", writer.Code, writer.Body.String())
	}
	if !strings.Contains(writer.Body.String(), "event: response.completed") || !strings.Contains(writer.Body.String(), "data: [DONE]") {
		t.Fatalf("unsupported-deadline stream = %q, want completed SSE", writer.Body.String())
	}
}

func serveOpenCodeFreeResponsesStreamForTest(t *testing.T, writer http.ResponseWriter) {
	t.Helper()
	const sseBody = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"
	model := modelcatalog.Model{
		ID: 1, PublicID: "free-model", UpstreamID: "free-model",
		Kind: modelcatalog.KindChat, Provider: modelcatalog.ProviderOpenCodeFree, Enabled: true,
	}
	handler := NewResponses(nil, nil, nil).WithOpenCodeFree(openCodeFreeFunc(func(_ context.Context, _ runtimeconfig.Snapshot, _ []byte, _ bool) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sseBody)),
		}, nil
	}))
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"free-model","stream":true}`))
	handler.serveOpenCodeFreeResponses(writer, request, []byte(`{"model":"free-model","stream":true}`), "resp_ocf", model, responsesprotocol.ResponseConfig{}, true)
}

func TestResponsesEmitterRefreshesWriteDeadlineForEveryEvent(t *testing.T) {
	writer := &deadlineRequiredResponseWriter{ResponseRecorder: httptest.NewRecorder()}
	emitter := &responsesSSEEmitter{
		encoder:      sse.NewEncoder(writer),
		commit:       &router.CommitState{},
		flusher:      writer,
		writeTimeout: time.Second,
	}
	for _, event := range []responsesprotocol.EmittedEvent{
		{Event: "response.created", Data: map[string]any{}},
		{Event: "response.in_progress", Data: map[string]any{}},
	} {
		if err := emitter.Emit(event); err != nil {
			t.Fatalf("Emit(%q): %v", event.Event, err)
		}
	}
	if writer.writes < 2 {
		t.Fatalf("SSE writes = %d, want at least 2", writer.writes)
	}
}

func TestResponsesEmitterCommitSetsWriteDeadline(t *testing.T) {
	writer := &deadlineRequiredResponseWriter{ResponseRecorder: httptest.NewRecorder()}
	emitter := &responsesSSEEmitter{
		commit:       &router.CommitState{},
		flusher:      writer,
		writeTimeout: time.Second,
	}
	if err := emitter.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if writer.headerWithoutDeadline {
		t.Fatal("response headers were committed without a write deadline")
	}
}

func TestResponsesStreamWriteDeadlineDisabledWithoutBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The behavior is exercised through the shared helper: a context deadline
	// must remain available when no router budget is attached.
	if got := streamWriteDeadline(ctx); got != 0 {
		t.Fatalf("write deadline = %s, want zero without budget or context deadline", got)
	}
}

type responseDeadlineWriter struct{ *httptest.ResponseRecorder }

func (w *responseDeadlineWriter) SetWriteDeadline(time.Time) error {
	return errors.New("write deadline unsupported")
}

type deadlineRequiredResponseWriter struct {
	*httptest.ResponseRecorder
	deadlineSet           bool
	headerWithoutDeadline bool
	writes                int
}

func (w *deadlineRequiredResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlineSet = !deadline.IsZero()
	return nil
}

func (w *deadlineRequiredResponseWriter) WriteHeader(status int) {
	if !w.deadlineSet {
		w.headerWithoutDeadline = true
	}
	w.ResponseRecorder.WriteHeader(status)
}

func (w *deadlineRequiredResponseWriter) Write(payload []byte) (int, error) {
	if !w.deadlineSet {
		return 0, errors.New("response write without deadline")
	}
	w.writes++
	return w.ResponseRecorder.Write(payload)
}

func TestChatDeltaSourceJoinsMultilineEventData(t *testing.T) {
	input := "data: {\"choices\":[{\n" +
		"data: \"delta\":{\"content\":\"hello\"}\n" +
		"data: }]}\n\n" +
		"data: [DONE]\n\n"
	source := &chatDeltaSource{decoder: sse.NewDecoder(strings.NewReader(input))}

	delta, err := source.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if delta.Content != "hello" {
		t.Fatalf("content = %q, want hello", delta.Content)
	}
	if _, err := source.Next(); !errors.Is(err, responsesprotocol.ErrStreamCompleted) {
		t.Fatalf("terminal error = %v, want ErrStreamCompleted", err)
	}
}

func TestChatDeltaSourceAccumulatesReasoningLength(t *testing.T) {
	input := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"chain\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"思考\",\"content\":\"ans\"}}]}\n\n" +
		"data: [DONE]\n\n"
	source := &chatDeltaSource{decoder: sse.NewDecoder(strings.NewReader(input))}

	for {
		if _, err := source.Next(); err != nil {
			if errors.Is(err, responsesprotocol.ErrStreamCompleted) {
				break
			}
			t.Fatalf("Next: %v", err)
		}
	}
	if !source.reasoningPresent {
		t.Fatal("reasoningPresent = false, want true")
	}
	// "chain" is 5 runes, "思考" is 2 runes -> 7 total.
	if source.reasoningChars != 7 {
		t.Fatalf("reasoningChars = %d, want 7", source.reasoningChars)
	}
}

func TestResponsesNonstreamRecordsReasoning(t *testing.T) {
	upstreamChat := []byte(`{"id":"c1","choices":[{"message":{"role":"assistant","reasoning_content":"deep reasoning","content":"hello"}}]}`)
	var upstreamRequestBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read upstream request body: %v", err)
			return
		}
		upstreamRequestBody = body
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(upstreamChat)
	}))
	t.Cleanup(upstream.Close)

	descriptor := nvidia.DefaultDescriptor()
	descriptor.Chat.URL = upstream.URL + "/v1/chat/completions"
	client, err := nvidia.NewClient(upstream.Client(), descriptor, testNVIDIASettings{}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	runner := attemptRunnerFunc(func(ctx context.Context, _ int64, _ bool, execute router.ExecuteFunc) (router.AttemptResult, error) {
		response, err := execute(ctx, 1, []byte("upstream-secret"), &router.CommitState{})
		return router.AttemptResult{Response: response, Attempts: 1}, err
	})
	resolver := modelResolverFunc(func(_ context.Context, publicID string, req modelcatalog.Requirements) (modelcatalog.Model, error) {
		if publicID != "public-chat" || req.Kind != modelcatalog.KindChat {
			t.Fatalf("resolve = %q, %#v", publicID, req)
		}
		return modelcatalog.Model{
			ID: 3, PublicID: "public-chat", UpstreamID: "vendor/chat", Kind: modelcatalog.KindChat, Enabled: true,
			SupportsReasoning: true, ReasoningWireFormat: "openai", ReasoningLevels: []string{"low", "medium"},
			ReasoningMaxBudget: 8192, ReasoningDynamicAllowed: false,
		}, nil
	})
	var recorded observability.RequestRecord
	recorder := requestRecorderFunc(func(_ context.Context, record observability.RequestRecord) error {
		recorded = record
		return nil
	})
	handler := observability.HTTPMiddleware(recorder, clock.RealClock{}, slog.New(slog.NewTextHandler(io.Discard, nil)), NewResponses(resolver, runner, client))
	body := `{"model":"public-chat","input":"hi","reasoning":{"effort":"high"}}`
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !recorded.ReasoningRequested {
		t.Fatal("reasoning_requested = false, want true")
	}
	// The Responses mapping turns reasoning.effort into upstream reasoning_effort.
	if recorded.ReasoningWireFields != "reasoning_effort" {
		t.Fatalf("reasoning_wire_fields = %q, want reasoning_effort", recorded.ReasoningWireFields)
	}
	if recorded.ReasoningRequestedLevel != "high" {
		t.Fatalf("reasoning_requested_level = %q, want high", recorded.ReasoningRequestedLevel)
	}
	if recorded.ReasoningEffectiveLevel != "high" {
		t.Fatalf("reasoning_effective_level = %q, want high (pass-through)", recorded.ReasoningEffectiveLevel)
	}
	if recorded.ReasoningSource != "client" {
		t.Fatalf("reasoning_source = %q, want client", recorded.ReasoningSource)
	}
	var upstreamPayload map[string]json.RawMessage
	if err := json.Unmarshal(upstreamRequestBody, &upstreamPayload); err != nil {
		t.Fatalf("decode upstream request: %v", err)
	}
	var upstreamModel string
	if err := json.Unmarshal(upstreamPayload["model"], &upstreamModel); err != nil || upstreamModel != "vendor/chat" {
		t.Fatalf("upstream model = %q, want vendor/chat", upstreamModel)
	}
	var upstreamReasoning string
	if err := json.Unmarshal(upstreamPayload["reasoning_effort"], &upstreamReasoning); err != nil || upstreamReasoning != "high" {
		t.Fatalf("upstream reasoning_effort = %q, want the client value forwarded verbatim", upstreamReasoning)
	}
	if !recorded.ReasoningPresent {
		t.Fatal("reasoning_present = false, want true")
	}
	if recorded.ReasoningChars == nil || *recorded.ReasoningChars != 14 {
		t.Fatalf("reasoning_chars = %#v, want 14", recorded.ReasoningChars)
	}
	if recorded.RouteMode != "direct" {
		t.Fatalf("route_mode = %q, want direct", recorded.RouteMode)
	}
}

func TestResponsesStreamParallelToolCallsReleasesLease(t *testing.T) {
	sseBody := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"fc_1\",\"function\":{\"name\":\"get\",\"arguments\":\"{\\\"a\\\":1}\"}},{\"index\":1,\"id\":\"fc_2\",\"function\":{\"name\":\"send\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	response, lease := serveStreamResponses(t, sseBody)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !lease.released {
		t.Fatal("stream attempt lease was not released")
	}
	if !strings.Contains(response.Body.String(), "event: response.function_call_arguments.done") {
		t.Fatalf("missing tool arguments.done:\n%s", response.Body.String())
	}
	if strings.Count(response.Body.String(), "event: response.output_item.added") != 2 {
		t.Fatalf("expected 2 tool output_item.added events")
	}
}

func TestResponsesStreamEmitsFailedTerminalOnInterruption(t *testing.T) {
	// Upstream sends one delta then closes with neither finish_reason nor [DONE].
	sseBody := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
	response, lease := serveStreamResponses(t, sseBody)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !lease.released {
		t.Fatal("stream attempt lease was not released")
	}
	body := response.Body.String()
	if !strings.Contains(body, "event: response.failed") {
		t.Fatalf("missing response.failed terminal on interruption:\n%s", body)
	}
	if strings.Contains(body, "event: response.completed") {
		t.Fatalf("response.completed must not appear on interruption:\n%s", body)
	}
	if strings.Count(body, "data: [DONE]") != 1 {
		t.Fatalf("[DONE] appeared %d times, want 1", strings.Count(body, "data: [DONE]"))
	}
}

func TestResponsesStreamDoneCompletesWithoutFinishReason(t *testing.T) {
	sseBody := "data: {\"choices\":[{\"delta\":{\"content\":\"complete\"}}]}\n\n" +
		"data: [DONE]\n\n"
	response, _ := serveStreamResponses(t, sseBody)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "event: response.completed") || strings.Contains(body, "event: response.failed") {
		t.Fatalf("unexpected terminal events:\n%s", body)
	}
	if strings.Count(body, "data: [DONE]") != 1 {
		t.Fatalf("[DONE] appeared %d times, want 1", strings.Count(body, "data: [DONE]"))
	}
}

func TestResponsesStreamDuplicateDoneEmitsSingleTerminal(t *testing.T) {
	sseBody := "data: {\"choices\":[{\"delta\":{\"content\":\"complete\"}}]}\n\n" +
		"data: [DONE]\n\n" +
		"data: [DONE]\n\n"
	response, _ := serveStreamResponses(t, sseBody)

	body := response.Body.String()
	if strings.Count(body, "event: response.completed") != 1 || strings.Count(body, "event: response.failed") != 0 {
		t.Fatalf("terminal event counts unexpected:\n%s", body)
	}
	if strings.Count(body, "data: [DONE]") != 1 {
		t.Fatalf("[DONE] appeared %d times, want 1", strings.Count(body, "data: [DONE]"))
	}
}

func TestResponsesStreamMalformedAfterCommitEmitsFailedTerminal(t *testing.T) {
	sseBody := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n" +
		"data: {not-json}\n\n"
	response, _ := serveStreamResponses(t, sseBody)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if strings.Count(body, "event: response.failed") != 1 || strings.Count(body, "event: response.completed") != 0 {
		t.Fatalf("terminal event counts unexpected:\n%s", body)
	}
	if strings.Count(body, "data: [DONE]") != 1 {
		t.Fatalf("[DONE] appeared %d times, want 1", strings.Count(body, "data: [DONE]"))
	}
}

func TestResponsesStreamClientCancellationDoesNotEmitFailedTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	upstream := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")),
	}
	response := httptest.NewRecorder()
	NewResponses(nil, nil, nil).streamResponse(ctx, response, upstream, "resp_cancel", "public-chat")

	body := response.Body.String()
	if strings.Contains(body, "event: response.failed") {
		t.Fatalf("client cancellation must not emit response.failed:\n%s", body)
	}
	if strings.Contains(body, "event: response.completed") {
		t.Fatalf("client cancellation must not emit response.completed:\n%s", body)
	}
}

// serveStreamResponses wires a Responses handler against a scripted upstream// Chat SSE stream and returns the recorder alongside the tracked lease.
func serveStreamResponses(t *testing.T, sseBody string) (*httptest.ResponseRecorder, *releaseTrackingLease) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte(sseBody))
	}))
	t.Cleanup(upstream.Close)

	descriptor := nvidia.DefaultDescriptor()
	descriptor.Chat.URL = upstream.URL + "/v1/chat/completions"
	client, err := nvidia.NewClient(upstream.Client(), descriptor, testNVIDIASettings{}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	lease := &releaseTrackingLease{id: 5}
	runner := attemptRunnerFunc(func(ctx context.Context, _ int64, _ bool, execute router.ExecuteFunc) (router.AttemptResult, error) {
		response, err := execute(ctx, lease.id, []byte("upstream-secret"), &router.CommitState{})
		return router.AttemptResult{Response: response, Lease: lease, Attempts: 1}, err
	})
	resolver := modelResolverFunc(func(_ context.Context, _ string, _ modelcatalog.Requirements) (modelcatalog.Model, error) {
		return modelcatalog.Model{ID: 3, PublicID: "public-chat", UpstreamID: "vendor/secret-id", Kind: modelcatalog.KindChat, Enabled: true}, nil
	})
	handler := NewResponses(resolver, runner, client)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"public-chat","input":"x","stream":true}`))
	// A discard request logger keeps post-commit truncation Warn lines (a
	// deliberate signal, not test noise) out of the test output.
	request = request.WithContext(observability.WithRequestLogger(request.Context(),
		slog.New(slog.NewTextHandler(io.Discard, nil))))
	handler.ServeHTTP(response, request)
	return response, lease
}

func TestResponsesRejectsMissingModel(t *testing.T) {
	handler := NewResponses(nil, nil, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response,
		httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"x"}`)))

	assertChatError(t, response, http.StatusBadRequest, "missing_required_parameter")
}

func TestResponsesReturnsModelNotFound(t *testing.T) {
	resolver := modelResolverFunc(func(context.Context, string, modelcatalog.Requirements) (modelcatalog.Model, error) {
		return modelcatalog.Model{}, modelcatalog.ErrModelNotFound
	})
	handler := NewResponses(resolver, nil, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response,
		httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"ghost","input":"x"}`)))

	assertChatError(t, response, http.StatusNotFound, "model_not_found")
}

func TestResponsesRejectsInvalidStreamFlag(t *testing.T) {
	handler := NewResponses(nil, nil, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response,
		httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public-chat","input":"x","stream":"yes"}`)))

	assertChatError(t, response, http.StatusBadRequest, "invalid_parameter")
}

func TestResponsesDoesNotLeakUpstreamModel(t *testing.T) {
	upstreamChat := []byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(upstreamChat)
	}))
	t.Cleanup(upstream.Close)
	descriptor := nvidia.DefaultDescriptor()
	descriptor.Chat.URL = upstream.URL + "/v1/chat/completions"
	client, _ := nvidia.NewClient(upstream.Client(), descriptor, testNVIDIASettings{}, nil)
	lease := &releaseTrackingLease{id: 1}
	runner := attemptRunnerFunc(func(ctx context.Context, _ int64, _ bool, execute router.ExecuteFunc) (router.AttemptResult, error) {
		response, err := execute(ctx, lease.id, []byte("upstream-secret"), &router.CommitState{})
		return router.AttemptResult{Response: response, Lease: lease, Attempts: 1}, err
	})
	resolver := modelResolverFunc(func(_ context.Context, _ string, _ modelcatalog.Requirements) (modelcatalog.Model, error) {
		return modelcatalog.Model{ID: 1, PublicID: "public-chat", UpstreamID: "vendor/secret-id", Kind: modelcatalog.KindChat, Enabled: true}, nil
	})

	response := httptest.NewRecorder()
	NewResponses(resolver, runner, client).ServeHTTP(response,
		httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public-chat","input":"x"}`)))

	if strings.Contains(response.Body.String(), "vendor/secret-id") {
		t.Fatalf("upstream id leaked into response body: %s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "upstream-secret") {
		t.Fatalf("upstream secret leaked into response body: %s", response.Body.String())
	}
}

func TestResponsesForwardsUpstreamErrorStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(`{"error":"rate limited"}`))
	}))
	t.Cleanup(upstream.Close)
	descriptor := nvidia.DefaultDescriptor()
	descriptor.Chat.URL = upstream.URL + "/v1/chat/completions"
	client, _ := nvidia.NewClient(upstream.Client(), descriptor, testNVIDIASettings{}, nil)
	lease := &releaseTrackingLease{id: 1}
	runner := attemptRunnerFunc(func(ctx context.Context, _ int64, _ bool, execute router.ExecuteFunc) (router.AttemptResult, error) {
		response, err := execute(ctx, lease.id, []byte("secret"), &router.CommitState{})
		return router.AttemptResult{Response: response, Lease: lease, Attempts: 1}, err
	})
	resolver := modelResolverFunc(func(_ context.Context, _ string, _ modelcatalog.Requirements) (modelcatalog.Model, error) {
		return modelcatalog.Model{ID: 1, PublicID: "public-chat", UpstreamID: "vendor/chat", Kind: modelcatalog.KindChat, Enabled: true}, nil
	})

	response := httptest.NewRecorder()
	NewResponses(resolver, runner, client).ServeHTTP(response,
		httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public-chat","input":"x"}`)))

	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body = %s", response.Code, response.Body.String())
	}
}
