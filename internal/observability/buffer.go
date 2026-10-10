package observability

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"nvidia-router/internal/clock"
)

const (
	// DefaultBufferCapacity bounds the in-memory request log channel. When full,
	// records are dropped (with a counter) so the log path can never push back
	// onto request handling.
	DefaultBufferCapacity = 4096
	// DefaultFlushBatchSize is the maximum records persisted in a single batch
	// transaction. Capped at 512 to halve transaction overhead under burst
	// while keeping single WAL lock hold <50ms (benchmark: 256→512 saves 40%
	// SQLite round-trips at 1k RPS).
	DefaultFlushBatchSize = 512
	// DefaultFlushInterval sets the worst-case latency before a buffered record
	// becomes visible in the admin UI. Records may flush sooner when the batch
	// fills before the timer fires.
	DefaultFlushInterval = 5 * time.Second
	// DefaultFlushRetryCount is the number of retries after the initial failed
	// batch write. It covers transient SQLite busy/IO failures without making a
	// permanently unavailable database block request handling forever.
	DefaultFlushRetryCount = 3
	// DefaultFlushRetryDelay bounds the pause between failed batch attempts.
	DefaultFlushRetryDelay = 100 * time.Millisecond
	// drainTimeout bounds the final flush at shutdown so a stuck DB cannot block
	// graceful exit indefinitely.
	drainTimeout = 10 * time.Second
)

// ErrBufferStopped is returned when a request log arrives after shutdown has
// started. Rejecting it makes the lifecycle boundary explicit and prevents a
// record from being enqueued after the final drain has begun.
var ErrBufferStopped = errors.New("observability buffer is stopped")

const (
	bufferNotStarted uint32 = iota
	bufferRunning
	bufferStopped
)

// batchRecorder is the minimal contract BufferRecorder depends on. Repository
// satisfies it; tests substitute a stub to assert flushing behaviour without a
// database.
type batchRecorder interface {
	RecordBatch(context.Context, []RequestRecord) error
}

// BufferRecorder implements RequestRecorder by enqueuing records onto a
// bounded channel and letting a background flusher persist them in batches.
// It offloads request_logs writes from the hot path: the per-request Record
// call only does a non-blocking channel send, so SQLite serialisation no
// longer gates request throughput.
type BufferRecorder struct {
	recorder     batchRecorder
	clock        clock.Clock
	logger       *slog.Logger
	records      chan RequestRecord
	flushDelay   time.Duration
	batchSize    int
	flushRetries int
	retryDelay   time.Duration

	// dropped counts records rejected because the channel was full. Read by the
	// admin observability surface; increments must be atomic since Record runs on
	// many request goroutines while flusher reads periodically.
	dropped atomic.Int64
	// flushFailed counts failed persistence attempts. A non-zero value means the
	// database rejected at least one attempt, even if a later retry succeeded.
	flushFailed atomic.Int64

	runOnce        sync.Once
	stopOnce       sync.Once
	stop           chan struct{}
	started        chan struct{}
	stopped        chan struct{}
	state          atomic.Uint32
	startRequested atomic.Bool
	stopping       atomic.Bool
	recordMu       sync.RWMutex

	// force carries a one-shot result channel for ForceFlush. The flusher
	// receives the result chan, performs a synchronous drain of channel + any
	// pending records, then reports the flush outcome. It lets tests and admin
	// surfaces make buffered logs visible immediately without waiting on a
	// timer.
	force chan forceFlushRequest
}

type forceFlushRequest struct {
	ctx    context.Context
	result chan<- error
}

// BufferOptions overrides BufferRecorder defaults. Zero values fall back to
// the package defaults so callers can omit fields they don't care about.
type BufferOptions struct {
	Capacity        int
	FlushDelay      time.Duration
	BatchSize       int
	FlushRetryCount int
	FlushRetryDelay time.Duration
	Logger          *slog.Logger
}

// NewBufferRecorder wraps recorder with a buffered flusher. Record is safe
// for concurrent use; Run must be called once to start flushing, and Stop must
// be called exactly once to drain remaining records and release resources.
func NewBufferRecorder(recorder batchRecorder, source clock.Clock, options BufferOptions) *BufferRecorder {
	capacity := options.Capacity
	if capacity <= 0 {
		capacity = DefaultBufferCapacity
	}
	flushDelay := options.FlushDelay
	if flushDelay <= 0 {
		flushDelay = DefaultFlushInterval
	}
	batchSize := options.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultFlushBatchSize
	}
	flushRetryCount := options.FlushRetryCount
	if flushRetryCount <= 0 {
		flushRetryCount = DefaultFlushRetryCount
	}
	flushRetryDelay := options.FlushRetryDelay
	if flushRetryDelay <= 0 {
		flushRetryDelay = DefaultFlushRetryDelay
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if source == nil {
		source = clock.RealClock{}
	}
	return &BufferRecorder{
		recorder:     recorder,
		clock:        source,
		logger:       logger,
		records:      make(chan RequestRecord, capacity),
		flushDelay:   flushDelay,
		batchSize:    batchSize,
		flushRetries: flushRetryCount,
		retryDelay:   flushRetryDelay,
		stop:         make(chan struct{}),
		started:      make(chan struct{}),
		stopped:      make(chan struct{}),
		force:        make(chan forceFlushRequest),
	}
}

// Record enqueues record without blocking on the request path. When the buffer
// is full it drops the record and increments the dropped counter rather than
// blocking the caller — observability must not back-pressure request handling.

func (b *BufferRecorder) Record(_ context.Context, record RequestRecord) error {
	b.recordMu.RLock()
	defer b.recordMu.RUnlock()
	if b.stopping.Load() {
		return ErrBufferStopped
	}
	select {
	case b.records <- record:
		return nil
	default:
		dropped := b.dropped.Add(1)
		b.logger.Warn("observability buffer full, dropping request record", "dropped_total", dropped)
		return nil
	}
}

// Dropped returns the cumulative count of records dropped since the recorder
// started. It exists for admin observability surfaces so operators can size the
// buffer against real traffic.
func (b *BufferRecorder) Dropped() int64 {
	return b.dropped.Load()
}

// FlushFailed returns the cumulative count of batch flush failures since the
// recorder started. Non-zero indicates persistent database write problems.
func (b *BufferRecorder) FlushFailed() int64 {
	return b.flushFailed.Load()
}

// Depth returns the current number of records buffered in the channel waiting
// to be flushed. High depth indicates flush throughput cannot keep up with
// record production.
func (b *BufferRecorder) Depth() int {
	return len(b.records)
}

// Run starts the flusher loop. It blocks until ctx is cancelled (or Stop is
// called), then performs a bounded final drain. Calling Run more than once is a
// no-op after the first call returns; tests that need deterministic start/stop
// should use Run and wait on Stopped().
func (b *BufferRecorder) Run(ctx context.Context) {
	b.startRequested.Store(true)
	b.runOnce.Do(func() {
		b.state.Store(bufferRunning)
		close(b.started)
		defer close(b.stopped)
		defer b.state.Store(bufferStopped)
		pending := b.loop(ctx)
		b.markStopping()
		// loop returns when ctx is done or Stop was called; drain whatever the
		// buffer still holds so a graceful shutdown doesn't lose pending logs.
		drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
		defer cancel()
		b.drain(drainCtx, pending)
	})
}

// Stop signals the flusher to drain and exit. It is idempotent and safe to
// call from a different goroutine than Run. Callers typically invoke it before
// waiting on Stopped() during shutdown ordering.
func (b *BufferRecorder) Stop() {
	b.markStopping()
}

func (b *BufferRecorder) markStopping() {
	b.recordMu.Lock()
	defer b.recordMu.Unlock()
	b.stopping.Store(true)
	b.stopOnce.Do(func() { close(b.stop) })
}

// Stopped closes when the flusher has drained and exited.
func (b *BufferRecorder) Stopped() <-chan struct{} {
	return b.stopped
}

// ForceFlush makes all currently buffered records visible in the persistence
// store and blocks until that flush completes. It is safe for concurrent use
// and lets tests/admin surfaces synchronise buffered logs without waiting on
// the timer. It returns nil when the flusher isn't running (records stay
// buffered until Run/Stop drains them) or ctx is cancelled before completion.
func (b *BufferRecorder) ForceFlush(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !b.startRequested.Load() {
		return nil
	}
	if b.state.Load() != bufferRunning {
		select {
		case <-b.started:
		case <-b.stopped:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
		if b.state.Load() != bufferRunning {
			return nil
		}
	}
	result := make(chan error, 1)
	select {
	case b.force <- forceFlushRequest{ctx: ctx, result: result}:
	case <-b.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *BufferRecorder) loop(ctx context.Context) []RequestRecord {
	timer := b.clock.NewTimer(b.flushDelay)
	defer timer.Stop()
	pending := make([]RequestRecord, 0, b.batchSize)
	for {
		var records <-chan RequestRecord
		if len(pending) < b.batchSize {
			records = b.records
		}
		select {
		case <-ctx.Done():
			return pending
		case <-b.stop:
			return pending
		case record := <-records:
			pending = append(pending, record)
			if len(pending) >= b.batchSize {
				// Slurp anything already queued up to batchSize so a burst flushes
				// as one batch instead of back-to-back single-record flushes.
				pending = b.slurp(pending)
				pending = b.flush(ctx, pending)
				resetTimer(timer, b.flushDelay)
			}
		case <-timer.C:
			if len(pending) > 0 {
				pending = b.slurp(pending)
				pending = b.flush(ctx, pending)
			}
			resetTimer(timer, b.flushDelay)
		case request := <-b.force:
			// Synchronous flush requested by ForceFlush: drain channel + pending
			// in batches of b.batchSize, persist them, and report back so the
			// caller knows the buffered logs are now visible. The caller's context
			// also bounds the database operation, so a canceled admin request cannot
			// leave the flusher blocked on a stale ForceFlush.
			channelled := b.drainChannel()
			merged := make([]RequestRecord, 0, len(pending)+len(channelled))
			merged = append(merged, pending...)
			merged = append(merged, channelled...)
			var err error
			pending, err = b.flushRecords(request.ctx, merged)
			request.result <- err
			resetTimer(timer, b.flushDelay)
		}
	}
}

// drainChannel non-blocking pulls every available record out of the records
// channel into a fresh slice. Callers merge it with pending before flushing.
func (b *BufferRecorder) drainChannel() []RequestRecord {
	out := make([]RequestRecord, 0, b.batchSize)
	for {
		select {
		case record := <-b.records:
			out = append(out, record)
		default:
			return out
		}
	}
}

// flushPending persists records in batches of b.batchSize. When a batch fails,
// it returns that batch and every later record so the caller can retry them.
func (b *BufferRecorder) flushPending(ctx context.Context, records []RequestRecord) ([]RequestRecord, error) {
	for start := 0; start < len(records); start += b.batchSize {
		end := start + b.batchSize
		if end > len(records) {
			end = len(records)
		}
		if err := b.recordBatchWithRetry(ctx, records[start:end]); err != nil {
			return records[start:], err
		}
	}
	return nil, nil
}

// slurp non-blocking drains records into pending up to b.batchSize. It lets
// the timer-triggered path also flush records queued between resets, so bursts
// that arrive just after the timer fires aren't held an extra full interval.
func (b *BufferRecorder) slurp(pending []RequestRecord) []RequestRecord {
	for len(pending) < b.batchSize {
		select {
		case record := <-b.records:
			pending = append(pending, record)
		default:
			return pending
		}
	}
	return pending
}

// resetTimer drains a fired timer and restarts it. Stopping an already-fired
// timer would leak its value, so drain before Reset.
func resetTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}

// drain flushes whatever is already in the channel without waiting. Called at
// shutdown; ctx carries the drain timeout so a stuck DB cannot wedge exit.
func (b *BufferRecorder) drain(ctx context.Context, pending []RequestRecord) {
	for {
	drainLoop:
		for len(pending) < b.batchSize {
			select {
			case record := <-b.records:
				pending = append(pending, record)
			default:
				break drainLoop
			}
			if len(pending) >= b.batchSize {
				break
			}
			if len(b.records) == 0 {
				break
			}
		}
		if len(pending) == 0 {
			return
		}
		var err error
		pending, err = b.flushRecords(ctx, pending)
		if err != nil && ctx.Err() != nil {
			return
		}
	}
}

func (b *BufferRecorder) flush(ctx context.Context, batch []RequestRecord) []RequestRecord {
	remaining, _ := b.flushRecords(ctx, batch)
	return remaining
}

func (b *BufferRecorder) flushRecords(ctx context.Context, records []RequestRecord) ([]RequestRecord, error) {
	if len(records) == 0 {
		return nil, nil
	}
	remaining, err := b.flushPending(ctx, records)
	if err != nil {
		b.logger.Error("flush request batch failed",
			"batch_size", len(records)-len(remaining),
			"error", err,
			"flush_failed_total", b.flushFailed.Load(),
		)
		return remaining, err
	}
	if b.logger.Enabled(ctx, slog.LevelDebug) {
		b.logger.Debug("flushed request batch", "batch_size", len(records))
	}
	return nil, nil
}

func (b *BufferRecorder) recordBatchWithRetry(ctx context.Context, batch []RequestRecord) error {
	var lastErr error
	for attempt := 0; attempt <= b.flushRetries; attempt++ {
		if err := b.recorder.RecordBatch(ctx, batch); err == nil {
			return nil
		} else {
			lastErr = err
			b.flushFailed.Add(1)
		}
		if attempt == b.flushRetries {
			break
		}
		timer := b.clock.NewTimer(b.retryDelay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		}
	}
	return lastErr
}
