// Package opencodefree provides the discovery and read-only probe surface for
// the local OpenCode Free gateway. It is deliberately not a production router
// provider: callers must opt into each operation explicitly.
package opencodefree

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nvidia-router/internal/runtimeconfig"
	"nvidia-router/internal/xkproxy"
)

const maxResponseBytes = 4 << 20

const (
	// proxyAttempts bounds how many pooled exits one request may dial. Three
	// covers a dead exit plus one genuinely flaky retry without turning a broken
	// gateway into a long stall.
	proxyAttempts = 3
	// proxyWaitBudget is how long one attempt waits for the pool to publish a
	// healthy exit before giving up. The collector runs on a ~5s cycle, so this
	// spans roughly two cycles.
	proxyWaitBudget = 10 * time.Second
)

const (
	// DefaultUserAgent is the standard User-Agent header expected by OpenCodeFree upstreams.
	DefaultUserAgent = "opencode/1.18.31"
	// UserAgentEnvVar allows overriding DefaultUserAgent via environment variable.
	UserAgentEnvVar = "OPENCODE_USER_AGENT"
	base62Alphabet  = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

type sessionCtxKey struct{}
type userAgentCtxKey struct{}

// WithSession associates an OpenCodeFree session ID with the context for session affinity.
func WithSession(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionCtxKey{}, sessionID)
}

// SessionFrom retrieves the session ID stored in the context, if present.
func SessionFrom(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	val, ok := ctx.Value(sessionCtxKey{}).(string)
	return val, ok
}

// WithUserAgent associates a custom User-Agent with the context.
func WithUserAgent(ctx context.Context, userAgent string) context.Context {
	return context.WithValue(ctx, userAgentCtxKey{}, userAgent)
}

// UserAgentFrom retrieves the custom User-Agent from context, if present.
func UserAgentFrom(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	val, ok := ctx.Value(userAgentCtxKey{}).(string)
	return val, ok
}

// WithForwardedHeaders extracts valid x-opencode-session and User-Agent from headers
// into the context.
func WithForwardedHeaders(ctx context.Context, headers http.Header) context.Context {
	if headers == nil {
		return ctx
	}
	if session := strings.TrimSpace(headers.Get("x-opencode-session")); IsValidSessionID(session) {
		ctx = WithSession(ctx, session)
	}
	if ua := strings.TrimSpace(headers.Get("User-Agent")); ua != "" {
		ctx = WithUserAgent(ctx, ua)
	}
	return ctx
}

// IsValidSessionID verifies whether a session ID strictly matches ^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$ (30 chars).
func IsValidSessionID(id string) bool {
	if len(id) != 30 || !strings.HasPrefix(id, "ses_") {
		return false
	}
	for i := 4; i < 16; i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	for i := 16; i < 30; i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// GenerateSessionID generates a collision-resistant 30-char session ID.
func GenerateSessionID() (string, error) {
	var hexBytes [6]byte
	if _, err := rand.Read(hexBytes[:]); err != nil {
		return "", fmt.Errorf("generate session hex: %w", err)
	}
	var b62Result [14]byte
	var randomByte [1]byte
	for i := 0; i < 14; {
		if _, err := rand.Read(randomByte[:]); err != nil {
			return "", fmt.Errorf("generate session base62: %w", err)
		}
		// 62 * 4 = 248. Reject >= 248 for unbiased uniform sampling across 62 symbols.
		if randomByte[0] < 248 {
			b62Result[i] = base62Alphabet[randomByte[0]%62]
			i++
		}
	}
	return fmt.Sprintf("ses_%s%s", hex.EncodeToString(hexBytes[:]), string(b62Result[:])), nil
}

func resolveUserAgent(ctx context.Context) string {
	if override := strings.TrimSpace(os.Getenv(UserAgentEnvVar)); override != "" {
		return override
	}
	if ctx != nil {
		if forwardedUA, ok := UserAgentFrom(ctx); ok && strings.HasPrefix(forwardedUA, "opencode/") {
			return forwardedUA
		}
	}
	return DefaultUserAgent
}

var ErrProtocol = errors.New("OpenCodeFree protocol error")

type Client struct {
	httpClient *http.Client
	baseURL    string
	authKey    string
	proxy      xkproxy.Provider
	// session pins this process to one pooled exit. The gateway sees a single
	// fixed bearer token, so letting every request hop to another IP would make
	// the account look like it is being shared across hosts — a far more likely
	// way to attract attention than a stable exit. A new exit is only picked when
	// the current one fails or its lease expires. The label is random per process
	// so the proxy vendor cannot correlate our sessions across restarts.
	session string
	// local marks a gateway that only this host or its private network can
	// reach, which no exit proxy can dial. See isLocalHost.
	local bool
}

func NewClient(httpClient *http.Client, baseURL *url.URL, authKey string) (*Client, error) {
	if httpClient == nil {
		return nil, errors.New("new OpenCodeFree client: HTTP client is required")
	}
	if baseURL == nil || baseURL.Host == "" || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, errors.New("new OpenCodeFree client: base URL must be an absolute HTTP or HTTPS URL without credentials, query, or fragment")
	}
	session := make([]byte, 16)
	if _, err := rand.Read(session); err != nil {
		return nil, fmt.Errorf("new OpenCodeFree client: generate proxy session label: %w", err)
	}
	return &Client{
		httpClient: httpClient,
		baseURL:    strings.TrimRight(baseURL.String(), "/"),
		authKey:    strings.TrimSpace(authKey),
		session:    hex.EncodeToString(session),
		local:      isLocalHost(baseURL.Hostname()),
	}, nil
}

// isLocalHost reports whether the gateway lives on this host or inside the
// private network the router itself runs in. An exit proxy exists to hide our
// origin address from a public endpoint; pointing one at an address only
// reachable from here cannot work — the exit has no route to it — and the
// vendor answers with its own non-standard status, which the router would then
// report as a broken gateway while also penalising a perfectly healthy exit.
func isLocalHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return true
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	if address := net.ParseIP(host); address != nil {
		return address.IsLoopback() || address.IsPrivate() ||
			address.IsLinkLocalUnicast() || address.IsUnspecified()
	}
	// A single-label name (no dot) is not resolvable on the public internet: it
	// is a container or LAN alias, e.g. a Compose service name.
	return !strings.Contains(host, ".")
}

// WithProxy routes every gateway call through the built-in proxy pool. Without
// it the client dials the gateway directly.
func (c *Client) WithProxy(provider xkproxy.Provider) *Client {
	if c == nil || provider == nil {
		return c
	}
	c.proxy = provider
	return c
}

func (c *Client) Models(ctx context.Context) ([]string, error) {
	response, err := c.do(ctx, runtimeconfig.Snapshot{}, http.MethodGet, "/models", nil, false)
	if err != nil {
		return nil, fmt.Errorf("request OpenCodeFree models: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("OpenCodeFree models returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read OpenCodeFree models: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("%w: model response exceeds %d bytes", ErrProtocol, maxResponseBytes)
	}
	return parseModels(body)
}

func (c *Client) Chat(ctx context.Context, snapshot runtimeconfig.Snapshot, body []byte, stream bool) (*http.Response, error) {
	normalizedBody, err := normalizeChatBody(body)
	if err != nil {
		return nil, fmt.Errorf("normalize OpenCodeFree chat body: %w", err)
	}
	response, err := c.do(ctx, snapshot, http.MethodPost, "/chat/completions", normalizedBody, stream)
	if err != nil {
		return nil, fmt.Errorf("request OpenCodeFree chat: %w", err)
	}
	if stream {
		return response, nil
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return response, nil
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if !strings.Contains(contentType, "text/event-stream") {
		return response, nil
	}
	defer func() { _ = response.Body.Close() }()
	aggregatedJSON, err := aggregateSseCompletion(response.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: aggregate OpenCodeFree SSE: %v", ErrProtocol, err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Proto:      response.Proto,
		ProtoMajor: response.ProtoMajor,
		ProtoMinor: response.ProtoMinor,
		Header: http.Header{
			"Content-Type":  []string{"application/json; charset=utf-8"},
			"Cache-Control": []string{"no-store"},
		},
		Body:          io.NopCloser(bytes.NewReader(aggregatedJSON)),
		ContentLength: int64(len(aggregatedJSON)),
		Request:       response.Request,
	}, nil
}

// do sends one gateway call, through the proxy pool when one is wired. A pooled
// attempt that never produced a response retires its exit and dials another, so
// a dead or throttled IP costs a retry instead of the whole request. An attempt
// that already reached the gateway is never replayed: the gateway may have
// accepted it and a second copy would double the call.
func (c *Client) do(ctx context.Context, snapshot runtimeconfig.Snapshot, method, path string, body []byte, stream bool) (*http.Response, error) {
	if sessionID, ok := SessionFrom(ctx); !ok || !IsValidSessionID(sessionID) {
		if generated, err := GenerateSessionID(); err == nil {
			ctx = WithSession(ctx, generated)
		}
	}
	// A gateway on this host or the private network is unreachable from an exit,
	// so no proxy can dial it: that case stays direct. Every public upstream must
	// leave through the pool — the upstream applies per-IP rate limits, so a
	// single origin address attracts 429s that a rotating exit set spreads out.
	// Falling back to a direct dial when the pool is unconfigured or disabled
	// would silently leak the host address and re-introduce those limits, so it
	// is an explicit failure instead.
	if c.local {
		request, err := c.newRequest(ctx, method, path, body, stream)
		if err != nil {
			return nil, err
		}
		return c.httpClient.Do(request)
	}
	if c.proxy == nil || !c.proxy.Configured() {
		return nil, xkproxy.NewTransportError(errors.New("OpenCodeFree requires the proxy pool to be configured"))
	}
	if !c.proxy.Enabled() {
		return nil, xkproxy.NewTransportError(errors.New("proxy is disabled"))
	}
	var lastErr error
	for attempt := 0; attempt < proxyAttempts; attempt++ {
		request, err := c.newRequest(ctx, method, path, body, stream)
		if err != nil {
			return nil, err
		}
		response, wrote, err := c.attemptThroughProxy(ctx, snapshot, request)
		if response != nil || err == nil {
			return response, err
		}
		lastErr = err
		if wrote || ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *Client) attemptThroughProxy(ctx context.Context, snapshot runtimeconfig.Snapshot, request *http.Request) (*http.Response, bool, error) {
	handle, err := xkproxy.AcquireWithWait(ctx, c.proxy, snapshot, c.session, proxyWaitBudget)
	if err != nil {
		return nil, false, err
	}
	var wrote atomic.Bool
	trace := &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				wrote.Store(true)
			}
		},
	}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	httpClient := *c.httpClient
	httpClient.Transport = handle.Transport()
	started := time.Now()
	response, err := httpClient.Do(request)
	if response != nil {
		if response.Body == nil {
			handle.Release()
			if err == nil {
				err = errors.New("OpenCodeFree proxy transport returned response without body")
			}
			return nil, wrote.Load(), err
		}
		if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
			// The response header marks the network-observable first-byte point;
			// body generation time must never influence proxy exit ranking.
			handle.ReportRequestLatency(time.Since(started))
		}
		// The exit is only credited once the body has actually been consumed: a
		// 2xx header proves CONNECT worked, not that the stream survived. Holding
		// the release until body close also keeps the handle alive for the
		// transfer. A long stream that breaks mid-flight now reports a request
		// failure, so a throttled or half-broken exit is demoted instead of being
		// ranked as the fastest healthy one forever.
		response.Body = newPooledBody(response.Body, handle.Release, func() {
			if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
				handle.ReportLatency(0)
			}
		}, handle.ReportRequestFailure)
		if err != nil {
			_ = response.Body.Close()
			return nil, wrote.Load(), err
		}
		switch {
		case response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError:
			// Throttling and server failures may be specific to this exit, so let
			// the pool isolate it rather than keep ranking it as healthy.
			handle.ReportHTTPFailure(response.StatusCode)
		}
		return response, wrote.Load(), nil
	}
	// Nothing came back at all. Only a failure before the request left the wire
	// points at the exit; a gateway that hangs up mid-flight would otherwise let
	// one bad gateway retire every healthy exit in the pool.
	if err == nil {
		err = errors.New("OpenCodeFree proxy transport returned no response")
	}
	if wrote.Load() || ctx.Err() != nil {
		return nil, wrote.Load(), err
	}
	handle.Retire(xkproxy.RetireReasonTransportError)
	return nil, false, err
}

func (c *Client) newRequest(ctx context.Context, method, path string, body []byte, stream bool) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create OpenCodeFree request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if stream || bytes.Contains(body, []byte(`"stream":true`)) {
		request.Header.Set("Accept", "text/event-stream")
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
		request.ContentLength = int64(len(body))
	}
	request.Header.Set("x-opencode-client", "desktop")
	request.Header.Set("User-Agent", resolveUserAgent(ctx))
	sessionID, ok := SessionFrom(ctx)
	if !ok || !IsValidSessionID(sessionID) {
		generated, err := GenerateSessionID()
		if err != nil {
			return nil, fmt.Errorf("generate OpenCodeFree session ID: %w", err)
		}
		sessionID = generated
	}
	request.Header.Set("x-opencode-session", sessionID)
	if c.authKey != "" {
		request.Header.Set("Authorization", "Bearer "+c.authKey)
	} else if strings.Contains(c.baseURL, "opencode.ai") {
		request.Header.Set("Authorization", "Bearer public")
	}
	return request, nil
}

type modelEnvelope struct {
	Data []modelRecord `json:"data"`
}

type modelRecord struct {
	ID string `json:"id"`
}

// pooledBody owns the pooled handle for the lifetime of an upstream response.
// The handle is released exactly once, and the terminal outcome is reported
// exactly once: onComplete when the body reaches EOF normally, onFailure when
// the read fails partway. Without this wrapper a pooled exit would be judged
// only by its response headers, so a stream that broke mid-transfer would leave
// a broken exit ranked as healthy.
type pooledBody struct {
	io.ReadCloser
	release     func()
	onComplete  func()
	onFailure   func()
	releaseOnce sync.Once
	terminal    atomic.Uint32
}

func newPooledBody(body io.ReadCloser, release func(), onComplete func(), onFailure func()) *pooledBody {
	return &pooledBody{ReadCloser: body, release: release, onComplete: onComplete, onFailure: onFailure}
}

func (b *pooledBody) Read(payload []byte) (int, error) {
	read, err := b.ReadCloser.Read(payload)
	switch {
	case err == io.EOF:
		b.completeTerminal()
	case err != nil:
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// A cancelled transfer says nothing about the exit: the client or the
			// router gave up, not the upstream.
			return read, err
		}
		b.failTerminal()
	}
	return read, err
}

func (b *pooledBody) Close() error {
	err := b.ReadCloser.Close()
	b.releaseOnce.Do(b.release)
	return err
}

func (b *pooledBody) completeTerminal() {
	if !b.terminal.CompareAndSwap(0, 1) {
		return
	}
	if b.onComplete != nil {
		b.onComplete()
	}
}

func (b *pooledBody) failTerminal() {
	if !b.terminal.CompareAndSwap(0, 2) {
		return
	}
	if b.onFailure != nil {
		b.onFailure()
	}
}

func parseModels(body []byte) ([]string, error) {
	var envelope modelEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Data == nil {
		return nil, fmt.Errorf("%w: decode model list", ErrProtocol)
	}
	free := make([]string, 0, len(envelope.Data))
	other := make([]string, 0, len(envelope.Data))
	seen := make(map[string]struct{}, len(envelope.Data))
	for _, record := range envelope.Data {
		modelID := strings.TrimSpace(record.ID)
		if modelID == "" {
			return nil, fmt.Errorf("%w: model id is empty", ErrProtocol)
		}
		if _, exists := seen[modelID]; exists {
			continue
		}
		seen[modelID] = struct{}{}
		if strings.HasSuffix(strings.ToLower(modelID), "-free") {
			free = append(free, modelID)
		} else {
			other = append(other, modelID)
		}
	}
	if len(free)+len(other) == 0 {
		return nil, fmt.Errorf("%w: model list is empty", ErrProtocol)
	}
	return append(free, other...), nil
}
