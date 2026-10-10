package xkproxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestParseProxy(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "simple host:port",
			input: "192.168.1.1:8080",
			want:  "192.168.1.1:8080",
		},
		{
			name:  "with credentials",
			input: "192.168.1.1:8080:user:pass",
			want:  "192.168.1.1:8080",
		},
		{
			name:  "http URL",
			input: "http://192.168.1.1:8080",
			want:  "192.168.1.1:8080",
		},
		{
			name:  "http URL with auth",
			input: "http://user:pass@192.168.1.1:8080",
			want:  "192.168.1.1:8080",
		},
		{
			name:    "empty",
			input:   "",
			wantErr: true,
		},
		{
			name:    "invalid port",
			input:   "192.168.1.1:99999",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy, err := ParseProxy(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseProxy() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && proxy.Address != tt.want {
				t.Errorf("ParseProxy() address = %v, want %v", proxy.Address, tt.want)
			}
		})
	}
}

func TestProxyLifecycle(t *testing.T) {
	now := time.Now()
	proxy := Proxy{
		Address:   "192.168.1.1:8080",
		ExpiresAt: now.Add(1 * time.Minute),
	}

	if !proxy.LiveAt(now) {
		t.Error("Expected proxy to be live")
	}

	if !proxy.LiveAt(now.Add(30 * time.Second)) {
		t.Error("Expected proxy to be live after 30s")
	}

	if proxy.LiveAt(now.Add(2 * time.Minute)) {
		t.Error("Expected proxy to be expired after 2m")
	}

	// Test ejection
	proxy.EjectedUntil = now.Add(10 * time.Second)
	if proxy.AvailableAt(now) {
		t.Error("Expected proxy to be unavailable when ejected")
	}

	if !proxy.AvailableAt(now.Add(11 * time.Second)) {
		t.Error("Expected proxy to be available after ejection expires")
	}
}

func TestPoolOperations(t *testing.T) {
	pool := NewPool()
	now := time.Now()

	proxies := []Proxy{
		{
			Address:     "192.168.1.1:8080",
			FetchedAt:   now,
			ValidatedAt: now,
			ExpiresAt:   now.Add(2 * time.Minute),
		},
		{
			Address:     "192.168.1.2:8080",
			FetchedAt:   now,
			ValidatedAt: now,
			ExpiresAt:   now.Add(2 * time.Minute),
		},
	}

	pool.Merge(now, proxies)

	if size := pool.Size(now); size != 2 {
		t.Errorf("Expected pool size 2, got %d", size)
	}

	proxy, ok := pool.Get(now)
	if !ok {
		t.Fatal("Expected to get a proxy")
	}
	if proxy.Address == "" {
		t.Error("Expected non-empty proxy address")
	}

	// Test expiration
	expiredTime := now.Add(3 * time.Minute)
	if size := pool.Size(expiredTime); size != 0 {
		t.Errorf("Expected pool size 0 after expiration, got %d", size)
	}
}

func TestPoolEjection(t *testing.T) {
	pool := NewPool()
	now := time.Now()

	proxy := Proxy{
		Address:     "192.168.1.1:8080",
		FetchedAt:   now,
		ValidatedAt: now,
		ExpiresAt:   now.Add(2 * time.Minute),
	}

	pool.Merge(now, []Proxy{proxy})

	policy := EjectionPolicy{
		FailureLimit: 3,
		BaseDuration: 10 * time.Second,
		MaxDuration:  60 * time.Second,
		MaxEjections: 3,
	}

	// Report failures
	outcome := pool.ReportFailure(proxy.Address, now, policy)
	if outcome != OutcomeCounted {
		t.Errorf("Expected OutcomeCounted, got %v", outcome)
	}

	outcome = pool.ReportFailure(proxy.Address, now, policy)
	if outcome != OutcomeCounted {
		t.Errorf("Expected OutcomeCounted, got %v", outcome)
	}

	outcome = pool.ReportFailure(proxy.Address, now, policy)
	if outcome != OutcomeEjected {
		t.Errorf("Expected OutcomeEjected, got %v", outcome)
	}

	// Proxy should be unavailable
	if size := pool.Size(now); size != 0 {
		t.Errorf("Expected pool size 0 after ejection, got %d", size)
	}

	// Report success should recover
	pool.ReportSuccess(proxy.Address, now, 100*time.Millisecond, policy)
	if size := pool.Size(now.Add(11 * time.Second)); size != 1 {
		t.Errorf("Expected pool size 1 after recovery, got %d", size)
	}
}

func TestParseProxies(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantCount int
		wantErr   bool
	}{
		{
			name:      "single proxy",
			input:     "192.168.1.1:8080",
			wantCount: 1,
		},
		{
			name:      "multiple proxies",
			input:     "192.168.1.1:8080\n192.168.1.2:8080",
			wantCount: 2,
		},
		{
			name:      "with empty lines",
			input:     "192.168.1.1:8080\n\n192.168.1.2:8080",
			wantCount: 2,
		},
		{
			name:      "deduplicate",
			input:     "192.168.1.1:8080\n192.168.1.1:8080",
			wantCount: 1,
		},
		{
			name:    "provider error",
			input:   "code: 204",
			wantErr: true,
		},
		{
			name:    "empty",
			input:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxies, err := ParseProxies(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseProxies() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && len(proxies) != tt.wantCount {
				t.Errorf("ParseProxies() count = %d, want %d", len(proxies), tt.wantCount)
			}
		})
	}
}

func TestProviderErrorDetection(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantCode string
	}{
		{
			name:     "code 204",
			input:    "code: 204",
			wantCode: "204",
		},
		{
			name:     "status 403",
			input:    "status: 403",
			wantCode: "403",
		},
		{
			name:     "error 211",
			input:    "error 211: whitelist required",
			wantCode: "211",
		},
		{
			name:     "no error",
			input:    "192.168.1.1:8080",
			wantCode: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseProxies(tt.input)
			code := ErrorCode(err)
			if code != tt.wantCode {
				t.Errorf("ErrorCode() = %v, want %v", code, tt.wantCode)
			}
		})
	}
}

func TestMergeRefreshesProxyExpiryOnRepeatedFetches(t *testing.T) {
	// Audit P2-1: Merge used to keep only the earliest ExpiresAt, so a proxy that
	// the collector refetches every interval froze its TTL at the first fetch and
	// the pool went empty every TTL window. A re-fetched live proxy must extend
	// its expiry.
	pool := NewPool()
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

	first := Proxy{Scheme: "http", Address: "10.0.0.1:8080", FetchedAt: now, ExpiresAt: now.Add(2 * time.Minute)}
	pool.Merge(now, []Proxy{first})
	if _, ok := pool.Peek(now); !ok {
		t.Fatal("first fetch did not make a proxy available")
	}

	later := now.Add(1 * time.Minute)
	// Second fetch returns the same proxy with a refreshed TTL.
	pool.Merge(later, []Proxy{{
		Scheme: "http", Address: "10.0.0.1:8080", FetchedAt: later, ExpiresAt: later.Add(2 * time.Minute),
	}})

	// After the original TTL would have expired, the refreshed proxy must still
	// be live — proving Merge extended the expiry instead of freezing it.
	afterOriginalExpiry := now.Add(2 * time.Minute).Add(30 * time.Second)
	if proxy, ok := pool.Peek(afterOriginalExpiry); !ok {
		t.Fatal("refreshed proxy is not live after the original expiry; ExpiresAt was not extended")
	} else if !proxy.ExpiresAt.After(afterOriginalExpiry) {
		t.Fatalf("refreshed proxy ExpiresAt = %s, want after %s", proxy.ExpiresAt, afterOriginalExpiry)
	}
}

// TestValidatorRejectsSlowProxy verifies the MaxLatency gate (audit H1): a proxy
// that reaches the validation target but takes longer than the configured window
// is rejected as ErrSlowProxy instead of being admitted as healthy.
func TestValidatorRejectsSlowProxy(t *testing.T) {
	delay := 80 * time.Millisecond
	validation := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(validation.Close)

	// A forwarding HTTP proxy: it sleeps before relaying the absolute-form
	// request to the validation target, so the round-trip is slow enough to trip
	// a 20ms MaxLatency gate but still returns the expected 404.
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		time.Sleep(delay)
		// Proxy-style absolute-form URI (the validator sends GET <full URL>).
		target := request.RequestURI
		if parsed, err := url.Parse(target); err != nil || !parsed.IsAbs() {
			target = validation.URL + request.RequestURI
		}
		relayed, err := http.NewRequest(request.Method, target, request.Body)
		if err != nil {
			writer.WriteHeader(http.StatusBadGateway)
			return
		}
		relayed.Header = request.Header.Clone()
		resp, err := http.DefaultClient.Do(relayed)
		if err != nil {
			writer.WriteHeader(http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		writer.WriteHeader(resp.StatusCode)
	}))
	t.Cleanup(proxy.Close)

	host, portText, err := net.SplitHostPort(proxy.Listener.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	slow := Proxy{Scheme: "http", Address: net.JoinHostPort(host, portText)}

	// Without the gate the proxy is considered valid (it returns the expected
	// status); with a 20ms cap it must be rejected as too slow.
	permissive := NewValidatorForTest(validation.URL, http.StatusNotFound, 2*time.Second)
	if _, err := permissive.ValidateWithLatency(context.Background(), slow); err != nil {
		t.Fatalf("permissive ValidateWithLatency: %v (want success, slow gate disabled)", err)
	}

	strict := NewValidatorWithMaxLatencyForTest(validation.URL, http.StatusNotFound, 2*time.Second, 20*time.Millisecond)
	_, err = strict.ValidateWithLatency(context.Background(), slow)
	if !errors.Is(err, ErrSlowProxy) {
		t.Fatalf("strict ValidateWithLatency error = %v, want ErrSlowProxy", err)
	}
}

// TestPoolDoesNotResurrectPermanentlyEjectedProxy verifies the M6 fix: a proxy
// permanently removed by exceeding MaxEjections must not be re-admitted by the
// next collector fetch while its removal cooldown is active.
func TestPoolDoesNotResurrectPermanentlyEjectedProxy(t *testing.T) {
	pool := NewPool()
	now := time.Now()
	proxy := Proxy{Scheme: "http", Address: "192.168.1.1:8080", FetchedAt: now, ExpiresAt: now.Add(2 * time.Minute)}
	pool.Merge(now, []Proxy{proxy})

	policy := EjectionPolicy{FailureLimit: 2, BaseDuration: 10 * time.Second, MaxDuration: 60 * time.Second, MaxEjections: 2}

	// Exceed MaxEjections: 2 ejections plus a final failure removes the proxy.
	pool.ReportFailure(proxy.Address, now, policy)            // HealthFails=1
	pool.ReportFailure(proxy.Address, now, policy)            // HealthFails=2 → ejected (EjectionCount=1)
	pool.ReportFailure(proxy.Address, now, policy)            // HealthFails=1
	pool.ReportFailure(proxy.Address, now, policy)            // HealthFails=2 → ejected (EjectionCount=2)
	pool.ReportFailure(proxy.Address, now, policy)            // HealthFails=1
	outcome := pool.ReportFailure(proxy.Address, now, policy) // HealthFails=2 → EjectionCount=3 > 2 → removed
	if outcome != OutcomeRemoved {
		t.Fatalf("final ReportFailure outcome = %v, want OutcomeRemoved", outcome)
	}
	if size := pool.LiveSize(now); size != 0 {
		t.Fatalf("pool size after removal = %d, want 0", size)
	}

	// The next fetch returns the same proxy; it must not be re-admitted while the
	// removal cooldown is active.
	refetch := now.Add(time.Minute)
	pool.Merge(refetch, []Proxy{{
		Scheme: "http", Address: "192.168.1.1:8080", FetchedAt: refetch, ExpiresAt: refetch.Add(2 * time.Minute),
	}})
	if _, ok := pool.Peek(refetch); ok {
		t.Fatal("permanently ejected proxy was resurrected by a fetch during the removal cooldown")
	}

	// After the cooldown expires the proxy is admitted again on a fresh fetch.
	later := now.Add(removalCooldown + time.Minute)
	pool.Merge(later, []Proxy{{
		Scheme: "http", Address: "192.168.1.1:8080", FetchedAt: later, ExpiresAt: later.Add(2 * time.Minute),
	}})
	if _, ok := pool.Peek(later); !ok {
		t.Fatal("proxy was not re-admitted after the removal cooldown expired")
	}
}

// TestStickyGetFallsBackWhenBoundProxyGone guards the StickyGet binding path
// against a double-unlock panic: when the bound proxy is ejected or expired, the
// method must drop the stale binding and fall back to the generic selection
// without touching stickyMu twice (audit: dead-code panic, enabled-on-use).
func TestStickyGetFallsBackWhenBoundProxyGone(t *testing.T) {
	now := time.Now()
	pool := NewPool()
	proxyA := Proxy{Scheme: "http", Address: "10.0.0.1:8080", ExpiresAt: now.Add(10 * time.Minute)}
	proxyB := Proxy{Scheme: "http", Address: "10.0.0.2:8080", ExpiresAt: now.Add(10 * time.Minute)}
	pool.Replace([]Proxy{proxyA, proxyB})
	pool.StickyRebind("sess", proxyA.Key(), now, time.Minute)

	// Bound proxy still present -> sticky hit returns it first.
	candidates, _, hit := pool.StickyGet("sess", now, 0, time.Minute)
	if !hit || len(candidates) == 0 || candidates[0].Key() != proxyA.Key() {
		t.Fatalf("sticky hit: got hit=%v candidates=%v, want proxy A first", hit, candidates)
	}

	// Remove the bound proxy: must fall back to the generic selection without
	// a double-unlock panic and must not report a sticky hit.
	pool.Replace([]Proxy{proxyB})
	candidates, _, hit = pool.StickyGet("sess", now, 0, time.Minute)
	if hit {
		t.Fatal("reported a sticky hit for a removed proxy")
	}
	if len(candidates) == 0 || candidates[0].Key() != proxyB.Key() {
		t.Fatalf("fallback: got candidates %v, want proxy B", candidates)
	}
}
