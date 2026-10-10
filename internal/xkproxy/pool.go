package xkproxy

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Outcome reports what a failure report did to the pool
type Outcome uint8

const (
	OutcomeCounted Outcome = iota
	OutcomeEjected
	OutcomeRemoved
)

// EjectionPolicy controls how failures translate into isolation and removal
type EjectionPolicy struct {
	FailureLimit int
	BaseDuration time.Duration
	MaxDuration  time.Duration
	MaxEjections int
	LatencyAlpha float64
	// HTTPFailureLimit is how many application-level failures (429/5xx through
	// this exit) isolate the proxy. Deliberately higher than FailureLimit: an
	// HTTP status is not by itself proof the exit is bad (it may be a shared
	// quota), so isolation requires a longer pattern (audit H8).
	HTTPFailureLimit int
}

const slowLatencyFactor = 3

// qualityExploreEvery gives an under-sampled but otherwise clean exit an
// occasional request. A bad exit cannot become preferred merely by being
// unexplored, while a newly collected exit is not permanently ignored.
const qualityExploreEvery = 8

const qualityMinRequestSamples = 3

// httpEjectSuccessWindow is how recently some exit must have served a real 2xx
// for HTTP failures on another exit to count toward isolation. It is the
// "pool is working" evidence: without it a key-level 429/5xx storm would blame
// and eject every exit, emptying the pool (audit H8).
const httpEjectSuccessWindow = 60 * time.Second

// httpFailureWindow bounds how long an exit's HTTP-failure count is allowed to
// accumulate. A slow-drip pattern spread over hours is not the sustained
// 429/5xx storm isolation is for; without the window a merely noisy exit would
// be ejected on a pattern it never actually maintained.
const httpFailureWindow = 60 * time.Second

// transportFailureWindow bounds how long an exit's transport failure count is
// allowed to accumulate without fresh failures. An exit that failed an hour ago
// and fails again today is not experiencing consecutive connection failures.
const transportFailureWindow = 60 * time.Second

// httpStatusOverloaded is NVIDIA's "overloaded" status. It describes the
// target's own load, not the exit: it recurs across every exit at once, so an
// exit must never be blamed for it (see ReportHTTPFailure).
const httpStatusOverloaded = 529

// upstreamOverloadWindow keeps a recent 529 visible long enough for the admin
// poller to explain an outage without turning an old overload into a current
// status. It is pool-wide because the signal describes NVIDIA, not an exit.
const upstreamOverloadWindow = 60 * time.Second

func (e EjectionPolicy) normalized() EjectionPolicy {
	if e.FailureLimit <= 0 {
		e.FailureLimit = 3
	}
	if e.BaseDuration <= 0 {
		e.BaseDuration = 10 * time.Second
	}
	if e.MaxDuration < e.BaseDuration {
		e.MaxDuration = e.BaseDuration
	}
	if e.MaxEjections <= 0 {
		e.MaxEjections = 3
	}
	if e.LatencyAlpha <= 0 || e.LatencyAlpha > 1 {
		e.LatencyAlpha = 0.3
	}
	if e.HTTPFailureLimit <= 0 {
		e.HTTPFailureLimit = 6
	}
	return e
}

type Pool struct {
	mu                     sync.RWMutex
	proxies                []Proxy
	selectionCursor        atomic.Uint64
	lastUpstreamOverloadAt time.Time

	// removed records proxies permanently ejected (EjectionCount exceeded
	// MaxEjections) so a later fetch does not immediately resurrect a
	// repeatedly-failing exit (audit M6). Entries expire after removalCooldown.
	removed map[string]time.Time

	stickyMu       sync.Mutex
	stickyBindings map[string]stickyEntry
}

// removalCooldown keeps a permanently-ejected proxy out of the pool after
// removal. The upstream provider may have fixed it or rotated it; after the
// window the pool admits it again on a fresh fetch. A window far shorter than
// the provider's own proxy churn lets a dead exit keep failing the whole pool.
const removalCooldown = 5 * time.Minute

type stickyEntry struct {
	proxyKey  string
	expiresAt time.Time
}

func NewPool() *Pool {
	return &Pool{removed: make(map[string]time.Time), stickyBindings: make(map[string]stickyEntry)}
}

func (p *Pool) Replace(proxies []Proxy) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.proxies = append([]Proxy(nil), proxies...)
	// A manual replace is an explicit override: reset the removal blacklist so
	// explicitly-provided proxies are admitted regardless of prior ejections.
	clear(p.removed)
}

func (p *Pool) Clear() {
	p.Replace(nil)
}

func (p *Pool) Peek(now time.Time) (Proxy, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	ordered, _ := p.orderedLocked(now, 0, false)
	if len(ordered) == 0 {
		return Proxy{}, false
	}
	return ordered[0], true
}

func (p *Pool) Get(now time.Time) (Proxy, bool) {
	candidates, _ := p.Candidates(now, 0)
	if len(candidates) == 0 {
		return Proxy{}, false
	}
	return candidates[0], true
}

// GetWithLatency selects the best exit honouring measured latency ordering
// (EWMA ascending first, unmeasured last with rotation-driven exploration).
// It is used when the operator enables latency-aware scheduling; the default
// Get keeps the legacy rotation + 3x-slow demotion behaviour.
func (p *Pool) GetWithLatency(now time.Time) (Proxy, bool) {
	candidates, _ := p.CandidatesWithLatency(now, 0, true)
	if len(candidates) == 0 {
		return Proxy{}, false
	}
	return candidates[0], true
}

// GetWithQuality selects by real request quality first. Probe latency is only
// a tie-breaker: a fast validation response is not evidence that NVIDIA will
// accept traffic through the exit.
func (p *Pool) GetWithQuality(now time.Time) (Proxy, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	candidates, _ := p.orderedLocked(now, 0, false)
	if len(candidates) == 0 {
		return Proxy{}, false
	}
	ordered := orderByQuality(candidates)
	selection := p.selectionCursor.Add(1) - 1
	if selection%qualityExploreEvery == qualityExploreEvery-1 {
		for _, candidate := range ordered[1:] {
			// Exploration is for learning about unknown exits, not for feeding an
			// exit whose only real request evidence is a failure: a failed sample
			// does not make the exit "unexplored", it makes it suspect.
			if candidate.RequestSamples() < qualityMinRequestSamples && candidate.HTTPFailCount == 0 && candidate.HealthFails == 0 && candidate.RequestFailureStreak == 0 {
				return candidate, true
			}
		}
	}
	return ordered[0], true
}

func orderByQuality(candidates []Proxy) []Proxy {
	ordered := append([]Proxy(nil), candidates...)
	slowThreshold := relativeSlowThreshold(ordered)
	// Precompute scores and effective latencies once per candidate so the sort
	// comparator does not repeatedly recompute QualityScore (division + branching)
	// on every comparison (O(n log n) calls). For n=4–10 the win is small per
	// request but accumulates at high RPS and keeps the comparator trivial.
	type scored struct {
		idx     int
		score   int
		latency time.Duration
		slow    bool
	}
	scoredCandidates := make([]scored, len(ordered))
	for i := range ordered {
		scoredCandidates[i] = scored{
			idx:     i,
			score:   ordered[i].QualityScore(),
			latency: ordered[i].EffectiveRequestLatency(),
			slow:    slowThreshold > 0 && ordered[i].RequestLatencyEWMA > slowThreshold,
		}
	}
	sort.SliceStable(scoredCandidates, func(a, b int) bool {
		sa, sb := scoredCandidates[a], scoredCandidates[b]
		if sa.slow != sb.slow {
			return !sa.slow
		}
		if sa.score != sb.score {
			return sa.score > sb.score
		}
		if sa.latency <= 0 {
			return false
		}
		if sb.latency <= 0 {
			return true
		}
		return sa.latency < sb.latency
	})
	result := make([]Proxy, len(ordered))
	for i, s := range scoredCandidates {
		result[i] = ordered[s.idx]
	}
	return result
}

// relativeSlowThreshold returns fastestRequestLatency×slowLatencyFactor across
// candidates with a measured real-request latency, or zero when no candidate has
// one. Probe latency is deliberately excluded: it is a different scale than a
// full request round-trip, so mixing the two would let a probe-only exit demote a
// proven (but slower) real-request exit.
func relativeSlowThreshold(candidates []Proxy) time.Duration {
	fastest := time.Duration(0)
	for _, candidate := range candidates {
		if candidate.RequestLatencyEWMA <= 0 {
			continue
		}
		if fastest == 0 || candidate.RequestLatencyEWMA < fastest {
			fastest = candidate.RequestLatencyEWMA
		}
	}
	if fastest == 0 {
		return 0
	}
	return fastest * slowLatencyFactor
}

func (p Proxy) RequestSamples() uint64 {
	return p.RequestSuccessCount + p.RequestFailureCount
}

func (p Proxy) EffectiveRequestLatency() time.Duration {
	if p.RequestLatencyEWMA > 0 {
		return p.RequestLatencyEWMA
	}
	return p.LatencyEWMA
}

// QualityScore returns a bounded, explainable score for real request routing.
// The neutral score for an untested exit is intentional: it can be explored,
// but cannot outrank an exit with a convincing success history by latency alone.
func (p Proxy) QualityScore() int {
	score := 50
	samples := p.RequestSamples()
	if samples > 0 {
		successRate := float64(p.RequestSuccessCount) / float64(samples)
		confidence := math.Min(float64(samples), 10) / 10
		score += int(math.Round((successRate - 0.5) * 60 * confidence))
	}
	score -= minInt(p.HTTPFailCount*10, 30)
	score -= minInt(p.HealthFails*10, 30)
	score -= minInt(p.RequestFailureStreak*8, 30)
	if p.RequestLatencyEWMA > 0 {
		switch {
		case p.RequestLatencyEWMA > 800*time.Millisecond:
			score -= 15
		case p.RequestLatencyEWMA > 300*time.Millisecond:
			score -= 8
		case p.RequestLatencyEWMA > 100*time.Millisecond:
			score -= 3
		}
	} else if p.LatencyEWMA > 0 {
		// Cold-start fallback: without a real-request sample, the collector probe
		// latency is the only evidence of exit speed. It is a different scale than
		// a full LLM round-trip, so it never demotes an exit that already has real
		// request latency — but for an untested exit, a very slow tunnel (a few
		// seconds) is a strong prior that the real request will be slow too, so
		// demote it now instead of letting the first request pay the penalty
		// (measured on the 星空 pool: probe 4.1s exit served requests at 4.8s).
		switch {
		case p.LatencyEWMA > 3*time.Second:
			score -= 15
		case p.LatencyEWMA > 2*time.Second:
			score -= 8
		case p.LatencyEWMA > time.Second:
			score -= 3
		}
	}
	if score < 0 {
		return 0
	}
	if score > 100 {
		return 100
	}
	return score
}

func minInt(value, limit int) int {
	if value < limit {
		return value
	}
	return limit
}

func (p *Pool) Candidates(now time.Time, minRemainingLife time.Duration) (candidates []Proxy, panicMode bool) {
	return p.CandidatesWithLatency(now, minRemainingLife, false)
}

func (p *Pool) CandidatesWithLatency(now time.Time, minRemainingLife time.Duration, preferLatency bool) (candidates []Proxy, panicMode bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	ordered, panicMode := p.orderedLocked(now, minRemainingLife, preferLatency)
	if len(ordered) == 0 {
		return nil, false
	}
	p.selectionCursor.Add(1)
	return ordered, panicMode
}

func (p *Pool) orderedLocked(now time.Time, minRemainingLife time.Duration, preferLatency bool) (ordered []Proxy, panicMode bool) {
	// Fast path: most requests use minRemainingLife==0 (no TTL floor).
	// Avoid allocating preferred/sufficientLive and skip RemainingLife checks.
	if minRemainingLife <= 0 {
		available := make([]Proxy, 0, len(p.proxies))
		live := make([]Proxy, 0, len(p.proxies))
		for _, proxy := range p.proxies {
			if !proxy.LiveAt(now) {
				continue
			}
			live = append(live, proxy)
			if proxy.AvailableAt(now) {
				available = append(available, proxy)
			}
		}
		if len(available) > 0 {
			return p.rotateFor(available, preferLatency), false
		}
		if len(live) > 0 {
			return p.rotateFor(live, preferLatency), true
		}
		return nil, false
	}
	// Slow path: TTL-aware filtering with preferred / panic fallback.
	live := make([]Proxy, 0, len(p.proxies))
	preferred := make([]Proxy, 0, len(p.proxies))
	for _, proxy := range p.proxies {
		if !proxy.LiveAt(now) {
			continue
		}
		live = append(live, proxy)
		if !proxy.AvailableAt(now) {
			continue
		}
		if proxy.RemainingLife(now) >= minRemainingLife {
			preferred = append(preferred, proxy)
		}
	}
	if len(preferred) > 0 {
		return p.rotateFor(preferred, preferLatency), false
	}
	sufficientLive := make([]Proxy, 0, len(live))
	for _, proxy := range live {
		if proxy.RemainingLife(now) >= minRemainingLife {
			sufficientLive = append(sufficientLive, proxy)
		}
	}
	if len(sufficientLive) > 0 {
		return p.rotateFor(sufficientLive, preferLatency), true
	}
	return nil, false
}

func (p *Pool) rotateFor(source []Proxy, preferLatency bool) []Proxy {
	if preferLatency {
		return p.rotateLatency(source)
	}
	return p.rotate(source)
}

// rotateLatency orders the selection window by measured EWMA so faster exits
// are served first, while still rotating the window so every exit (including
// unmeasured newcomers) is served as the cursor advances under load. Unmeasured
// exits sort last: they keep exploration opportunities as the rotation cycles
// past the measured ones, but do not displace exits with proven latency.
func (p *Pool) rotateLatency(source []Proxy) []Proxy {
	length := len(source)
	ordered := make([]Proxy, length)
	copy(ordered, source)
	sort.SliceStable(ordered, func(a, b int) bool {
		latencyA, latencyB := ordered[a].LatencyEWMA, ordered[b].LatencyEWMA
		if latencyA <= 0 {
			return false
		}
		if latencyB <= 0 {
			return true
		}
		return latencyA < latencyB
	})
	cursor := p.selectionCursor.Load()
	start := int(cursor % uint64(length))
	// Rotate in place via append to avoid second full allocation + manual copy.
	// append(ordered[start:], ordered[:start]...) reuses the same backing array
	// capacity and is measurably cheaper at high RPS than the explicit loop.
	result := make([]Proxy, 0, length)
	result = append(result, ordered[start:]...)
	result = append(result, ordered[:start]...)
	return result
}

func (p *Pool) rotate(source []Proxy) []Proxy {
	length := len(source)
	cursor := p.selectionCursor.Load()
	start := length - 1 - int(cursor%uint64(length))
	result := make([]Proxy, length)
	for offset := range result {
		index := (start - offset + length) % length
		result[offset] = source[index]
	}
	return demoteSlow(result)
}

func demoteSlow(candidates []Proxy) []Proxy {
	fastest := time.Duration(0)
	for _, candidate := range candidates {
		if candidate.LatencyEWMA <= 0 {
			continue
		}
		if fastest == 0 || candidate.LatencyEWMA < fastest {
			fastest = candidate.LatencyEWMA
		}
	}
	if fastest == 0 {
		return candidates
	}
	threshold := fastest * slowLatencyFactor
	// Stable partition in O(n) without sorting: fast first, slow last, preserving
	// original rotation order within each group. The previous implementation used
	// sort.SliceStable over an index array (O(n log n) comparisons) just to
	// partition by a boolean.
	fast := make([]Proxy, 0, len(candidates))
	slow := make([]Proxy, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.LatencyEWMA > threshold {
			slow = append(slow, candidate)
		} else {
			fast = append(fast, candidate)
		}
	}
	if len(slow) == 0 {
		return candidates
	}
	return append(fast, slow...)
}

func (p *Pool) Merge(now time.Time, incoming []Proxy, policies ...EjectionPolicy) {
	policy := EjectionPolicy{}
	if len(policies) > 0 {
		policy = policies[0]
	}
	policy = policy.normalized()

	p.mu.Lock()
	defer p.mu.Unlock()

	p.pruneLocked(now)
	index := make(map[string]int, len(p.proxies))
	for i, existing := range p.proxies {
		index[existing.Key()] = i
	}

	for _, candidate := range incoming {
		if !candidate.LiveAt(now) {
			continue
		}
		if until, gone := p.removed[candidate.Key()]; gone && now.Before(until) {
			// The proxy was permanently ejected and its removal cooldown has not
			// expired: do not re-admit it even though the upstream keeps returning
			// it (audit M6). After the cooldown the pool admits it again on a
			// fresh fetch, giving a fixed/rotated exit a second chance.
			continue
		}
		position, found := index[candidate.Key()]
		if !found {
			// A proxy that was permanently removed (EjectionCount exceeded
			// MaxEjections) must not be silently resurrected by the next fetch:
			// it was failing repeatedly, and re-adding it fresh would restart the
			// eject/re-add churn (audit M6). The upstream is still free to return
			// it again next cycle; the pool simply does not re-admit it here.
			position = len(p.proxies)
			index[candidate.Key()] = position
			p.proxies = append(p.proxies, candidate)
			continue
		}

		current := p.proxies[position]
		current.ValidatedAt = candidate.ValidatedAt
		current.Scheme = candidate.Scheme
		current.Username = candidate.Username
		current.Password = candidate.Password
		if candidate.LatencyEWMA > 0 {
			current.LatencyEWMA = blendLatency(current.LatencyEWMA, candidate.LatencyEWMA, policy.LatencyAlpha)
			current.LatencySamples++
		}
		if !candidate.FetchedAt.IsZero() && (current.FetchedAt.IsZero() || candidate.FetchedAt.Before(current.FetchedAt)) {
			current.FetchedAt = candidate.FetchedAt
		}
		// Refresh the expiry from every live fetch. The proxy pool re-validates on
		// a schedule (collector interval) and each fetch extends the TTL; taking
		// only an earlier expiry left ExpiresAt frozen at the first fetch, so the
		// pool went empty every TTL window even though the upstream kept returning
		// the same proxies (audit P2-1). A candidate that failed LiveAt above is
		// already skipped, so refreshing here always extends toward a valid proxy.
		if !candidate.ExpiresAt.IsZero() {
			current.ExpiresAt = candidate.ExpiresAt
		}
		current.SuccessCount++
		current.LastSuccessAt = candidate.ValidatedAt
		if current.HealthFails > 0 {
			current.HealthFails--
		}
		// Do NOT clear EjectedUntil: a proxy inside its isolation window must stay
		// isolated even though the collector keeps fetching it. Clearing it here
		// would let a still-failing proxy back into rotation every fetch and then
		// re-isolate it, churning the selection order (audit M6). Success (through
		// ReportSuccess) is what clears the isolation window.
		if current.EjectionCount > policy.MaxEjections {
			continue
		}
		p.proxies[position] = current
	}
}

func blendLatency(previous, sample time.Duration, alpha float64) time.Duration {
	if previous <= 0 {
		return sample
	}
	return time.Duration(alpha*float64(sample) + (1-alpha)*float64(previous))
}

func (p *Pool) Remove(identity string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.removeMatchingLocked(identity)
}

func (p *Pool) removeMatchingLocked(identity string) bool {
	filtered := p.proxies[:0]
	removed := false
	for _, proxy := range p.proxies {
		if matches(proxy, identity) {
			removed = true
			continue
		}
		filtered = append(filtered, proxy)
	}
	p.truncateLocked(filtered)
	return removed
}

func (p *Pool) ReportFailure(identity string, now time.Time, policy EjectionPolicy) Outcome {
	policy = policy.normalized()

	p.mu.Lock()
	defer p.mu.Unlock()

	p.pruneLocked(now)
	outcome := OutcomeCounted
	survivors := p.proxies[:0]
	for _, proxy := range p.proxies {
		if !matches(proxy, identity) {
			survivors = append(survivors, proxy)
			continue
		}

		prevFailureAt := proxy.LastFailureAt
		proxy.FailureCount++
		proxy.RequestFailureCount++
		proxy.RequestFailureStreak++
		proxy.LastFailureAt = now

		// Forget failures older than the window before counting this one: an
		// exit that failed twice an hour ago and fails again now is not
		// "three consecutive failures".
		if !prevFailureAt.IsZero() && now.Sub(prevFailureAt) > transportFailureWindow {
			proxy.HealthFails = 1
		} else if proxy.HealthFails < policy.FailureLimit {
			proxy.HealthFails++
		}

		if proxy.HealthFails < policy.FailureLimit {
			survivors = append(survivors, proxy)
			continue
		}

		proxy.EjectionCount++
		if proxy.EjectionCount > policy.MaxEjections {
			// Permanently remove and remember the removal so Merge does not
			// resurrect a repeatedly-failing exit on the next fetch (audit M6).
			outcome = OutcomeRemoved
			p.removed[proxy.Key()] = now.Add(removalCooldown)
			continue
		}
		proxy.HealthFails = 0
		proxy.EjectedUntil = now.Add(ejectionWindow(policy, proxy.EjectionCount))
		if outcome != OutcomeRemoved {
			outcome = OutcomeEjected
		}
		survivors = append(survivors, proxy)
	}
	p.truncateLocked(survivors)
	return outcome
}

// ReportRequestFailure records a real request body/stream failure without
// treating it as proof that the exit is unreachable. The upstream may have
// closed a response after headers, so this signal affects quality ranking but
// does not increment transport health failures or eject the proxy.
func (p *Pool) ReportRequestFailure(identity string, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.pruneLocked(now)
	for i := range p.proxies {
		if !matches(p.proxies[i], identity) {
			continue
		}
		p.proxies[i].RequestFailureCount++
		p.proxies[i].RequestFailureStreak++
		p.proxies[i].LastFailureAt = now
	}
}

func ejectionWindow(policy EjectionPolicy, ejections int) time.Duration {
	window := policy.BaseDuration * time.Duration(ejections)
	if window > policy.MaxDuration {
		return policy.MaxDuration
	}
	return window
}

// ReportHTTPFailure records an application-level failure (429/5xx) observed
// through this exit. Unlike ReportFailure (transport-level), an HTTP status is
// not by itself proof the exit is dead: NVIDIA throttles and overloads are often
// key-level or global, hitting every exit at once. So the exit is only isolated
// when the pool as a whole has served a real 2xx recently — the "some exit
// works, this one consistently doesn't" condition that identifies a
// rate-limited or blocked IP (audit H8). During a key-wide 429 storm no exit
// counts, protecting the pool from being blamed and emptied by a condition that
// is not about the proxies at all. 529 is observed at pool scope but never
// counted toward exit quality or isolation: it is the target's own overload
// signal.
func (p *Pool) ReportHTTPFailure(identity string, now time.Time, status int, policy EjectionPolicy) Outcome {
	policy = policy.normalized()

	p.mu.Lock()
	defer p.mu.Unlock()

	p.pruneLocked(now)
	if status == httpStatusOverloaded {
		// 529 is NVIDIA's own overload signal. Keep it at pool scope and return
		// before touching the selected exit's failure state.
		p.lastUpstreamOverloadAt = now
		return OutcomeCounted
	}
	outcome := OutcomeCounted
	canEject := p.hasRecentRequestSuccessLocked(now)

	survivors := p.proxies[:0]
	for _, proxy := range p.proxies {
		if !matches(proxy, identity) {
			survivors = append(survivors, proxy)
			continue
		}

		prevFailureAt := proxy.LastHTTPFailureAt
		proxy.FailureCount++
		proxy.LastFailureAt = now
		proxy.LastHTTPFailureAt = now
		proxy.LastHTTPFailureStatus = status
		proxy.RequestFailureCount++
		// Observe the pattern regardless of attribution (the UI shows it), but
		// saturate at the limit so a key-wide storm cannot inflate the counter
		// without bound. Only the isolation decision is gated below.
		if proxy.HTTPFailCount < policy.HTTPFailureLimit {
			proxy.HTTPFailCount++
		}
		// Forget failures older than the window before counting this one: an
		// exit that failed 5 times two hours ago and fails again today is not
		// "six consecutive failures".
		if !prevFailureAt.IsZero() && now.Sub(prevFailureAt) > httpFailureWindow {
			proxy.HTTPFailCount = 1
		}
		if !canEject || proxy.HTTPFailCount < policy.HTTPFailureLimit {
			survivors = append(survivors, proxy)
			continue
		}

		proxy.EjectionCount++
		if proxy.EjectionCount > policy.MaxEjections {
			// Repeatedly isolated and repeatedly failing: permanently remove and
			// remember the removal so Merge does not resurrect it (audit M6).
			outcome = OutcomeRemoved
			p.removed[proxy.Key()] = now.Add(removalCooldown)
			continue
		}
		proxy.HTTPFailCount = 0
		proxy.EjectedUntil = now.Add(ejectionWindow(policy, proxy.EjectionCount))
		if outcome != OutcomeRemoved {
			outcome = OutcomeEjected
		}
		survivors = append(survivors, proxy)
	}
	p.truncateLocked(survivors)
	return outcome
}

// UpstreamOverloadStatus reports the recent pool-wide 529 signal and its
// timestamp as one lock-consistent snapshot.
func (p *Pool) UpstreamOverloadStatus(now time.Time) (bool, time.Time) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return recentUpstreamOverload(p.lastUpstreamOverloadAt, now), p.lastUpstreamOverloadAt
}

func recentUpstreamOverload(last, now time.Time) bool {
	return !last.IsZero() && !now.Before(last) && now.Sub(last) <= upstreamOverloadWindow
}

// hasRecentRequestSuccessLocked reports whether any exit served a real 2xx
// recently. Collector validations deliberately do not count: they prove
// reachability, not that the upstream accepts traffic through the exit, so they
// would open the isolation gate during a key-wide throttle (audit H8).
func (p *Pool) hasRecentRequestSuccessLocked(now time.Time) bool {
	for _, proxy := range p.proxies {
		if !proxy.LastRequestSuccessAt.IsZero() && now.Sub(proxy.LastRequestSuccessAt) <= httpEjectSuccessWindow {
			return true
		}
	}
	return false
}

func (p *Pool) ReportSuccess(identity string, now time.Time, latency time.Duration, policy EjectionPolicy) {
	policy = policy.normalized()

	p.mu.Lock()
	defer p.mu.Unlock()

	p.pruneLocked(now)
	for i := range p.proxies {
		if !matches(p.proxies[i], identity) {
			continue
		}
		p.proxies[i].SuccessCount++
		p.proxies[i].RequestSuccessCount++
		p.proxies[i].RequestFailureStreak = 0
		p.proxies[i].LastSuccessAt = now
		// A real 2xx through this exit proves it currently works: reset the
		// HTTP-failure pattern and clear any isolation (audit H8).
		p.proxies[i].HTTPFailCount = 0
		p.proxies[i].LastRequestSuccessAt = now
		if p.proxies[i].HealthFails > 0 {
			p.proxies[i].HealthFails--
		}
		if p.proxies[i].EjectionCount > 0 {
			p.proxies[i].EjectionCount--
		}
		p.proxies[i].EjectedUntil = time.Time{}
		if latency > 0 {
			p.proxies[i].LatencyEWMA = blendLatency(p.proxies[i].LatencyEWMA, latency, policy.LatencyAlpha)
			p.proxies[i].LatencySamples++
			p.proxies[i].RequestLatencyEWMA = blendLatency(p.proxies[i].RequestLatencyEWMA, latency, policy.LatencyAlpha)
			p.proxies[i].RequestLatencySamples++
		}
	}
}

// ReportRequestLatency records a completed request's network-observable
// latency without declaring the request semantically successful. Callers use
// this for time-to-first-byte so model generation length cannot distort proxy
// quality, while ReportSuccess remains the terminal success signal.
func (p *Pool) ReportRequestLatency(identity string, now time.Time, latency time.Duration, policy EjectionPolicy) {
	if latency <= 0 {
		return
	}
	policy = policy.normalized()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneLocked(now)
	for i := range p.proxies {
		if matches(p.proxies[i], identity) {
			p.proxies[i].RequestLatencyEWMA = blendLatency(p.proxies[i].RequestLatencyEWMA, latency, policy.LatencyAlpha)
			p.proxies[i].RequestLatencySamples++
		}
	}
}

func matches(proxy Proxy, identity string) bool {
	return proxy.Address == identity || proxy.Key() == identity
}

func (p *Pool) Size(now time.Time) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	count := 0
	for _, proxy := range p.proxies {
		if proxy.AvailableAt(now) {
			count++
		}
	}
	return count
}

// HasHealthy reports whether the pool still holds an available proxy with the
// given key. The Manager uses it to decide whether a cached transport bound to a
// proxy that was ejected or removed should be rebuilt against a fresher proxy
// (audit H3): when the bound exit is gone, keeping the cache would keep sending
// traffic to a dead proxy until the next connection-level failure.
func (p *Pool) HasHealthy(proxyKey string, now time.Time) bool {
	if proxyKey == "" {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, proxy := range p.proxies {
		if proxy.Key() == proxyKey && proxy.AvailableAt(now) {
			return true
		}
	}
	return false
}

func (p *Pool) LiveSize(now time.Time) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	count := 0
	for _, proxy := range p.proxies {
		if proxy.LiveAt(now) {
			count++
		}
	}
	return count
}

func (p *Pool) List(now time.Time) []Proxy {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]Proxy, 0, len(p.proxies))
	for _, proxy := range p.proxies {
		if proxy.LiveAt(now) {
			result = append(result, proxy)
		}
	}
	return result
}

func (p *Pool) Prune(now time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	before := len(p.proxies)
	p.pruneLocked(now)
	return before - len(p.proxies)
}

// Grace extends the expiry of live proxies after an upstream fetch failure so a
// short provider outage does not drain the pool while the collector is backed
// off (audit H6). Only live proxies are extended (a dead exit still falls off on
// its normal TTL path), and maxLifetime caps how long a stale-but-usable exit
// can be kept after the provider stopped refreshing it: past that cap the pool
// drains and requests degrade to retryable 503 instead of serving increasingly
// stale exits.
//
// The cap is anchored to each proxy's own ValidatedAt, not to `now`: the
// collector calls Grace every fetch cycle (audit H6), so a cap of
// `now + maxLifetime` would be recomputed from a later `now` on every call and
// never actually expire — a persistent dead-exit patch would keep the pool
// "full" of stale exits forever. Anchoring to ValidatedAt makes the retained
// exit age out exactly maxLifetime after it was last genuinely validated.
func (p *Pool) Grace(now time.Time, grace time.Duration, maxLifetime time.Duration) int {
	if grace <= 0 || maxLifetime <= 0 {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	extended := 0
	for i := range p.proxies {
		proxy := &p.proxies[i]
		if !proxy.LiveAt(now) {
			continue
		}
		// A proxy that was never actually validated has no proof it ever worked;
		// it must not be retained past its natural TTL, so skip it.
		if proxy.ValidatedAt.IsZero() {
			continue
		}
		cap := proxy.ValidatedAt.Add(maxLifetime)
		candidate := proxy.ExpiresAt.Add(grace)
		if candidate.After(cap) {
			candidate = cap
		}
		if candidate.After(proxy.ExpiresAt) {
			proxy.ExpiresAt = candidate
			extended++
		}
	}
	return extended
}

func (p *Pool) pruneLocked(now time.Time) {
	filtered := p.proxies[:0]
	for _, proxy := range p.proxies {
		if proxy.LiveAt(now) {
			filtered = append(filtered, proxy)
		}
	}
	p.truncateLocked(filtered)
	p.pruneRemovedLocked(now)
}

// pruneRemovedLocked drops expired removal-cooldown entries so the map cannot
// grow without bound.
func (p *Pool) pruneRemovedLocked(now time.Time) {
	for key, until := range p.removed {
		if !now.Before(until) {
			delete(p.removed, key)
		}
	}
}

func (p *Pool) truncateLocked(filtered []Proxy) {
	for i := len(filtered); i < len(p.proxies); i++ {
		p.proxies[i] = Proxy{}
	}
	p.proxies = filtered
}

// StickyGet returns candidates with session affinity when configured
func (p *Pool) StickyGet(sessionKey string, now time.Time, minRemainingLife time.Duration, stickyTTL time.Duration) (candidates []Proxy, panicMode bool, stickyHit bool) {
	p.stickyMu.Lock()
	binding, exists := p.stickyBindings[sessionKey]
	if exists && now.Before(binding.expiresAt) {
		// The binding is still valid: we must drop stickyMu before taking p.mu
		// (lock order stickyMu -> p.mu is the only order this code uses).
		p.stickyMu.Unlock()

		p.mu.RLock()
		defer p.mu.RUnlock()

		for _, proxy := range p.proxies {
			if proxy.Key() == binding.proxyKey && proxy.AvailableAt(now) && proxy.RemainingLife(now) >= minRemainingLife {
				allCandidates, pm := p.orderedLocked(now, minRemainingLife, false)
				if len(allCandidates) == 0 {
					return nil, false, false
				}
				result := make([]Proxy, 0, len(allCandidates))
				result = append(result, proxy)
				for _, candidate := range allCandidates {
					if candidate.Key() != proxy.Key() {
						result = append(result, candidate)
					}
				}
				p.selectionCursor.Add(1)
				return result, pm, true
			}
		}
		// The bound proxy was ejected or expired: clear the stale binding and
		// fall through to the generic selection path. stickyMu was already
		// released above, so re-lock briefly to delete the binding.
		p.stickyMu.Lock()
		delete(p.stickyBindings, sessionKey)
		p.stickyMu.Unlock()
		candidates, panicMode = p.orderedLocked(now, minRemainingLife, false)
		if len(candidates) == 0 {
			return nil, false, false
		}
		p.selectionCursor.Add(1)
		return candidates, panicMode, false
	} else if exists {
		delete(p.stickyBindings, sessionKey)
	}
	p.stickyMu.Unlock()

	candidates, panicMode = p.Candidates(now, minRemainingLife)
	return candidates, panicMode, false
}

// StickyRebind binds a session to a proxy after successful selection
func (p *Pool) StickyRebind(sessionKey string, proxyKey string, now time.Time, stickyTTL time.Duration) {
	p.stickyMu.Lock()
	defer p.stickyMu.Unlock()
	p.stickyBindings[sessionKey] = stickyEntry{
		proxyKey:  proxyKey,
		expiresAt: now.Add(stickyTTL),
	}
}
