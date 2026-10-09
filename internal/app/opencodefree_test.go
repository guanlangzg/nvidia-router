package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nvidia-router/internal/accesskey"
	"nvidia-router/internal/clock"
	"nvidia-router/internal/config"
	"nvidia-router/internal/crypto"
	"nvidia-router/internal/modelcatalog"
	"nvidia-router/tests/mocknvidia"
)

// newOpenCodeFreeTestApp builds an app whose chat route can send OpenCodeFree
// models to the given gateway. gatewayURL may be empty to simulate an
// unconfigured gateway.
func newOpenCodeFreeTestApp(t *testing.T, gatewayURL string) (*App, string) {
	t.Helper()
	db := openAppDatabase(t)
	appOwnsDB := false
	t.Cleanup(func() {
		if !appOwnsDB {
			_ = db.Close()
		}
	})
	masterKey := [32]byte{1}
	keySet, err := crypto.New(masterKey)
	if err != nil {
		t.Fatalf("crypto.New: %v", err)
	}
	accessKeys := accesskey.NewService(accesskey.NewRepository(db), keySet, clock.RealClock{})
	createdAccessKey, err := accessKeys.Create(context.Background(), "test")
	if err != nil {
		t.Fatalf("create access key: %v", err)
	}
	seedNVIDIAKeys(t, db, keySet, []string{"ocf-upstream-key"})

	nvidiaURL, err := url.Parse(mocknvidia.New(mocknvidia.Script{Status: http.StatusOK, Body: `{"id":"nv-1","choices":[{"message":{"role":"assistant","content":"nvidia"}}]}`}).URL())
	if err != nil {
		t.Fatalf("parse nvidia url: %v", err)
	}
	cfg := config.Config{
		InitialAdminPassword: testInitialAdminPassword,
		DataDir:              t.TempDir(),
		MasterKey:            masterKey,
		NVIDIABaseURL:        nvidiaURL,
	}
	if gatewayURL != "" {
		ocfURL, err := url.Parse(gatewayURL)
		if err != nil {
			t.Fatalf("parse opencodefree url: %v", err)
		}
		cfg.OpenCodeFreeBaseURL = ocfURL
	}
	application, err := New(context.Background(), Dependencies{
		Config: cfg, DB: db,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: clock.RealClock{},
		NVIDIAHTTPClient: http.DefaultClient,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	completeInitialPasswordChange(t, db)
	appOwnsDB = true
	t.Cleanup(func() { _ = application.Close() })
	return application, createdAccessKey.Plaintext
}

func seedChatModelWithProvider(t *testing.T, application *App, publicID, upstreamID, provider string) {
	t.Helper()
	repo := modelcatalog.NewRepository(application.db)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	if _, err := repo.SaveSelectionsResult(context.Background(), []modelcatalog.Selection{{
		PublicID: publicID, UpstreamID: upstreamID, DisplayName: "Free Model",
		Kind: modelcatalog.KindChat, Enabled: true, SupportsReasoning: true, ReasoningWireFormat: "openai",
	}}, now); err != nil {
		t.Fatalf("save opencodefree model: %v", err)
	}
	model, err := repo.ResolveEnabled(context.Background(), publicID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	providerCopy := provider
	if _, _, err := repo.Patch(context.Background(), model.ID, modelcatalog.Patch{Provider: &providerCopy}, now); err != nil {
		t.Fatalf("patch provider: %v", err)
	}
}

func TestOpenCodeFreeChatRoutesToGatewayAndListsInModels(t *testing.T) {
	var hits int64
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"data":[{"id":"model-free"}]}`)
			return
		}
		atomic.AddInt64(&hits, 1)
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"ocf-1","object":"chat.completion","model":"model-free","choices":[{"index":0,"message":{"role":"assistant","content":"hello from opencodefree"},"finish_reason":"stop"}]}`)
	}))
	defer gateway.Close()

	application, accessToken := newOpenCodeFreeTestApp(t, gateway.URL)
	seedChatModelWithProvider(t, application, "pub-free", "model-free", modelcatalog.ProviderOpenCodeFree)
	server := httptest.NewServer(application.Handler())
	t.Cleanup(server.Close)

	// /v1/models must expose the OpenCodeFree model now that it is routable.
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get models: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "pub-free") {
		t.Fatalf("models missing opencodefree: status=%d body=%s", resp.StatusCode, string(body))
	}

	// Non-stream chat routes to the gateway.
	reqBody := `{"model":"pub-free","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`
	req, _ = http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d body=%s", resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), "hello from opencodefree") {
		t.Fatalf("chat did not come from gateway: %s", string(body))
	}
	if atomic.LoadInt64(&hits) != 1 {
		t.Fatalf("gateway hits = %d, want 1", atomic.LoadInt64(&hits))
	}

	// Responses endpoint uses the same gateway route for the OpenCodeFree provider.
	req, _ = http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"pub-free","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("responses: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("responses status = %d body=%s", resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), "hello from opencodefree") {
		t.Fatalf("responses did not come from gateway: %s", string(body))
	}
	if atomic.LoadInt64(&hits) != 2 {
		t.Fatalf("gateway hits = %d, want 2 after Responses", atomic.LoadInt64(&hits))
	}
}

func TestOpenCodeFreeChatStreamRoutesToGateway(t *testing.T) {
	var hits int64
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"data":[{"id":"model-free"}]}`)
			return
		}
		atomic.AddInt64(&hits, 1)
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"id":"ocf-s","choices":[{"delta":{"role":"assistant","content":"part one"}}]}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, `data: {"id":"ocf-s","choices":[{"delta":{"content":" part two"},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer gateway.Close()

	application, accessToken := newOpenCodeFreeTestApp(t, gateway.URL)
	seedChatModelWithProvider(t, application, "pub-free", "model-free", modelcatalog.ProviderOpenCodeFree)
	server := httptest.NewServer(application.Handler())
	t.Cleanup(server.Close)

	reqBody := `{"model":"pub-free","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":32}`
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d body=%s", resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), "part one") || !strings.Contains(string(body), "part two") || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("stream body missing chunks: %s", string(body))
	}
	if atomic.LoadInt64(&hits) != 1 {
		t.Fatalf("gateway hits = %d, want 1", atomic.LoadInt64(&hits))
	}
}

func TestOpenCodeFreeUnconfiguredReturns503(t *testing.T) {
	// No gateway configured: routing an OpenCodeFree model must be a clear 503,
	// not an internal error or a hang.
	application, accessToken := newOpenCodeFreeTestApp(t, "")
	seedChatModelWithProvider(t, application, "pub-free", "model-free", modelcatalog.ProviderOpenCodeFree)
	server := httptest.NewServer(application.Handler())
	t.Cleanup(server.Close)

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"pub-free","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestOpenCodeFreeRetriesTransient502(t *testing.T) {
	// A gateway that answers 502 once then succeeds must still deliver the
	// response (non-stream only).
	var calls int64
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"data":[{"id":"model-free"}]}`)
			return
		}
		n := atomic.AddInt64(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":"transient"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"ocf-r","object":"chat.completion","model":"model-free","choices":[{"index":0,"message":{"role":"assistant","content":"retried ok"},"finish_reason":"stop"}]}`)
	}))
	defer gateway.Close()

	application, accessToken := newOpenCodeFreeTestApp(t, gateway.URL)
	seedChatModelWithProvider(t, application, "pub-free", "model-free", modelcatalog.ProviderOpenCodeFree)
	server := httptest.NewServer(application.Handler())
	t.Cleanup(server.Close)

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"pub-free","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "retried ok") {
		t.Fatalf("expected retried success, status=%d body=%s", resp.StatusCode, string(body))
	}
	if atomic.LoadInt64(&calls) != 2 {
		t.Fatalf("gateway calls = %d, want 2 (1 fail + 1 retry)", atomic.LoadInt64(&calls))
	}
}

func TestOpenCodeFreeRetriesTransient436(t *testing.T) {
	var calls int64
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"data":[{"id":"model-free"}]}`)
			return
		}
		if atomic.AddInt64(&calls, 1) == 1 {
			w.WriteHeader(436)
			_, _ = io.WriteString(w, `{"error":"temporary gateway capacity"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"ocf-436","object":"chat.completion","model":"model-free","choices":[{"index":0,"message":{"role":"assistant","content":"recovered"},"finish_reason":"stop"}]}`)
	}))
	defer gateway.Close()

	application, accessToken := newOpenCodeFreeTestApp(t, gateway.URL)
	seedChatModelWithProvider(t, application, "pub-free", "model-free", modelcatalog.ProviderOpenCodeFree)
	server := httptest.NewServer(application.Handler())
	defer server.Close()

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"pub-free","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "recovered") {
		t.Fatalf("expected 436 retry success, status=%d body=%s", resp.StatusCode, string(body))
	}
	if atomic.LoadInt64(&calls) != 2 {
		t.Fatalf("gateway calls = %d, want 2", calls)
	}
}

func TestOpenCodeFreeFinal436MapsToUnavailable(t *testing.T) {
	var calls int64
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"data":[{"id":"model-free"}]}`)
			return
		}
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(436)
		_, _ = io.WriteString(w, `{"error":"temporary gateway capacity"}`)
	}))
	defer gateway.Close()

	application, accessToken := newOpenCodeFreeTestApp(t, gateway.URL)
	seedChatModelWithProvider(t, application, "pub-free", "model-free", modelcatalog.ProviderOpenCodeFree)
	server := httptest.NewServer(application.Handler())
	defer server.Close()

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"pub-free","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "upstream_unavailable") {
		t.Fatalf("final 436 = status %d body=%s, want 502 upstream_unavailable", resp.StatusCode, string(body))
	}
	if atomic.LoadInt64(&calls) != 2 {
		t.Fatalf("gateway calls = %d, want 2 (initial + one retry)", calls)
	}
}

func TestOpenCodeFreeRetriesMalformedSuccessResponse(t *testing.T) {
	var calls int64
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"data":[{"id":"model-free"}]}`)
			return
		}
		if atomic.AddInt64(&calls, 1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `not-json`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"ocf-protocol-retry","object":"chat.completion","model":"model-free","choices":[{"index":0,"message":{"role":"assistant","content":"protocol retry ok"},"finish_reason":"stop"}]}`)
	}))
	defer gateway.Close()

	application, accessToken := newOpenCodeFreeTestApp(t, gateway.URL)
	seedChatModelWithProvider(t, application, "pub-free", "model-free", modelcatalog.ProviderOpenCodeFree)
	server := httptest.NewServer(application.Handler())
	t.Cleanup(server.Close)

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"pub-free","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "protocol retry ok") {
		t.Fatalf("expected protocol retry success, status=%d body=%s", resp.StatusCode, string(body))
	}
	if atomic.LoadInt64(&calls) != 2 {
		t.Fatalf("gateway calls = %d, want 2 (1 malformed + 1 retry)", atomic.LoadInt64(&calls))
	}
}

func TestOpenCodeFreeDoesNotRetry429(t *testing.T) {
	// 429 is the gateway's own concurrency limit; retrying would only add load.
	var calls int64
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"data":[{"id":"model-free"}]}`)
			return
		}
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate limited"}`)
	}))
	defer gateway.Close()

	application, accessToken := newOpenCodeFreeTestApp(t, gateway.URL)
	seedChatModelWithProvider(t, application, "pub-free", "model-free", modelcatalog.ProviderOpenCodeFree)
	server := httptest.NewServer(application.Handler())
	t.Cleanup(server.Close)

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"pub-free","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("gateway calls = %d, want 1 (no retry on 429)", atomic.LoadInt64(&calls))
	}
}

func TestOpenCodeFreePreservesRequestedReasoningEffort(t *testing.T) {
	var calls int64
	var bodies []map[string]json.RawMessage
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"data":[{"id":"model-free"}]}`)
			return
		}
		atomic.AddInt64(&calls, 1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read gateway body: %v", err)
		}
		fields := map[string]json.RawMessage{}
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatalf("decode gateway body: %v", err)
		}
		bodies = append(bodies, fields)
		if got := string(fields["reasoning_effort"]); got != `"low"` {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"reasoning_effort must be low"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"ocf-reasoning","object":"chat.completion","model":"model-free","choices":[{"index":0,"message":{"role":"assistant","content":"reasoning preserved"},"finish_reason":"stop"}]}`)
	}))
	defer gateway.Close()

	application, accessToken := newOpenCodeFreeTestApp(t, gateway.URL)
	seedChatModelWithProvider(t, application, "pub-free", "model-free", modelcatalog.ProviderOpenCodeFree)
	server := httptest.NewServer(application.Handler())
	t.Cleanup(server.Close)

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"pub-free","messages":[{"role":"user","content":"think"}],"reasoning_effort":"low","max_tokens":16}`))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "reasoning preserved") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(body))
	}
	if atomic.LoadInt64(&calls) != 1 || len(bodies) != 1 {
		t.Fatalf("gateway calls = %d bodies = %d, want 1", atomic.LoadInt64(&calls), len(bodies))
	}
	if got := string(bodies[0]["reasoning_effort"]); got != `"low"` {
		t.Fatalf("reasoning_effort = %s, want low; bodies=%#v", got, bodies)
	}
}
