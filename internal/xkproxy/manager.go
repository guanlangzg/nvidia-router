package xkproxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nvidia-router/internal/runtimeconfig"
)

type RetireReason string

const (
	RetireReasonTransportError RetireReason = "transport_error"
	// RetireReasonProxyRejected marks an exit that answered a CONNECT with an
	// HTTP refusal. Retiring it feeds the pool's transport-failure counter so
	// repeated refusals eject the exit instead of leaving it sticky for later
	// requests on the same session.
	RetireReasonProxyRejected RetireReason = "proxy_rejected"
	RetireReasonShutdown      RetireReason = "shutdown"
)

type ErrorReason string

const (
	ReasonTransportFailed ErrorReason = "transport_failed"
	// ReasonProxyRejected marks a proxy that answered with an HTTP error (e.g. a
	// 5xx CONNECT answer) rather than a pure transport failure. The proxy is up
	// and already refused the request, so replaying or switching keys would just
	// double the upstream load on a known-bad path (audit R5). The router surfaces
	// the error without a key switch, unlike ReasonTransportFailed.
	ReasonProxyRejected ErrorReason = "proxy_rejected"
	ReasonManagerClosed ErrorReason = "manager_closed"
	// ReasonNoHealthyProxy marks a pool that is momentarily empty (e.g. every
	// TTL expired between two collector fetches). The request never reached the
	// upstream through any exit, so the router treats it as retryable instead of
	// cooldowning the key (audit D3).
	ReasonNoHealthyProxy ErrorReason = "no_healthy_proxy"
)

type Error struct {
	reason ErrorReason
	cause  error
}

func (e *Error) Error() string       { return "upstream proxy unavailable" }
func (e *Error) Unwrap() error       { return e.cause }
func (e *Error) Reason() ErrorReason { return e.reason }

func NewTransportError(cause error) *Error {
	return &Error{reason: ReasonTransportFailed, cause: cause}
}

// NewProxyRejectedError marks a proxy that answered with an HTTP error rather
// than a transport failure (see ReasonProxyRejected).
func NewProxyRejectedError(cause error) *Error {
	return &Error{reason: ReasonProxyRejected, cause: cause}
}

// NewNoHealthyProxyError marks a momentarily empty pool (see
// ReasonNoHealthyProxy). The router maps it to a retryable 503 and does not
// cooldown the key, since the request never reached the upstream.
func NewNoHealthyProxyError() *Error {
	return &Error{reason: ReasonNoHealthyProxy, cause: errors.New("no healthy proxy available")}
}

type Provider interface {
	Configured() bool
	Enabled() bool
	Acquire(context.Context, runtimeconfig.Snapshot, string) (*Handle, error)
}

type Manager struct {
	mu         sync.RWMutex
	proxyURL   *url.URL
	pool       *Pool      // Built-in proxy pool (optional)
	collector  *Collector // Proxy collector (optional)
	base       *http.Transport
	logger     *slog.Logger
	transports map[transportKey]*cachedTransport
	clock      atomic.Uint64
	closed     bool
	// policy is the single source of ejection parameters for this manager.
	// Pool mode takes the operator-configured CollectorConfig.EjectionPolicy;
	// static mode falls back to the defaults. retire/ReportLatency/
	// ReportHTTPFailure all use it so a custom policy actually takes effect
	// everywhere instead of only inside the collector.
	policy EjectionPolicy
}

const maxCachedTransports = 64

// defaultEjectionPolicy is the fallback used by static-proxy managers and by
// pool managers whose CollectorConfig carries no explicit policy. It matches
// the values the reporting paths previously hardcoded.
func defaultEjectionPolicy() EjectionPolicy {
	return EjectionPolicy{
		FailureLimit: 3,
		BaseDuration: 10 * time.Second,
		MaxDuration:  60 * time.Second,
		MaxEjections: 3,
		LatencyAlpha: 0.3,
	}
}

// stickyRebindInterval bounds how long a session stays pinned to one exit.
// Session affinity prevents keys from racing each other through different
// exits, but a pin that never expires leaves a session on an exit that has
// quietly become slow or throttled without producing transport errors. The
// rebind closes idle connections and re-selects from the pool, which prefers
// the fastest live exit (audit H9).
const stickyRebindInterval = 60 * time.Second

type cachedTransport struct {
	transport *http.Transport
	// proxyKey is the pool identity the transport was built against. It is fixed
	// when the entry is created and reused on cache hits so failure reporting
	// always names the proxy the transport actually dials, never whatever the
	// rotation cursor happens to point at on a later Acquire.
	proxyKey string
	lastUsed atomic.Uint64
	// createdAt anchors the sticky-rebind window: once the entry is older than
	// stickyRebindInterval it is eligible for re-selection against a fresh pool
	// proxy.
	createdAt time.Time
}

type Handle struct {
	manager   *Manager
	key       transportKey
	transport *http.Transport
	proxyKey  string // Track which proxy this handle uses for failure reporting
}

type transportKey struct {
	connectTimeoutMS   int
	firstByteTimeoutMS int
	// session isolates one NVIDIA key's exit so different keys never share a
	// CONNECT. The empty session is the shared pool for callers without affinity.
	session string
}

func New(proxyURL *url.URL, authKey string, base *http.Transport, logger *slog.Logger) (*Manager, error) {
	if err := validateProxyURL(proxyURL); err != nil {
		return nil, err
	}
	if strings.TrimSpace(authKey) == "" {
		return nil, errors.New("initialize proxy manager: proxy authentication key is required")
	}
	if base == nil {
		return nil, errors.New("initialize proxy manager: HTTP transport is required")
	}
	if logger == nil {
		logger = slog.Default()
	}

	withAuth := *proxyURL
	withAuth.User = url.UserPassword("proxy", authKey)
	return &Manager{
		proxyURL:   &withAuth,
		base:       base,
		logger:     logger,
		transports: make(map[transportKey]*cachedTransport),
		policy:     defaultEjectionPolicy(),
	}, nil
}

// NewWithPool creates a manager with built-in proxy pool. The authKey parameter
// is retained for signature symmetry with New (static proxy mode) but is unused
// by the pool mode: the collector authenticates to the upstream API through the
// CollectorConfig URL, not through a fixed proxy credential.
func NewWithPool(cfg CollectorConfig, authKey string, base *http.Transport, logger *slog.Logger) (*Manager, error) {
	if base == nil {
		return nil, errors.New("initialize proxy manager: HTTP transport is required")
	}
	if logger == nil {
		logger = slog.Default()
	}

	pool := NewPool()
	collector := NewCollector(cfg, pool, logger)

	return &Manager{
		pool:       pool,
		collector:  collector,
		base:       base,
		logger:     logger,
		transports: make(map[transportKey]*cachedTransport),
		policy:     cfg.EjectionPolicy.normalized(),
	}, nil
}

// StartCollector starts the proxy collection loop
func (m *Manager) StartCollector(ctx context.Context) {
	if m.collector != nil {
		m.collector.Start(ctx)
	}
}

// Refresh runs one collector cycle for an operator-triggered refresh.
func (m *Manager) Refresh(ctx context.Context) error {
	if m == nil || m.collector == nil {
		return errors.New("built-in proxy collector is not enabled")
	}
	return m.collector.Refresh(ctx)
}

func validateProxyURL(value *url.URL) error {
	if value == nil || !value.IsAbs() || value.Host == "" || value.User != nil || value.RawQuery != "" || value.ForceQuery || value.Fragment != "" || (value.Path != "" && value.Path != "/") {
		return errors.New("initialize proxy manager: proxy URL is invalid")
	}
	scheme := strings.ToLower(value.Scheme)
	if scheme != "http" && scheme != "https" {
		return errors.New("initialize proxy manager: proxy URL is invalid")
	}
	return nil
}

func (m *Manager) Configured() bool { return m != nil }

func (m *Manager) Enabled() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return !m.closed
}

func (m *Manager) Acquire(ctx context.Context, snapshot runtimeconfig.Snapshot, session string) (*Handle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("proxy manager is nil")
	}

	key := transportKey{
		connectTimeoutMS:   snapshot.ConnectTimeoutMS,
		firstByteTimeoutMS: snapshot.FirstByteTimeoutMS,
		session:            session,
	}

	// Fast path: read lock for cache hit.
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return nil, &Error{reason: ReasonManagerClosed}
	}
	entry := m.transports[key]
	if entry != nil {
		proxyKey := entry.proxyKey
		createdAt := entry.createdAt
		transport := entry.transport
		if m.pool == nil || proxyKey == "" {
			entry.lastUsed.Store(m.clock.Add(1))
			m.mu.RUnlock()
			return &Handle{manager: m, key: key, transport: transport, proxyKey: proxyKey}, nil
		}
		now := time.Now()
		stale := now.Sub(createdAt) >= stickyRebindInterval
		needsRebuild := stale || !m.pool.HasHealthy(proxyKey, now)
		if !needsRebuild {
			entry.lastUsed.Store(m.clock.Add(1))
			m.mu.RUnlock()
			return &Handle{manager: m, key: key, transport: transport, proxyKey: proxyKey}, nil
		}
		m.mu.RUnlock()

		// Rebuild path: select fresh proxy and clone outside lock.
		var selectedProxy Proxy
		var hasProxy bool
		if snapshot.LatencyRoutingEnabled {
			selectedProxy, hasProxy = m.pool.GetWithQuality(now)
		} else {
			selectedProxy, hasProxy = m.pool.Get(now)
		}
		// If pool is empty now, keep old cached entry usable.
		if !hasProxy {
			m.mu.RLock()
			if e, ok := m.transports[key]; ok && e.transport == transport {
				e.lastUsed.Store(m.clock.Add(1))
				m.mu.RUnlock()
				return &Handle{manager: m, key: key, transport: transport, proxyKey: proxyKey}, nil
			}
			m.mu.RUnlock()
		} else if stale && selectedProxy.Key() == proxyKey {
			// Rebind would pick same proxy, keep old to avoid needless CONNECT.
			m.mu.RLock()
			if e, ok := m.transports[key]; ok && e.transport == transport {
				e.lastUsed.Store(m.clock.Add(1))
				m.mu.RUnlock()
				return &Handle{manager: m, key: key, transport: transport, proxyKey: proxyKey}, nil
			}
			m.mu.RUnlock()
		} else {
			transport2, err := m.newTransport(key, selectedProxy)
			if err != nil {
				return nil, NewTransportError(err)
			}
			m.mu.Lock()
			if m.closed {
				transport2.CloseIdleConnections()
				m.mu.Unlock()
				return nil, &Error{reason: ReasonManagerClosed}
			}
			// Double-check entry still same.
			if cur, ok := m.transports[key]; ok && cur.transport == transport {
				cur.transport.CloseIdleConnections()
			}
			newEntry := &cachedTransport{transport: transport2, proxyKey: selectedProxy.Key(), createdAt: now}
			newEntry.lastUsed.Store(m.clock.Add(1))
			m.transports[key] = newEntry
			if len(m.transports) > maxCachedTransports {
				m.evictLeastRecentlyUsed()
			}
			m.mu.Unlock()
			return &Handle{manager: m, key: key, transport: newEntry.transport, proxyKey: newEntry.proxyKey}, nil
		}
	} else {
		m.mu.RUnlock()
	}

	// Cache miss: select proxy and build transport outside lock.
	var selectedProxy Proxy
	var hasProxy bool
	if m.pool != nil {
		if snapshot.LatencyRoutingEnabled {
			selectedProxy, hasProxy = m.pool.GetWithQuality(time.Now())
		} else {
			selectedProxy, hasProxy = m.pool.Get(time.Now())
		}
		if !hasProxy {
			return nil, NewNoHealthyProxyError()
		}
	}
	transport, err := m.newTransport(key, selectedProxy)
	if err != nil {
		return nil, NewTransportError(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		transport.CloseIdleConnections()
		return nil, &Error{reason: ReasonManagerClosed}
	}
	// Another goroutine may have installed entry while we cloned.
	if existing, ok := m.transports[key]; ok {
		transport.CloseIdleConnections()
		existing.lastUsed.Store(m.clock.Add(1))
		if len(m.transports) > maxCachedTransports {
			m.evictLeastRecentlyUsed()
		}
		return &Handle{manager: m, key: key, transport: existing.transport, proxyKey: existing.proxyKey}, nil
	}
	entry2 := &cachedTransport{transport: transport, proxyKey: selectedProxy.Key(), createdAt: time.Now()}
	entry2.lastUsed.Store(m.clock.Add(1))
	m.transports[key] = entry2
	if len(m.transports) > maxCachedTransports {
		m.evictLeastRecentlyUsed()
	}
	return &Handle{manager: m, key: key, transport: entry2.transport, proxyKey: entry2.proxyKey}, nil
}

func (m *Manager) evictLeastRecentlyUsed() {
	var oldestKey transportKey
	var oldest uint64
	for key, entry := range m.transports {
		used := entry.lastUsed.Load()
		if oldest == 0 || used < oldest {
			oldestKey = key
			oldest = used
		}
	}
	entry := m.transports[oldestKey]
	if entry != nil {
		entry.transport.CloseIdleConnections()
		delete(m.transports, oldestKey)
	}
}

func (m *Manager) newTransport(key transportKey, proxy Proxy) (*http.Transport, error) {
	transport := m.base.Clone()

	// If we have a proxy from the pool, use it; otherwise use the configured proxyURL
	if proxy.Address != "" {
		proxyURL, err := proxy.URL()
		if err != nil {
			return nil, fmt.Errorf("configure selected proxy %q: %w", proxy.Address, err)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	} else if m.proxyURL != nil {
		transport.Proxy = http.ProxyURL(m.proxyURL)
	}
	// Pin the session on the outer proxy request so the pool can bind the exit.
	// GetProxyConnectHeader is only consulted for HTTPS CONNECT, which is the only
	// path this router uses through the pool.
	if key.session != "" {
		transport.GetProxyConnectHeader = func(ctx context.Context, proxyURL *url.URL, target string) (http.Header, error) {
			header := make(http.Header)
			header.Set("X-XK-Session", key.session)
			return header, nil
		}
	}
	connectTimeout := time.Duration(key.connectTimeoutMS) * time.Millisecond
	if connectTimeout > 0 {
		// DialContext already bounds the TCP connect; without a matching TLS
		// handshake timeout the proxy-leg (or tunnel-target) TLS could hang far
		// beyond the operator's connect intent — ResponseHeaderTimeout does not
		// cover the handshake, which happens before the request is written.
		transport.TLSHandshakeTimeout = connectTimeout
	}
	// KeepAlive probes prevent NAT/firewall tables from silently dropping idle
	// CONNECT tunnels, which would appear as phantom transport errors on the next
	// request that tries to reuse the cached connection.
	keepAliveDialer := &net.Dialer{
		Timeout:   connectTimeout,
		KeepAlive: 30 * time.Second,
	}
	// The manager's transport always dials the configured proxy, so it must not
	// inherit a caller transport's custom dialer. Such a dialer may be scoped to
	// the direct upstream (as in TLS test clients) and would bypass the proxy.
	transport.DialContext = keepAliveDialer.DialContext
	transport.ResponseHeaderTimeout = time.Duration(key.firstByteTimeoutMS) * time.Millisecond
	// Larger idle pools reduce connection churn when many sticky sessions are
	// active simultaneously; 90s keeps CONNECT tunnels alive past most
	// NAT/load-balancer idle timeouts without holding sockets indefinitely.
	transport.MaxIdleConns = 128
	transport.MaxIdleConnsPerHost = 64
	transport.IdleConnTimeout = 90 * time.Second
	// The outer leg to the proxy is a plain HTTP CONNECT (http:// proxy URL), so
	// ForceAttemptHTTP2 only affects the TLS connection to the TARGET inside the
	// tunnel. The NVIDIA API sits behind a CDN that speaks HTTP/2; disabling h2
	// here made the inner request HTTP/1.1 while the target replied with HTTP/2
	// frames, breaking every proxied request with a malformed-response error
	// (found in real 联调 2026-08-12). Direct mode already enables h2 (chat.go);
	// the proxy path must match so ALPN negotiates h2 with the target.
	transport.ForceAttemptHTTP2 = true
	return transport, nil
}

func (m *Manager) retire(handle *Handle, reason RetireReason) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if handle == nil || handle.manager != m {
		return
	}
	if current := m.transports[handle.key]; current != nil && current.transport == handle.transport {
		delete(m.transports, handle.key)
	}
	handle.transport.CloseIdleConnections()
	m.logger.Info("proxy_transport_retired", "reason", reason)

	// Report failure to pool if we have one
	if m.pool != nil && handle.proxyKey != "" {
		m.pool.ReportFailure(handle.proxyKey, time.Now(), m.policy)
	}
}

func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	// Take the collector reference out from under the lock: collector.Close()
	// blocks until in-flight fetch/validation completes (up to ~27s), which must
	// not stall concurrent Acquire calls. closed is already set, so new Acquires
	// reject while we wait.
	collector := m.collector
	m.collector = nil
	for _, entry := range m.transports {
		entry.transport.CloseIdleConnections()
	}
	m.transports = nil
	m.mu.Unlock()

	if collector != nil {
		_ = collector.Close()
	}
	m.logger.Info("proxy_manager_closed")
}

// PoolStatus returns current pool status for monitoring
func (m *Manager) PoolStatus() PoolStatus {
	if m == nil {
		return PoolStatus{}
	}
	m.mu.RLock()
	proxyURL := ""
	if m.proxyURL != nil {
		publicURL := *m.proxyURL
		publicURL.User = nil
		proxyURL = publicURL.String()
	}
	closed := m.closed
	pool := m.pool
	m.mu.RUnlock()
	if closed {
		return PoolStatus{}
	}
	if pool == nil {
		status := PoolStatus{Configured: proxyURL != "", Mode: "external", Endpoint: proxyURL}
		if proxyURL == "" {
			return status
		}
		// /healthz is deliberately used instead of /readyz: the latter requires
		// the pool's separate admin credential, which must never be copied into
		// the NVIDIA router. Reachability is useful diagnostics; request routing
		// still fails closed when the pool has no usable lease.
		healthURL, err := url.Parse(proxyURL)
		if err != nil {
			return status
		}
		healthURL.Path = "/healthz"
		healthURL.RawQuery = ""
		healthURL.Fragment = ""
		if m.base == nil {
			return status
		}
		transport := m.base.Clone()
		transport.Proxy = nil
		client := &http.Client{Transport: transport, Timeout: time.Second}
		started := time.Now()
		response, err := client.Get(healthURL.String())
		if err == nil {
			_ = response.Body.Close()
			status.Reachable = response.StatusCode == http.StatusOK
			status.HealthLatencyMS = time.Since(started).Milliseconds()
		}
		transport.CloseIdleConnections()
		return status
	}
	now := time.Now()
	upstreamOverloaded, lastUpstreamOverloadAt := m.pool.UpstreamOverloadStatus(now)
	status := PoolStatus{
		Configured:             true,
		Mode:                   "built-in",
		TotalSize:              pool.LiveSize(now),
		HealthySize:            m.pool.Size(now),
		CollectorEnabled:       m.collector != nil,
		Proxies:                pool.List(now),
		UpstreamOverloaded:     upstreamOverloaded,
		LastUpstreamOverloadAt: lastUpstreamOverloadAt,
		// Panic mode: candidates exist but every one is ejected or TTL-expired;
		// the pool would serve them anyway to keep the service alive.
		PanicMode: pool.LiveSize(now) > 0 && pool.Size(now) == 0,
	}
	// Collector health tells the operator why the pool might be empty: the
	// upstream is unreachable (LastErrorCode set) or simply returned nothing.
	if m.collector != nil {
		status.LastFetchAt, status.LastSuccessAt, status.LastErrorCode = m.collector.LastFetchResult()
		status.Endpoint = "xingkong-xapi"
	}
	return status
}

type PoolStatus struct {
	// Configured and Mode describe both external and built-in modes. The external
	// mode is the production path: the router only talks to the pool's standard
	// HTTP forward-proxy port and never receives individual exits.
	Configured             bool   `json:"configured"`
	Mode                   string `json:"mode"`
	Endpoint               string `json:"endpoint"`
	Reachable              bool   `json:"reachable"`
	HealthLatencyMS        int64  `json:"health_latency_ms"`
	TotalSize              int
	HealthySize            int
	CollectorEnabled       bool
	Proxies                []Proxy
	UpstreamOverloaded     bool      `json:"upstream_overloaded"`
	LastUpstreamOverloadAt time.Time `json:"last_upstream_overload_at"`

	// PanicMode reports that live candidates exist but none is currently
	// available (all ejected or expired), so selection would serve a degraded
	// exit to keep the service alive.
	PanicMode bool `json:"panic_mode"`

	// Collector diagnostics (zero/empty when not in pool mode).
	LastFetchAt   time.Time
	LastSuccessAt time.Time
	LastErrorCode string
}

// JSON-safe projection of a pooled proxy for the admin UI. Only non-sensitive,
// operator-visible quality fields are exposed: the exit address, its measured
// latency, remaining TTL, isolation state and recent failure signals.
type ProxyStatus struct {
	Address               string `json:"address"`
	LatencyEWMAMS         int64  `json:"latency_ewma_ms"`
	LatencySamples        int    `json:"latency_samples"`
	RemainingSeconds      int    `json:"remaining_seconds"`
	Healthy               bool   `json:"healthy"`
	Ejected               bool   `json:"ejected"`
	EjectionCount         int    `json:"ejection_count"`
	HealthFails           int    `json:"health_fails"`
	SuccessCount          uint64 `json:"success_count"`
	FailureCount          uint64 `json:"failure_count"`
	QualityScore          int    `json:"quality_score"`
	RequestSuccessCount   uint64 `json:"request_success_count"`
	RequestFailureCount   uint64 `json:"request_failure_count"`
	RequestFailureStreak  int    `json:"request_failure_streak"`
	RequestLatencyEWMAMS  int64  `json:"request_latency_ewma_ms"`
	RequestLatencySamples int    `json:"request_latency_samples"`
	// HTTPFailCount exposes the consecutive 429/5xx pattern so the UI can flag
	// exits that are throttled but not yet isolated.
	HTTPFailCount int `json:"http_fail_count"`
	// LastHTTPStatus is the most recent HTTP failure status through this exit
	// (0 when none); 529 is observed but never counted toward isolation.
	LastHTTPStatus int `json:"last_http_status"`
	// LastFailureAt is the most recent transport or HTTP failure timestamp
	// (RFC3339 UTC, empty when the exit has never failed).
	LastFailureAt string `json:"last_failure_at"`
}

func (s PoolStatus) View() []ProxyStatus {
	now := time.Now()
	view := make([]ProxyStatus, 0, len(s.Proxies))
	for _, proxy := range s.Proxies {
		lastFailure := ""
		if !proxy.LastFailureAt.IsZero() {
			lastFailure = proxy.LastFailureAt.UTC().Format(time.RFC3339)
		}
		view = append(view, ProxyStatus{
			Address:               proxy.Address,
			LatencyEWMAMS:         proxy.LatencyEWMA.Milliseconds(),
			LatencySamples:        proxy.LatencySamples,
			RemainingSeconds:      int(proxy.RemainingLife(now) / time.Second),
			Healthy:               proxy.AvailableAt(now),
			Ejected:               proxy.EjectedAt(now),
			EjectionCount:         proxy.EjectionCount,
			HealthFails:           proxy.HealthFails,
			SuccessCount:          proxy.SuccessCount,
			FailureCount:          proxy.FailureCount,
			QualityScore:          proxy.QualityScore(),
			RequestSuccessCount:   proxy.RequestSuccessCount,
			RequestFailureCount:   proxy.RequestFailureCount,
			RequestFailureStreak:  proxy.RequestFailureStreak,
			RequestLatencyEWMAMS:  proxy.RequestLatencyEWMA.Milliseconds(),
			RequestLatencySamples: proxy.RequestLatencySamples,
			HTTPFailCount:         proxy.HTTPFailCount,
			LastHTTPStatus:        proxy.LastHTTPFailureStatus,
			LastFailureAt:         lastFailure,
		})
	}
	return view
}

func (h *Handle) Transport() http.RoundTripper {
	if h == nil {
		return nil
	}
	return h.transport
}

// ProxyKey reports the pool identity of the exit this handle's transport dials,
// or "" in static-proxy mode (no pool). The request path uses it for diagnostics
// so a failed or slow request can be correlated with a specific proxy exit.
func (h *Handle) ProxyKey() string {
	if h == nil {
		return ""
	}
	return h.proxyKey
}

func (h *Handle) Retire(reason RetireReason) {
	if h == nil || h.manager == nil {
		return
	}
	h.manager.retire(h, reason)
}

// Invalidate drops the cached transport bound to this handle without treating
// the exit as conclusively dead. It is used when a request-level first-byte
// timeout makes the connection unusable but cannot distinguish a slow target
// from a bad proxy. The next request gets a fresh transport and can explore a
// different exit.
func (h *Handle) Invalidate() {
	if h == nil || h.manager == nil {
		return
	}
	h.manager.invalidate(h.key, h.transport)
}

// ReportLatency records a successful request and, when latency is positive,
// feeds its duration into the pool's EWMA. A zero duration is a valid semantic
// completion signal for callers that already recorded first-byte latency; it
// increments success without adding a second latency sample. Best-effort: a
// nil pool or empty proxyKey (static proxy mode) is a no-op.
func (h *Handle) ReportLatency(latency time.Duration) {
	if h == nil || h.manager == nil || h.manager.pool == nil || h.proxyKey == "" || latency < 0 {
		return
	}
	h.manager.pool.ReportSuccess(h.proxyKey, time.Now(), latency, h.manager.policy)
}

// ReportRequestLatency records network latency without marking the request as
// a completed NVIDIA request. This is the signal used for first-byte quality.
func (h *Handle) ReportRequestLatency(latency time.Duration) {
	if h == nil || h.manager == nil || h.manager.pool == nil || h.proxyKey == "" || latency <= 0 {
		return
	}
	h.manager.pool.ReportRequestLatency(h.proxyKey, time.Now(), latency, h.manager.policy)
}

// ReportHTTPFailure feeds an application-level failure (429/5xx observed
// through this exit) into the pool so a rate-limited or blocked IP is isolated
// instead of being treated as healthy (audit H8). Unlike ReportLatency it does
// not clear an existing isolation window and does not update the latency EWMA:
// a fast rejection must not make a failing exit look fast. Best-effort like
// ReportLatency.
func (h *Handle) ReportHTTPFailure(status int) {
	if h == nil || h.manager == nil || h.manager.pool == nil || h.proxyKey == "" {
		return
	}
	h.manager.pool.ReportHTTPFailure(h.proxyKey, time.Now(), status, h.manager.policy)
}

// ReportRequestFailure records a failure while reading a response body. It
// feeds quality routing without applying transport-level proxy isolation.
func (h *Handle) ReportRequestFailure() {
	if h == nil || h.manager == nil || h.manager.pool == nil || h.proxyKey == "" {
		return
	}
	h.manager.pool.ReportRequestFailure(h.proxyKey, time.Now())
}

func (h *Handle) Release() {}

func (m *Manager) invalidate(key transportKey, transport http.RoundTripper) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.transports[key]
	if entry == nil || entry.transport != transport {
		return
	}
	entry.transport.CloseIdleConnections()
	delete(m.transports, key)
}
