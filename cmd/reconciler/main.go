package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trnahnh/idemio/internal/archive"
	"github.com/trnahnh/idemio/internal/config"
	"github.com/trnahnh/idemio/internal/maintenance"
	"github.com/trnahnh/idemio/internal/manifest"
	"github.com/trnahnh/idemio/internal/probe"
	"github.com/trnahnh/idemio/internal/reconcile"
	"github.com/trnahnh/idemio/internal/store"
	"github.com/trnahnh/idemio/internal/telemetry"
)

const maintenanceInterval = time.Hour

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	manifests, err := manifest.NewStore(cfg.ManifestDir)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()

	if err := store.VerifyUniqueConstraint(ctx, pool); err != nil {
		return err
	}

	metrics := telemetry.New(pool, cfg, logger)
	metrics.ManifestInfo.WithLabelValues(manifests.Current().Version()).Set(1)
	go serveMetrics(cfg.MetricsAddr, metrics, logger)
	go manifests.Watch(ctx, cfg.ManifestReloadInterval, logger,
		func(snapshot *manifest.Snapshot) {
			metrics.ManifestInfo.Reset()
			metrics.ManifestInfo.WithLabelValues(snapshot.Version()).Set(1)
		},
		metrics.ManifestReloadFailures.Inc)

	archiver, err := archive.New(ctx, pool, archive.Options{
		Endpoint:  cfg.ArchiveEndpoint,
		Bucket:    cfg.ArchiveBucket,
		AccessKey: cfg.ArchiveAccessKey,
		SecretKey: cfg.ArchiveSecretKey,
	})
	if err != nil {
		return err
	}
	if archiver == nil {
		logger.Warn("no archive configured; partitions past retention will be left attached " +
			"rather than dropped")
	}

	go maintain(ctx, pool, cfg, archiver, metrics, logger)

	prober := probe.New(cfg.DownstreamBaseURL, cfg.DownstreamTimeout)
	reconciler := reconcile.New(pool, prober, manifests, cfg.ReconcileStaleAfter, metrics, logger)

	logger.Info("reconciler started",
		"interval", cfg.ReconcileInterval.String(),
		"stale_after", cfg.ReconcileStaleAfter.String(),
		"manifest_version", manifests.Current().Version())

	return reconciler.Run(ctx, cfg.ReconcileInterval)
}

// ADR-0016: partition creation and retention are database housekeeping on the same cadence
// as crash recovery, and belong in the same process as it.
func maintain(ctx context.Context, pool *pgxpool.Pool, cfg config.Config,
	archiver maintenance.Archiver, metrics *telemetry.Metrics, logger *slog.Logger) {

	retention := maintenance.Retention{
		Keys:          cfg.RetentionKeys,
		Intents:       cfg.RetentionIntents,
		Conflicts:     cfg.RetentionConflicts,
		Audit:         cfg.RetentionAudit,
		RowsPerSecond: cfg.RetentionRowsPerSec,
	}

	ticker := time.NewTicker(maintenanceInterval)
	defer ticker.Stop()

	for {
		runMaintenance(ctx, pool, cfg, retention, archiver, metrics, logger)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func runMaintenance(ctx context.Context, pool *pgxpool.Pool, cfg config.Config,
	retention maintenance.Retention, archiver maintenance.Archiver,
	metrics *telemetry.Metrics, logger *slog.Logger) {

	created := func(table string) { metrics.PartitionsCreated.WithLabelValues(table).Inc() }
	if err := maintenance.EnsurePartitions(ctx, pool, cfg.PartitionAhead, created); err != nil {
		logger.Error("partition maintenance failed; a missing partition is a write outage",
			"error", err)
	}

	deleted, err := retention.ExpireKeys(ctx, pool, maintenanceInterval/2)
	if err != nil {
		logger.Error("key expiry failed", "error", err)
	}
	if deleted > 0 {
		metrics.RetentionDeleted.WithLabelValues("idempotency_keys").Add(float64(deleted))
	}

	if archiver == nil {
		return
	}
	retired, err := retention.RetireExpired(ctx, pool, archiver, logger)
	if err != nil {
		logger.Error("partition retirement failed", "error", err)
	}
	for _, partition := range retired {
		if partition.Archived {
			metrics.ArchivedPartitions.WithLabelValues(partition.Table).Inc()
			metrics.RetentionDeleted.WithLabelValues(partition.Table).Inc()
		}
	}
}

func serveMetrics(addr string, metrics *telemetry.Metrics, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics.Handler())

	if err := http.ListenAndServe(addr, mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("metrics endpoint stopped", "addr", addr, "error", err)
	}
}
