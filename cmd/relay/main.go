package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"

	"github.com/trnahnh/idemio/internal/config"
	"github.com/trnahnh/idemio/internal/relay"
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
	if cfg.KafkaBrokers == "" {
		return errors.New("IDEMIO_KAFKA_BROKERS is required to run the relay")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()

	writer := &kafka.Writer{
		Addr:                   kafka.TCP(strings.Split(cfg.KafkaBrokers, ",")...),
		Topic:                  cfg.KafkaTopic,
		Balancer:               &kafka.Hash{},
		RequiredAcks:           kafka.RequireAll,
		AllowAutoTopicCreation: true,
	}
	defer writer.Close()

	metrics := telemetry.New(pool, cfg, logger)
	go serveMetrics(cfg.MetricsAddr, metrics, logger)

	logger.Info("relay started",
		"brokers", cfg.KafkaBrokers, "topic", cfg.KafkaTopic,
		"interval", cfg.RelayInterval.String())

	return relay.New(pool, writer, cfg.RelayBatch, logger).Run(ctx, cfg.RelayInterval,
		func(count int) { metrics.RelayPublished.Add(float64(count)) },
		metrics.RelayFailures.Inc)
}

func serveMetrics(addr string, metrics *telemetry.Metrics, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics.Handler())

	if err := http.ListenAndServe(addr, mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("metrics endpoint stopped", "addr", addr, "error", err)
	}
}
