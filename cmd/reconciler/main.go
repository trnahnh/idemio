package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trnahnh/idemio/internal/config"
	"github.com/trnahnh/idemio/internal/probe"
	"github.com/trnahnh/idemio/internal/reconcile"
	"github.com/trnahnh/idemio/internal/store"
)

// This binary links internal/probe and never internal/downstream. The import graph is the
// enforcement of "the reconciler has no downstream write path", and a test asserts it.
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

	prober := probe.New(cfg.DownstreamBaseURL, cfg.DownstreamTimeout)
	reconciler := reconcile.New(pool, prober, cfg.ReconcileStaleAfter, logger)

	logger.Info("reconciler started",
		"interval", cfg.ReconcileInterval.String(),
		"stale_after", cfg.ReconcileStaleAfter.String())

	return reconciler.Run(ctx, cfg.ReconcileInterval)
}
