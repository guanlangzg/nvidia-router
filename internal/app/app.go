package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nvidia-router/internal/accesskey"
	"nvidia-router/internal/adminaudit"
	"nvidia-router/internal/adminauth"
	"nvidia-router/internal/clock"
	"nvidia-router/internal/config"
	"nvidia-router/internal/crypto"
	"nvidia-router/internal/database"
	"nvidia-router/internal/embedcache"
	"nvidia-router/internal/eventhub"
	"nvidia-router/internal/httpapi"
	adminapi "nvidia-router/internal/httpapi/admin"
	"nvidia-router/internal/httpapi/health"
	metricsapi "nvidia-router/internal/httpapi/metrics"
	v1 "nvidia-router/internal/httpapi/v1"
	"nvidia-router/internal/modelcatalog"
	"nvidia-router/internal/modelhealth"
	"nvidia-router/internal/nvidiakey"
	"nvidia-router/internal/observability"
	"nvidia-router/internal/pool"
	"nvidia-router/internal/processlock"
	"nvidia-router/internal/providercredential"
	"nvidia-router/internal/router"
	"nvidia-router/internal/runtimeconfig"
	"nvidia-router/internal/upstream/nvidia"
	"nvidia-router/internal/upstream/opencodefree"
	webui "nvidia-router/internal/web"
	"nvidia-router/internal/xkproxy"
)

type Dependencies struct {
	Config           config.Config
	DB               *sql.DB
	Logger           *slog.Logger
	Clock            clock.Clock
	NVIDIAHTTPClient *http.Client
}

type App struct {
	Dependencies    Dependencies
	Pool            *pool.Pool
	RuntimeSettings *runtimeconfig.Store
	Server          *Server
	proxy           *xkproxy.Switcher
	proxySettings   *xkproxy.SettingsService
	nvidiaClient    *nvidia.Client

	db                  *sql.DB
	dbReader            *sql.DB
	dbLock              *processlock.Lock
	handler             http.Handler
	requestRecorder     *observability.BufferRecorder
	healthChecker       *nvidiakey.HealthChecker
	shutting            atomic.Bool
	cleanupCancel       context.CancelFunc
	cleanupDone         chan struct{}
	recorderCancel      context.CancelFunc
	recorderDone        chan struct{}
	healthCancel        context.CancelFunc
	healthDone          chan struct{}
	modelHealthDone     <-chan struct{}
	ocfSyncDone         <-chan struct{}
	capabilityProbeDone <-chan struct{}
	rootCancel          context.CancelFunc
	shutdownOnce        sync.Once
	shutdownGrace       time.Duration
	shutdownTimer       *time.Timer
	shutdownDeadline    time.Time
	close               sync.Once
	closeErr            error
}

func New(ctx context.Context, dependencies Dependencies) (*App, error) {
	resolved, err := resolveDependencies(dependencies)
	if err != nil {
		return nil, err
	}

	// Check for insecure production configurations
	checkProductionSecurity(resolved.Config, resolved.Logger)

	var dbLock *processlock.Lock
	lockTransferred := false
	if resolved.DB == nil {
		if err := os.MkdirAll(resolved.Config.DataDir, 0o750); err != nil {
			return nil, fmt.Errorf("create data directory: %w", err)
		}
		lockPath := filepath.Join(resolved.Config.DataDir, ".router.db.lock")
		dbLock, err = processlock.TryLock(lockPath)
		if err != nil {
			return nil, fmt.Errorf("acquire router database lock: %w", err)
		}
		defer func() {
			if !lockTransferred {
				_ = dbLock.Close()
			}
		}()
	}
	db, reader, err := openDatabase(resolved)
	if err != nil {
		return nil, err
	}
	keys, err := initialize(ctx, db, resolved)
	if err != nil {
		return nil, closeAfterInitializationError(db, reader, err)
	}
	settings, err := runtimeconfig.New(ctx, db)
	if err != nil {
		return nil, closeAfterInitializationError(db, reader, fmt.Errorf("initialize runtime settings store: %w", err))
	}
	keyRepository := nvidiakey.NewRepository(db).WithReader(reader)
	keySnapshots, err := keyRepository.ListSnapshots(ctx)
	if err != nil {
		return nil, closeAfterInitializationError(db, reader, fmt.Errorf("load NVIDIA key scheduling snapshots: %w", err))
	}
	modelRepository := modelcatalog.NewRepository(db).WithReader(reader)
	modelBlocks, err := modelRepository.ListBlocks(ctx)
	if err != nil {
		return nil, closeAfterInitializationError(db, reader, fmt.Errorf("load NVIDIA key model blocks: %w", err))
	}
	keyPool := pool.New(settings, resolved.Clock)
	keyPool.LoadSnapshot(keySnapshots, modelBlocks)
	descriptor, err := nvidiaDescriptor(resolved.Config)
	if err != nil {
		return nil, closeAfterInitializationError(db, reader, err)
	}
	base := resolved.NVIDIAHTTPClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	baseTransport, ok := base.(*http.Transport)
	if !ok {
		return nil, closeAfterInitializationError(db, reader, errors.New("initialize proxy manager: HTTP transport is required"))
	}
	proxySettings, err := xkproxy.NewSettingsService(ctx, db, keys, xkproxy.EnvironmentConfig{
		URL: resolved.Config.XKProxyURL, AuthKey: resolved.Config.XKProxyAuthKey,
	}, toCollectorConfig(resolved.Config.XKPool), baseTransport, resolved.Logger)
	if err != nil {
		return nil, closeAfterInitializationError(db, reader, fmt.Errorf("initialize proxy settings: %w", err))
	}
	proxy := proxySettings.Switcher()
	nvidiaClient, err := nvidia.NewClient(resolved.NVIDIAHTTPClient, descriptor, settings, proxy)
	if err != nil {
		proxySettings.Close()
		return nil, closeAfterInitializationError(db, reader, fmt.Errorf("initialize NVIDIA client: %w", err))
	}
	var openCodeFreeClient *opencodefree.Client
	if resolved.Config.OpenCodeFreeBaseURL != nil {
		openCodeFreeClient, err = opencodefree.NewClient(resolved.NVIDIAHTTPClient, resolved.Config.OpenCodeFreeBaseURL, resolved.Config.OpenCodeFreeAuthKey)
		if err != nil {
			nvidiaClient.Close()
			proxySettings.Close()
			return nil, closeAfterInitializationError(db, reader, fmt.Errorf("initialize OpenCodeFree client: %w", err))
		}
		// The gateway is reached through the same pooled exits as NVIDIA so the
		// router's own address stays off the wire on both providers.
		openCodeFreeClient.WithProxy(proxy)
	}
	nvidiaKeys := nvidiakey.NewService(keyRepository, keys, nvidiaClient, resolved.Clock)
	healthChecker := nvidiakey.NewHealthChecker(keyRepository, resolved.Clock, nvidiakey.HealthCheckerOptions{
		Logger: resolved.Logger,
	})
	healthChecker.WireProbe(nvidiaKeys.ProbeHealth)
	healthChecker.WireWriter(nvidiaKeys)
	// Mirror DB recovery into pool state so a key the checker revives is
	// immediately acquirable without waiting for the next restart.
	healthChecker.WireSync(keyPool.ApplySuccess)
	// Probe half-open keys right after their cooldown expires instead of
	// waiting out the full sweep interval (half-open circuit recovery).
	healthChecker.WireCooldownExpiry(keyRepository.EarliestCooldownExpiry)
	models := modelcatalog.NewService(modelRepository, nvidiaKeys, nvidiaClient, descriptor, resolved.Clock)
	models.WithOpenCodeFree(openCodeFreeClient)
	modelHealthRepository := modelhealth.NewRepository(db).WithReader(reader)
	modelHealth := modelhealth.NewService(modelHealthRepository, models, models, nvidiaKeys, resolved.Clock, resolved.Logger)
	accessKeys := accesskey.NewService(accesskey.NewRepository(db).WithReader(reader), keys, resolved.Clock)
	adminRepository := adminauth.NewRepository(db, resolved.Clock)
	originPolicy := adminauth.OriginPolicy{ExternalOrigin: resolved.Config.AdminExternalOrigin, TrustedProxies: resolved.Config.TrustedProxyCIDRs}
	adminSecurity := adminapi.NewAuth(adminRepository, adminauth.NewSessionService(db, resolved.Clock, keys, resolved.Config.AdminSecureCookie).WithReader(reader), adminauth.NewLoginLimiter(resolved.Clock), originPolicy)
	auditRepository := adminaudit.NewRepository(db).WithReader(reader)
	auditRecorder := adminaudit.NewRecorder(auditRepository, resolved.Logger)
	providerCredentialRepository := providercredential.NewRepository(db, resolved.Clock, keys)
	adminManagement := adminapi.NewManagement(
		adminapi.NewNVIDIAKeys(nvidiaKeys, keyPool),
		adminapi.NewAccessKeys(accessKeys),
		adminapi.NewModels(models, nvidiaKeys, keyPool),
		adminapi.NewProxyPool(proxySettings),
		adminapi.NewAuditLogs(auditRepository),
		adminapi.NewProviderCredentials(providerCredentialRepository),
		adminapi.NewModelTestJobs(models),
		adminapi.NewModelHealth(modelHealth),
	)
	attempts := router.NewAttempt(settings, keyPool, nvidiaKeys, nvidiaKeys, keyPool, resolved.Clock, keyPool)
	observabilityRepository := observability.NewRepository(db).WithReader(reader)
	// Wrap the repository with a buffering recorder so request_logs writes
	// move off the hot path: per-request Record only enqueues, a background
	// flusher persists batches in a single SQLite transaction (audit #25).
	requestRecorder := observability.NewBufferRecorder(observabilityRepository, resolved.Clock, observability.BufferOptions{
		Logger: resolved.Logger,
	})
	// The event hub feeds the admin live view: every completed request record is
	// broadcast (with a bounded replay ring) so a connected SSE client sees
	// activity in near-real time without polling the DB. Publish is off the hot
	// path (non-blocking hub fan-out).
	eventHub := eventhub.New(0)
	requestEventSink := func(record observability.RequestRecord) error {
		if record.Endpoint == "" {
			return nil
		}
		eventHub.Publish(eventhub.Event{Type: "request", Serialized: adminapi.RequestEventLine(record)})
		return nil
	}
	observe := func(next http.Handler) http.Handler {
		guarded := httpapi.DataMiddleware(accessKeys, next)
		return observedHandler(requestRecorder, resolved.Clock, resolved.Logger, guarded, requestEventSink)
	} // The embedding cache exact-matches repeat (model, input) requests so
	// identical vectors skip the upstream. It is in-memory and bounded; the max
	// entry count follows the runtime setting on each embedding request.
	embeddingCache := embedcache.New(settings.Snapshot().EmbeddingCacheMaxEntries)
	chatHandler := v1.NewChat(models, attempts, nvidiaClient).WithRuntimeConfig(settings)
	// A nil *opencodefree.Client must not be assigned to the optional interface:
	// Go would wrap the nil pointer in a non-nil interface value and the route
	// would panic instead of answering 503 when the gateway is unconfigured.
	if openCodeFreeClient != nil {
		chatHandler.WithOpenCodeFree(openCodeFreeClient)
	}
	chat := observe(chatHandler)
	responsesHandler := v1.NewResponses(models, attempts, nvidiaClient).WithRuntimeConfig(settings)
	if openCodeFreeClient != nil {
		responsesHandler.WithOpenCodeFree(openCodeFreeClient)
	}
	responses := observe(responsesHandler)
	embeddings := observe(v1.NewEmbeddings(models, attempts, nvidiaClient, settings, embeddingCache))
	audio := observe(v1.NewAudio(models, attempts, nvidiaClient, resolved.Config.TempDir))
	speech := observe(v1.NewSpeech(models, attempts, nvidiaClient))
	modelList := observe(v1.NewModels(models))
	frontend, err := webui.NewEmbeddedHandler()
	if err != nil {
		nvidiaClient.Close()
		proxySettings.Close()
		return nil, closeAfterInitializationError(db, reader, fmt.Errorf("initialize embedded frontend: %w", err))
	}

	resolved.DB = db
	cleanupCtx, cleanupCancel := context.WithCancel(context.Background())
	cleanupDone := make(chan struct{})
	recorderCtx, recorderCancel := context.WithCancel(context.Background())
	recorderDone := make(chan struct{})
	healthCtx, healthCancel := context.WithCancel(context.Background())
	healthDone := make(chan struct{})
	rootCtx, rootCancel := context.WithCancel(context.Background())
	app := &App{
		Dependencies: resolved, db: db, dbReader: reader, dbLock: dbLock, Pool: keyPool, RuntimeSettings: settings,
		proxy: proxy, proxySettings: proxySettings, nvidiaClient: nvidiaClient,
		requestRecorder: requestRecorder, healthChecker: healthChecker,

		cleanupCancel: cleanupCancel, cleanupDone: cleanupDone,
		recorderCancel: recorderCancel, recorderDone: recorderDone,
		healthCancel: healthCancel, healthDone: healthDone,
		rootCancel: rootCancel,
	}
	app.modelHealthDone = modelHealth.Start(rootCtx)
	unsupported := observe(v1.Unsupported)
	statsHandler := adminapi.NewStats(observabilityRepository, resolved.Clock)
	monitoringHandler := adminapi.NewMonitoring(observabilityRepository, resolved.Clock)
	metricsHandler := metricsapi.New(keyPool, observabilityRepository, requestRecorder)
	// The built-in proxy pool is the project's core, so its live health belongs
	// in Prometheus next to the key pool: operators can alert on "pool drained"
	// without scraping the admin page. Static-proxy mode leaves this nil and the
	// pool metrics absent, which is itself a distinguishable state.
	metricsHandler.WithProxyPool(proxySettings)
	// The audit middleware wraps the full router so it observes both management
	// mutations (already carrying a principal from RequireManagement) and
	// unauthenticated attempts at /admin/api/auth/*; its path guard leaves
	// /v1, /metrics and the frontend untouched.
	router := adminapi.AuditMiddleware(auditRecorder, resolved.Config.TrustedProxyCIDRs, httpapi.NewRouter(
		health.New(db, keys, app.shutting.Load).WithReader(reader), chat, responses, embeddings, audio, speech, modelList, unsupported,
		adminSecurity, adminManagement, adminapi.NewSettings(settings), adminapi.NewRuntime(keyPool), frontend,
		statsHandler, monitoringHandler, metricsHandler, adminapi.NewEventStream(eventHub),
	))
	app.handler = httpapi.RecoverMiddleware(resolved.Logger, shutdownMiddleware(app.shutting.Load, router))
	app.Server = NewServer(resolved.Config.ListenAddress, app.handler, settings, func() { app.beginShutdown(0) })
	app.Server.setRootContext(rootCtx)

	app.startBackgroundWorkers(rootCtx, cleanupCtx, recorderCtx, healthCtx, settings, models, modelHealth, observabilityRepository, adminRepository, resolved)

	lockTransferred = true
	return app, nil
}

func (a *App) startBackgroundWorkers(
	rootCtx, cleanupCtx, recorderCtx, healthCtx context.Context,
	settings *runtimeconfig.Store,
	models *modelcatalog.Service,
	modelHealth *modelhealth.Service,
	observabilityRepository *observability.Repository,
	adminRepository *adminauth.Repository,
	resolved Dependencies,
) {
	observabilityWorker := observability.NewCleanupWorker(observabilityRepository, resolved.Clock, resolved.Logger, settings)
	adminSessionWorker := adminauth.NewSessionCleanupWorker(adminRepository, resolved.Clock, resolved.Logger)
	var cleanupWorkers sync.WaitGroup
	cleanupWorkers.Add(2)
	go func() {
		defer cleanupWorkers.Done()
		observabilityWorker.Run(cleanupCtx)
	}()
	go func() {
		defer cleanupWorkers.Done()
		adminSessionWorker.Run(cleanupCtx)
	}()
	go func() {
		cleanupWorkers.Wait()
		close(a.cleanupDone)
	}()
	// Flusher pairs with the buffer recorder: it must outlive request serving
	// long enough to drain the in-memory queue, so it gets its own ctx that
	// shutdown.go cancels right before closing the DB.
	go func() {
		defer close(a.recorderDone)
		a.requestRecorder.Run(recorderCtx)
	}()
	// Health checker pairs with the request path: it independently probes
	// unhealthy keys and recovers valid ones so a user request doesn't pay the
	// first failure after a key recovers from cooldown.
	go func() {
		defer close(a.healthDone)
		a.healthChecker.Run(healthCtx)
	}()
	// Periodic OpenCodeFree catalog sync: keeps the enabled free list aligned
	// with the gateway's live /models so a 6-model outage (2026-08-19) cannot
	// recur without operator intervention. No-op when the gateway is unconfigured.
	a.ocfSyncDone = models.StartOpenCodeFreeSync(rootCtx, time.Hour)
	// Startup capability-metadata check: a reasoning model whose profile cannot
	// express any level (e.g. levels=[none] with zero_allowed=false) answers 501
	// to every effort request. Log the offenders so the operator can PATCH them;
	// nothing is auto-written (2026-08-25 llama incident).
	checkCtx, checkCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if broken, ids, err := models.CountUnexpressibleReasoningProfiles(checkCtx); err != nil {
		resolved.Logger.Warn("reasoning profile consistency check failed", "error", err)
	} else if broken > 0 {
		resolved.Logger.Warn("reasoning models with unexpressible profiles found",
			"count", broken, "public_ids", strings.Join(ids, ","))
	}
	checkCancel()
	// Periodic capability probe (migration 045): re-runs the detailed probe so
	// tools/reasoning metadata tracks the upstream instead of drifting. The
	// enabled flag is read per cycle from runtime settings, so toggling it in
	// the admin panel takes effect without a restart.
	capabilityProbe := modelcatalog.NewCapabilityProbeRunner(models, resolved.Logger)
	a.capabilityProbeDone = capabilityProbe.Start(rootCtx, time.Duration(probeIntervalHours(settings.Snapshot().CapabilityProbeIntervalHours))*time.Hour, func() bool {
		return settings.Snapshot().CapabilityProbeEnabled
	})
}

func (a *App) Handler() http.Handler {
	return a.handler
}

// probeIntervalHours resolves the operator's cadence to the documented default
// when the row has not been through migration 045 (interval column = 0).
func probeIntervalHours(configured int) int {
	if configured <= 0 {
		return 24
	}
	return configured
}

// FlushObservability synchronously persists any buffered request-log records.
// Tests and admin surfaces use it to make buffered logs visible without
// waiting on the flusher's timer. Safe to call when the recorder is unstarted
// or already stopped (no-op).
func (a *App) FlushObservability(ctx context.Context) error {
	if a.requestRecorder == nil {
		return nil
	}
	return a.requestRecorder.ForceFlush(ctx)
}

func nvidiaDescriptor(cfg config.Config) (nvidia.Descriptor, error) {
	descriptor := nvidia.DefaultDescriptor()
	if cfg.NVIDIABaseURL == nil {
		return descriptor, nil
	}
	rewritten, err := descriptor.WithBaseURL(cfg.NVIDIABaseURL)
	if err != nil {
		return nvidia.Descriptor{}, fmt.Errorf("configure NVIDIA endpoints: %w", err)
	}
	return rewritten, nil
}

// toCollectorConfig bridges the config-layer built-in pool settings to the
// xkproxy collector contract. It returns nil when the pool is not configured.
func toCollectorConfig(pool *config.XKPoolConfig) *xkproxy.CollectorConfig {
	if pool == nil {
		return nil
	}
	return &xkproxy.CollectorConfig{
		UpstreamURL:       pool.UpstreamURL,
		UpstreamTimeout:   pool.UpstreamTimeout,
		ValidationURL:     pool.ValidationURL,
		ValidationStatus:  pool.ValidationStatus,
		ValidationTimeout: pool.ValidationTimeout,
		MaxLatency:        pool.MaxLatency,
		Interval:          pool.Interval,
		ProxyTTL:          pool.ProxyTTL,
		ExpectedQty:       pool.ExpectedQty,
		Concurrency:       pool.Concurrency,
	}
}

func resolveDependencies(dependencies Dependencies) (Dependencies, error) {
	if dependencies.Config.ListenAddress == "" && dependencies.Config.DataDir == "" && dependencies.Config.TempDir == "" && dependencies.Config.MasterKey == ([32]byte{}) && dependencies.Config.InitialAdminPassword == "" && !dependencies.Config.AdminSecureCookie && dependencies.Config.AdminExternalOrigin == nil && len(dependencies.Config.TrustedProxyCIDRs) == 0 && dependencies.Config.NVIDIABaseURL == nil && dependencies.Config.OpenCodeFreeBaseURL == nil && dependencies.Config.OpenCodeFreeAuthKey == "" && dependencies.Config.XKProxyURL == nil && dependencies.Config.XKProxyAuthKey == "" && dependencies.Config.XKPool == nil {
		loaded, err := config.LoadFromEnv(config.LoadOptions{})
		if err != nil {
			return Dependencies{}, fmt.Errorf("load configuration: %w", err)
		}
		dependencies.Config = loaded
	}
	if dependencies.Logger == nil {
		dependencies.Logger = slog.Default()
	}
	if dependencies.Clock == nil {
		dependencies.Clock = clock.RealClock{}
	}
	if dependencies.NVIDIAHTTPClient == nil {
		dependencies.NVIDIAHTTPClient = http.DefaultClient
	}
	return dependencies, nil
}

// openDatabase returns the writer pool and, when this process owns the file, a
// separate read-only pool. An injected DB (tests, CLI) has no path to reopen,
// so the reader is nil there and repositories fall back to the writer.
func openDatabase(dependencies Dependencies) (*sql.DB, *sql.DB, error) {
	if dependencies.DB != nil {
		return dependencies.DB, nil, nil
	}
	if err := os.MkdirAll(dependencies.Config.DataDir, 0o750); err != nil {
		return nil, nil, fmt.Errorf("create data directory: %w", err)
	}
	path := filepath.Join(dependencies.Config.DataDir, routerDBFilename)
	db, err := database.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open router database: %w", err)
	}
	// Must follow Open: a read-only connection cannot create the WAL index, so
	// the writer has to have initialized it first.
	reader, err := database.OpenReader(path)
	if err != nil {
		return nil, nil, closeAfterInitializationError(db, nil, fmt.Errorf("open router database reader pool: %w", err))
	}
	return db, reader, nil
}

func initialize(ctx context.Context, db *sql.DB, dependencies Dependencies) (*crypto.KeySet, error) {
	activeVersion := dependencies.Config.MasterKeyVersion
	if activeVersion <= 0 {
		activeVersion = 1
	}
	keys, err := crypto.NewVersioned(activeVersion, dependencies.Config.MasterKey)
	if err != nil {
		return nil, fmt.Errorf("create crypto key set: %w", err)
	}
	if dependencies.Config.LegacyMasterKey != nil {
		legacyVersion := dependencies.Config.LegacyMasterKeyVersion
		if legacyVersion <= 0 {
			legacyVersion = 1
		}
		keys, err = keys.WithLegacyMasterKey(legacyVersion, *dependencies.Config.LegacyMasterKey)
		if err != nil {
			return nil, fmt.Errorf("add legacy crypto key: %w", err)
		}
	}
	if err := keys.EnsureSentinel(ctx, db); err != nil {
		return nil, fmt.Errorf("ensure crypto sentinel: %w", err)
	}
	if err := adminauth.NewRepository(db, dependencies.Clock).EnsureAdmin(ctx, dependencies.Config.InitialAdminPassword); err != nil {
		return nil, fmt.Errorf("initialize administrator: %w", err)
	}
	return keys, nil
}

func closeAfterInitializationError(db *sql.DB, reader *sql.DB, operationErr error) error {
	closeErrs := []error{operationErr}
	if reader != nil {
		closeErrs = append(closeErrs, reader.Close())
	}
	closeErrs = append(closeErrs, db.Close())
	if joined := errors.Join(closeErrs[1:]...); joined != nil {
		return fmt.Errorf("initialize application and close database: %w", errors.Join(operationErr, joined))
	}
	return operationErr
}

// listenExposure classifies a listen address for the startup security audit.
// config.ListenAddress is a host:port string (the default is "0.0.0.0:3756"), so
// the host has to be split out before it can be compared against a loopback or
// wildcard address; comparing the whole value matches neither, which silently
// turned the audit into three false warnings for the loopback default and let the
// bare ":port" wildcard form past the public-binding check.
func listenExposure(address string) (loopback, wildcard bool) {
	host := address
	if split, _, err := net.SplitHostPort(address); err == nil {
		host = split
	}
	if host == "" {
		// ":3756" and "" both bind every interface: net treats a missing host as
		// the unspecified address, not as loopback.
		return false, true
	}
	if strings.EqualFold(host, "localhost") {
		return true, false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A hostname that is not resolved here; report it as exposed so the audit
		// errs toward warning rather than staying silent.
		return false, false
	}
	return ip.IsLoopback(), ip.IsUnspecified()
}

// checkProductionSecurity logs warnings when the configuration may expose
// sensitive data or management endpoints without proper protection.
func checkProductionSecurity(cfg config.Config, logger *slog.Logger) {
	if cfg.ListenAddress == "" {
		// config.Load always substitutes the default, so an empty address means the
		// caller builds Config by hand and serves through its own listener (the
		// in-process test harness). There is no socket to audit.
		return
	}
	isLoopback, isWildcard := listenExposure(cfg.ListenAddress)

	if !isLoopback {
		// Listening on non-loopback - check for security configurations
		if !cfg.AdminSecureCookie {
			logger.Warn(
				"SECURITY: AdminSecureCookie is disabled while listening on non-loopback address",
				"listen_address", cfg.ListenAddress,
				"recommendation", "Set NVIDIA_ROUTER_ADMIN_SECURE_COOKIE=true and use HTTPS reverse proxy",
			)
		}

		if cfg.AdminExternalOrigin == nil {
			logger.Warn(
				"SECURITY: AdminExternalOrigin is not configured while listening on non-loopback address",
				"listen_address", cfg.ListenAddress,
				"recommendation", "Set NVIDIA_ROUTER_ADMIN_EXTERNAL_ORIGIN to your HTTPS origin (e.g., https://example.com)",
			)
		}

		if len(cfg.TrustedProxyCIDRs) == 0 {
			logger.Warn(
				"SECURITY: TrustedProxyCIDRs is not configured while listening on non-loopback address",
				"listen_address", cfg.ListenAddress,
				"recommendation", "Set NVIDIA_ROUTER_TRUSTED_PROXY_CIDRS to your reverse proxy IP range",
			)
		}

		// Check for completely public binding
		if isWildcard {
			logger.Warn(
				"SECURITY: Listening on all interfaces without reverse proxy exposes plaintext HTTP",
				"listen_address", cfg.ListenAddress,
				"risk", "Access Keys, admin credentials, prompts, and responses will be transmitted in plaintext",
				"recommendation", "Use HTTPS reverse proxy (nginx, Caddy) or bind to loopback only",
			)
		}
	}
}

func (a *App) Serve(ctx context.Context) error {
	err := a.Server.ListenAndServe(ctx)
	if closeErr := a.Close(); closeErr != nil {
		return fmt.Errorf("serve application: %w", errors.Join(err, closeErr))
	}
	if err != nil {
		return fmt.Errorf("serve application: %w", err)
	}
	return nil
}

func (a *App) Close() error {
	a.beginShutdown(0)
	a.close.Do(func() {
		a.closeErr = a.finishShutdown()
		if a.dbLock != nil {
			a.closeErr = errors.Join(a.closeErr, a.dbLock.Close())
		}
	})
	if a.closeErr != nil {
		return fmt.Errorf("close router database: %w", a.closeErr)
	}
	return nil
}
