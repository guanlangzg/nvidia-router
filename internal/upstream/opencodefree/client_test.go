package opencodefree

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"nvidia-router/internal/runtimeconfig"
)

func TestModelsPrioritizeFreeEntriesAndPreserveOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/models" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer local-entry-key" {
			t.Fatalf("authorization = %q", got)
		}
		_, _ = io.WriteString(writer, `{"data":[{"id":"regular-a"},{"id":"model-free"},{"id":"regular-b"},{"id":"MODEL-FREE"},{"id":"regular-a"}]}`)
	}))
	t.Cleanup(server.Close)
	baseURL, err := url.Parse(server.URL + "/v1")
	if err != nil {
		t.Fatalf("parse base URL: %v", err)
	}
	client, err := NewClient(server.Client(), baseURL, "local-entry-key")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	models, err := client.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	want := []string{"model-free", "MODEL-FREE", "regular-a", "regular-b"}
	if strings.Join(models, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("models = %v, want %v", models, want)
	}
}

func TestChatOmitsOptionalEntryAuthorization(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "" {
			t.Fatalf("authorization = %q, want empty", got)
		}
		if got := request.Header.Get("x-opencode-client"); got != "desktop" {
			t.Fatalf("x-opencode-client = %q", got)
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	t.Cleanup(server.Close)
	baseURL, err := url.Parse(server.URL + "/v1")
	if err != nil {
		t.Fatalf("parse base URL: %v", err)
	}
	client, err := NewClient(server.Client(), baseURL, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	response, err := client.Chat(context.Background(), runtimeconfig.Snapshot{}, []byte(`{"model":"model-free"}`), false)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
		defer func() { _ = response.Body.Close() }()
	}

func TestClientSetsOpencodeUserAgentAndSessionHeader(t *testing.T) {
	var gotUA, gotSession, gotClient string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotUA = request.Header.Get("User-Agent")
		gotSession = request.Header.Get("x-opencode-session")
		gotClient = request.Header.Get("x-opencode-client")
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	t.Cleanup(server.Close)
	baseURL, err := url.Parse(server.URL + "/v1")
	if err != nil {
		t.Fatalf("parse base URL: %v", err)
	}
	client, err := NewClient(server.Client(), baseURL, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	response, err := client.Chat(context.Background(), runtimeconfig.Snapshot{}, []byte(`{"model":"model-free"}`), false)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_ = response.Body.Close()

	if gotUA != DefaultUserAgent {
		t.Errorf("got User-Agent = %q, want %q", gotUA, DefaultUserAgent)
	}
	if gotClient != "desktop" {
		t.Errorf("got x-opencode-client = %q, want desktop", gotClient)
	}
	if !IsValidSessionID(gotSession) {
		t.Errorf("got invalid x-opencode-session = %q (len=%d)", gotSession, len(gotSession))
	}
}

func TestClientUserAgentOverrideViaEnv(t *testing.T) {
	const customUA = "opencode/1.25.0-custom"
	t.Setenv(UserAgentEnvVar, customUA)

	var gotUA string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotUA = request.Header.Get("User-Agent")
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	t.Cleanup(server.Close)
	baseURL, err := url.Parse(server.URL + "/v1")
	if err != nil {
		t.Fatalf("parse base URL: %v", err)
	}
	client, err := NewClient(server.Client(), baseURL, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	response, err := client.Chat(context.Background(), runtimeconfig.Snapshot{}, []byte(`{"model":"model-free"}`), false)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_ = response.Body.Close()

	if gotUA != customUA {
		t.Errorf("got User-Agent = %q, want %q", gotUA, customUA)
	}
}

func TestClientUserAgentForwardedFromContext(t *testing.T) {
	var gotUA string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotUA = request.Header.Get("User-Agent")
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	t.Cleanup(server.Close)
	baseURL, err := url.Parse(server.URL + "/v1")
	if err != nil {
		t.Fatalf("parse base URL: %v", err)
	}
	client, err := NewClient(server.Client(), baseURL, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// 1. With official opencode/* UA from context
	ctxWithOpencode := WithUserAgent(context.Background(), "opencode/1.19.2")
	response, err := client.Chat(ctxWithOpencode, runtimeconfig.Snapshot{}, []byte(`{"model":"model-free"}`), false)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_ = response.Body.Close()
	if gotUA != "opencode/1.19.2" {
		t.Errorf("got User-Agent = %q, want opencode/1.19.2", gotUA)
	}

	// 2. With non-opencode UA (should fall back to DefaultUserAgent)
	ctxWithGeneric := WithUserAgent(context.Background(), "curl/7.88.1")
	response, err = client.Chat(ctxWithGeneric, runtimeconfig.Snapshot{}, []byte(`{"model":"model-free"}`), false)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_ = response.Body.Close()
	if gotUA != DefaultUserAgent {
		t.Errorf("got User-Agent = %q, want %q", gotUA, DefaultUserAgent)
	}
}

func TestClientSessionAffinityStableAcrossMultipleTurns(t *testing.T) {
	sessionID, err := GenerateSessionID()
	if err != nil {
		t.Fatalf("GenerateSessionID: %v", err)
	}
	receivedSessions := make([]string, 0, 3)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		receivedSessions = append(receivedSessions, request.Header.Get("x-opencode-session"))
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	t.Cleanup(server.Close)
	baseURL, err := url.Parse(server.URL + "/v1")
	if err != nil {
		t.Fatalf("parse base URL: %v", err)
	}
	client, err := NewClient(server.Client(), baseURL, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx := WithSession(context.Background(), sessionID)
	for i := 0; i < 3; i++ {
		resp, err := client.Chat(ctx, runtimeconfig.Snapshot{}, []byte(`{"model":"model-free"}`), false)
		if err != nil {
			t.Fatalf("Chat turn %d: %v", i, err)
		}
		_ = resp.Body.Close()
	}

	if len(receivedSessions) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(receivedSessions))
	}
	for i, s := range receivedSessions {
		if s != sessionID {
			t.Errorf("turn %d got session %q, want %q", i, s, sessionID)
		}
	}
}

func TestClientGeneratesDistinctSessionsForParallelCalls(t *testing.T) {
	const count = 50
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	t.Cleanup(server.Close)
	baseURL, err := url.Parse(server.URL + "/v1")
	if err != nil {
		t.Fatalf("parse base URL: %v", err)
	}
	client, err := NewClient(server.Client(), baseURL, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	seen := make(map[string]struct{}, count)
	for i := 0; i < count; i++ {
		req, err := client.newRequest(context.Background(), http.MethodPost, "/chat/completions", []byte(`{}`), false)
		if err != nil {
			t.Fatalf("newRequest %d: %v", i, err)
		}
		sess := req.Header.Get("x-opencode-session")
		if !IsValidSessionID(sess) {
			t.Fatalf("invalid session format %q", sess)
		}
		if _, exists := seen[sess]; exists {
			t.Fatalf("collision detected for session ID %q", sess)
		}
		seen[sess] = struct{}{}
	}
}

func TestIsValidSessionID(t *testing.T) {
	valid, err := GenerateSessionID()
	if err != nil {
		t.Fatalf("GenerateSessionID: %v", err)
	}
	if !IsValidSessionID(valid) {
		t.Fatalf("expected valid session for %q", valid)
	}

	invalidCases := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"wrong prefix", "abc_0123456789abcdefghijklmn"},
		{"too short", "ses_0123456789ab"},
		{"too long", "ses_0123456789abcdefghijklmnop_toolong"},
		{"invalid hex chars", "ses_0123456789GZabcdefghijklmn"},
		{"invalid base62 chars", "ses_0123456789ababcdefghijk---"},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			if IsValidSessionID(tc.id) {
				t.Errorf("IsValidSessionID(%q) = true, want false", tc.id)
			}
		})
	}
}

func TestWithForwardedHeaders(t *testing.T) {
	sess, _ := GenerateSessionID()
	hdr := make(http.Header)
	hdr.Set("x-opencode-session", sess)
	hdr.Set("User-Agent", "opencode/1.18.31")

	ctx := WithForwardedHeaders(context.Background(), hdr)
	gotSess, ok := SessionFrom(ctx)
	if !ok || gotSess != sess {
		t.Errorf("got session = %q (ok=%v), want %q", gotSess, ok, sess)
	}
	gotUA, ok := UserAgentFrom(ctx)
	if !ok || gotUA != "opencode/1.18.31" {
		t.Errorf("got UA = %q (ok=%v), want opencode/1.18.31", gotUA, ok)
	}
}
