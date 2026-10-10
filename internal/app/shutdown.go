package app

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const defaultShutdownGrace = 60 * time.Second

func (a *App) beginShutdown(grace time.Duration) {
	a.shutdownOnce.Do(func() {
		a.shutting.Store(true)
		if grace <= 0 {
			grace = a.resolveShutdownGrace()
		}
		if grace <= 0 {
			grace = defaultShutdownGrace
		}
		a.shutdownGrace = grace
		a.shutdownDeadline = time.Now().Add(grace)
		if a.Pool != nil {
			a.Pool.Shutdown()
		}
		if a.proxySettings != nil {
			a.proxySettings.Close()
		} else if a.proxy != nil {
			a.proxy.Close()
		}
		if a.cleanupCancel != nil {
			a.cleanupCancel()
		}
		if a.healthCancel != nil {
			// Stop the health checker so it isn't probing NVIDIA against a
			// closing DB; in-flight probe write attempts will get cancelled.
			a.healthCancel()
		}
		if a.Server != nil {
			a.Server.setShutdownGrace(grace)
			a.Server.setShutdownDeadline(a.shutdownDeadline)
		}
		if a.rootCancel != nil {
			a.shutdownTimer = time.AfterFunc(grace, a.rootCancel)
		}
	})
}

func (a *App) resolveShutdownGrace() time.Duration {
	if a.RuntimeSettings == nil {
		return defaultShutdownGrace
	}
	return time.Duration(a.RuntimeSettings.Snapshot().ShutdownGraceMS) * time.Millisecond
}

func (a *App) finishShutdown() error {
	start := time.Now()
	logger := a.getLogger()
	logger.Info("shutdown_started")

	var shutdownErr error
	if a.Server != nil {
		componentStart := time.Now()
		shutdownErr = a.Server.Shutdown(context.Background())
		logger.Info("shutdown_component",
			"component", "http_server",
			"duration_ms", time.Since(componentStart).Milliseconds(),
			"error", shutdownErr != nil,
		)
	}

	if a.recorderCancel != nil {
		// Stop recording only after HTTP has drained; handlers may enqueue their
		// final request log while they are unwinding during Server.Shutdown.
		componentStart := time.Now()
		a.recorderCancel()
		if a.recorderDone != nil {
			<-a.recorderDone
		}
		logger.Info("shutdown_component",
			"component", "observability_recorder",
			"duration_ms", time.Since(componentStart).Milliseconds(),
		)
	}

	if a.shutdownTimer != nil {
		a.shutdownTimer.Stop()
	}
	if a.rootCancel != nil {
		a.rootCancel()
	}

	if a.modelHealthDone != nil {
		componentStart := time.Now()
		<-a.modelHealthDone
		logger.Info("shutdown_component",
			"component", "model_health",
			"duration_ms", time.Since(componentStart).Milliseconds(),
		)
	}

	if a.nvidiaClient != nil {
		componentStart := time.Now()
		a.nvidiaClient.Close()
		logger.Info("shutdown_component",
			"component", "nvidia_client",
			"duration_ms", time.Since(componentStart).Milliseconds(),
		)
	}

	if a.cleanupDone != nil {
		componentStart := time.Now()
		<-a.cleanupDone
		logger.Info("shutdown_component",
			"component", "cleanup_worker",
			"duration_ms", time.Since(componentStart).Milliseconds(),
		)
	}

	if a.healthDone != nil {
		componentStart := time.Now()
		<-a.healthDone
		logger.Info("shutdown_component",
			"component", "health_checker",
			"duration_ms", time.Since(componentStart).Milliseconds(),
		)
	}

	if a.ocfSyncDone != nil {
		componentStart := time.Now()
		<-a.ocfSyncDone
		logger.Info("shutdown_component",
			"component", "opencodefree_sync",
			"duration_ms", time.Since(componentStart).Milliseconds(),
		)
	}

	if a.capabilityProbeDone != nil {
		componentStart := time.Now()
		<-a.capabilityProbeDone
		logger.Info("shutdown_component",
			"component", "capability_probe",
			"duration_ms", time.Since(componentStart).Milliseconds(),
		)
	}

	// Close the reader pool before the writer: readers hold WAL read locks that
	// would otherwise make the writer's final checkpoint contend.
	if a.dbReader != nil {
		componentStart := time.Now()
		err := a.dbReader.Close()
		shutdownErr = errors.Join(shutdownErr, err)
		logger.Info("shutdown_component",
			"component", "database_reader",
			"duration_ms", time.Since(componentStart).Milliseconds(),
			"error", err != nil,
		)
	}

	if a.db != nil {
		componentStart := time.Now()
		err := a.db.Close()
		shutdownErr = errors.Join(shutdownErr, err)
		logger.Info("shutdown_component",
			"component", "database_writer",
			"duration_ms", time.Since(componentStart).Milliseconds(),
			"error", err != nil,
		)
	}

	logger.Info("shutdown_completed",
		"total_duration_ms", time.Since(start).Milliseconds(),
		"error", shutdownErr != nil,
	)

	return shutdownErr
}

func (a *App) getLogger() *slog.Logger {
	if a.Dependencies.Logger != nil {
		return a.Dependencies.Logger
	}
	return slog.Default()
}
