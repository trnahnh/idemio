package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trnahnh/idemio/internal/api"
	"github.com/trnahnh/idemio/internal/config"
	"github.com/trnahnh/idemio/internal/downstream"
	"github.com/trnahnh/idemio/internal/resource"
	"github.com/trnahnh/idemio/internal/store"
	"github.com/trnahnh/idemio/internal/telemetry"
)

const (
	minPartitionHeadroom = 14 * 24 * time.Hour
	drainMargin          = 2 * time.Second
)

func main() {
	if err := run(); err != nil {
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

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()

	if err := store.Migrate(ctx, pool); err != nil {
		return err
	}
	if err := store.VerifyUniqueConstraint(ctx, pool); err != nil {
		return err
	}
	if err := store.VerifyPartitionHeadroom(ctx, pool, minPartitionHeadroom); err != nil {
		return err
	}

	server := &http.Server{
		Handler: api.New(cfg, pool,
			downstream.New(cfg.DownstreamBaseURL, cfg.DownstreamConnectTimeout, cfg.DownstreamTimeout),
			telemetry.New(pool, cfg.ResultInlineBytes, logger),
			logger,
		).Routes(),
	}

	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("bind %s: %w", cfg.ListenAddr, err)
	}

	fmt.Printf("listen=%s\n", listener.Addr().String())
	os.Stdout.Sync()
	logger.Info("idemio listening", "addr", listener.Addr().String())

	return serveUntilSignal(ctx, server, listener, cfg, logger)
}

func serveUntilSignal(ctx context.Context, server *http.Server, listener net.Listener,
	cfg config.Config, logger *slog.Logger) error {

	failed := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- fmt.Errorf("serve: %w", err)
			return
		}
		failed <- nil
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-failed:
		return err
	case received := <-signals:
		grace := cfg.DownstreamConnectTimeout + cfg.DownstreamTimeout + drainMargin
		logger.Info("draining", "signal", received.String(), "grace", grace.String())

		shutdownCtx, cancel := context.WithTimeout(ctx, grace)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("drain incomplete; in-flight writes may be left pending", "error", err)
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}
