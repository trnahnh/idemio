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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trnahnh/idemio/internal/config"
	"github.com/trnahnh/idemio/internal/probe"
	"github.com/trnahnh/idemio/internal/reconcile"
	"github.com/trnahnh/idemio/internal/resource"
	"github.com/trnahnh/idemio/internal/store"
	"github.com/trnahnh/idemio/internal/telemetry"
)

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
	if err := resource.Validate(); err != nil {
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
	go serveMetrics(cfg.MetricsAddr, metrics, logger)

	prober := probe.New(cfg.DownstreamBaseURL, cfg.DownstreamTimeout)
	reconciler := reconcile.New(pool, prober, cfg.ReconcileStaleAfter, metrics, logger)

	logger.Info("reconciler started",
		"interval", cfg.ReconcileInterval.String(),
		"stale_after", cfg.ReconcileStaleAfter.String())

	return reconciler.Run(ctx, cfg.ReconcileInterval)
}

func serveMetrics(addr string, metrics *telemetry.Metrics, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics.Handler())

	if err := http.ListenAndServe(addr, mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("metrics endpoint stopped", "addr", addr, "error", err)
	}
}
