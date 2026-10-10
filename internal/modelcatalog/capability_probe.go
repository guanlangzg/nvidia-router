package modelcatalog

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// CapabilityProbeRunner periodically re-runs the detailed capability probe on
// every chat model so drift is written back automatically instead of being
// patched by hand (2026-08-25 P3). The caller supplies the enabled flag and
// interval from the runtime snapshot each cycle, so an operator toggle takes
// effect without a restart.
type CapabilityProbeRunner struct {
	service *Service
	logger  *slog.Logger
	now     func() time.Time
}

// NewCapabilityProbeRunner wires the runner to the catalog service.
func NewCapabilityProbeRunner(service *Service, logger *slog.Logger) *CapabilityProbeRunner {
	if logger == nil {
		logger = slog.Default()
	}
	return &CapabilityProbeRunner{service: service, logger: logger, now: time.Now}
}

// Start launches the blocking probe loop; cancel ctx to stop it. The first run
// waits one full interval so a restart does not immediately fan out probes.
// It closes the returned channel once the loop exits.
func (r *CapabilityProbeRunner) Start(ctx context.Context, interval time.Duration, enabled func() bool) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(interval)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			if enabled != nil && !enabled() {
				r.logger.Debug("capability probe skipped (disabled)")
				timer.Reset(interval)
				continue
			}
			runCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
			started := r.now()
			summary, err := r.runOnce(runCtx)
			cancel()
			if err != nil {
				r.logger.Warn("capability probe cycle failed", "error", err)
			} else if summary != nil {
				r.logger.Info("capability probe cycle completed",
					"models", summary.total,
					"supported", summary.supported,
					"unsupported", summary.unsupported,
					"unknown", summary.unknown,
					"duration_s", r.now().Sub(started).Round(time.Second).Seconds(),
				)
			}
			timer.Reset(interval)
		}
	}()
	return done
}

type probeCycleSummary struct {
	total       int
	supported   int
	unsupported int
	unknown     int
}

func (r *CapabilityProbeRunner) runOnce(ctx context.Context) (*probeCycleSummary, error) {
	models, err := r.service.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list models for capability probe: %w", err)
	}
	summary := &probeCycleSummary{}
	// Serial on purpose: the detailed probe already rotates NVIDIA keys, so
	// parallelism only multiplies upstream load, and serialized probes keep
	// capability write-backs ordered and predictable. Chat models are few.
	for _, model := range models {
		if ctx.Err() != nil {
			return summary, nil
		}
		if !model.Enabled || model.Kind != KindChat {
			continue
		}
		summary.total++
		probe, err := r.service.TestModelAutoDetailed(ctx, model.ID)
		if err != nil {
			// A failed probe leaves capability rows untouched; log at debug
			// so routine upstream blips do not spam the operator feed.
			r.logger.Debug("capability probe failed", "model", model.PublicID, "error", err)
			continue
		}
		switch probe.Tools {
		case "supported":
			summary.supported++
		case ProbeStatusUnsupported:
			summary.unsupported++
		default:
			summary.unknown++
		}
	}
	return summary, nil
}
