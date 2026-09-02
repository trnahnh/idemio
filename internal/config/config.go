package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const AuthModeTrustedHeader = "trusted_header"

type Config struct {
	DatabaseURL string
	ListenAddr  string
	MetricsAddr string
	AuthMode    string

	ClaimPendingWait time.Duration

	ManifestDir            string
	ManifestReloadInterval time.Duration

	ConflictLockTimeout time.Duration

	ReadMaxSpan time.Duration

	ReconcileStaleAfter time.Duration
	ReconcileInterval   time.Duration

	DownstreamBaseURL        string
	DownstreamConnectTimeout time.Duration
	DownstreamTimeout        time.Duration

	PayloadBytes      int64
	ResultInlineBytes int64

	OutcomeWriteAttempts int

	PartitionAhead time.Duration

	RetentionKeys       time.Duration
	RetentionIntents    time.Duration
	RetentionConflicts  time.Duration
	RetentionAudit      time.Duration
	RetentionRowsPerSec int

	KafkaBrokers  string
	KafkaTopic    string
	RelayInterval time.Duration
	RelayBatch    int

	ArchiveEndpoint  string
	ArchiveBucket    string
	ArchiveAccessKey string
	ArchiveSecretKey string
}

// Same-agent serialization waits for another request's downstream call, so its bound is
// that call's budget. It is derived rather than configured: a value below the budget would
// turn every serialized write into a 503 without saying so (ADR-0015).
func (c Config) SerializeWait() time.Duration {
	return c.DownstreamConnectTimeout + c.DownstreamTimeout
}

func Load() (Config, error) {
	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	cfg := Config{
		DatabaseURL:              os.Getenv("IDEMIO_DATABASE_URL"),
		ListenAddr:               withDefault("IDEMIO_LISTEN_ADDR", "127.0.0.1:8080"),
		MetricsAddr:              withDefault("IDEMIO_METRICS_ADDR", "127.0.0.1:9090"),
		AuthMode:                 withDefault("IDEMIO_AUTH_MODE", ""),
		DownstreamBaseURL:        os.Getenv("IDEMIO_DOWNSTREAM_BASE_URL"),
		ClaimPendingWait:         duration("IDEMIO_CLAIM_PENDING_WAIT_MS", 0, fail),
		ReconcileStaleAfter:      goDuration("IDEMIO_RECONCILE_STALE_AFTER", 5*time.Minute, fail),
		ReconcileInterval:        goDuration("IDEMIO_RECONCILE_INTERVAL", 30*time.Second, fail),
		DownstreamConnectTimeout: duration("IDEMIO_DOWNSTREAM_CONNECT_TIMEOUT_MS", 1000, fail),
		DownstreamTimeout:        duration("IDEMIO_DOWNSTREAM_TIMEOUT_MS", 10000, fail),
		PayloadBytes:             integer("IDEMIO_LIMITS_PAYLOAD_BYTES", 262144, fail),
		ResultInlineBytes:        integer("IDEMIO_LIMITS_RESULT_INLINE_BYTES", 65536, fail),
		OutcomeWriteAttempts:     int(integer("IDEMIO_OUTCOME_WRITE_ATTEMPTS", 3, fail)),

		ManifestDir:            os.Getenv("IDEMIO_MANIFEST_DIR"),
		ManifestReloadInterval: goDuration("IDEMIO_MANIFEST_RELOAD_INTERVAL", 30*time.Second, fail),
		ConflictLockTimeout:    duration("IDEMIO_CONFLICT_LOCK_TIMEOUT_MS", 250, fail),
		ReadMaxSpan:            goDuration("IDEMIO_READ_MAX_SPAN", 31*24*time.Hour, fail),

		PartitionAhead: goDuration("IDEMIO_PARTITION_AHEAD", 8*7*24*time.Hour, fail),

		RetentionKeys:       goDuration("IDEMIO_RETENTION_KEYS", 90*24*time.Hour, fail),
		RetentionIntents:    goDuration("IDEMIO_RETENTION_INTENTS", 90*24*time.Hour, fail),
		RetentionConflicts:  goDuration("IDEMIO_RETENTION_CONFLICTS", 365*24*time.Hour, fail),
		RetentionAudit:      goDuration("IDEMIO_RETENTION_AUDIT", 365*24*time.Hour, fail),
		RetentionRowsPerSec: int(integer("IDEMIO_RETENTION_ROWS_PER_SEC", 500, fail)),

		KafkaBrokers:  os.Getenv("IDEMIO_KAFKA_BROKERS"),
		KafkaTopic:    withDefault("IDEMIO_KAFKA_TOPIC", "idemio.write-intents"),
		RelayInterval: goDuration("IDEMIO_RELAY_INTERVAL", time.Second, fail),
		RelayBatch:    int(integer("IDEMIO_RELAY_BATCH", 500, fail)),

		ArchiveEndpoint:  os.Getenv("IDEMIO_ARCHIVE_ENDPOINT"),
		ArchiveBucket:    os.Getenv("IDEMIO_ARCHIVE_BUCKET"),
		ArchiveAccessKey: os.Getenv("IDEMIO_ARCHIVE_ACCESS_KEY"),
		ArchiveSecretKey: os.Getenv("IDEMIO_ARCHIVE_SECRET_KEY"),
	}

	if len(problems) > 0 {
		return Config{}, errors.New("configuration: " + strings.Join(problems, "; "))
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if c.DatabaseURL == "" {
		fail("IDEMIO_DATABASE_URL is required")
	}
	if c.DownstreamBaseURL == "" {
		fail("IDEMIO_DOWNSTREAM_BASE_URL is required")
	}
	if c.AuthMode != AuthModeTrustedHeader {
		fail("IDEMIO_AUTH_MODE must be set to %q; running without verified identity is never a default",
			AuthModeTrustedHeader)
	}

	callBudget := c.DownstreamConnectTimeout + c.DownstreamTimeout
	if c.ReconcileStaleAfter < 3*callBudget {
		fail("IDEMIO_RECONCILE_STALE_AFTER (%s) must be at least 3x the downstream call budget (%s)",
			c.ReconcileStaleAfter, 3*callBudget)
	}
	if c.ReconcileStaleAfter <= 2*c.ReconcileInterval {
		fail("IDEMIO_RECONCILE_STALE_AFTER (%s) must exceed twice IDEMIO_RECONCILE_INTERVAL (%s)",
			c.ReconcileStaleAfter, 2*c.ReconcileInterval)
	}
	if c.ClaimPendingWait >= c.DownstreamTimeout {
		fail("IDEMIO_CLAIM_PENDING_WAIT_MS (%s) must be below IDEMIO_DOWNSTREAM_TIMEOUT_MS (%s)",
			c.ClaimPendingWait, c.DownstreamTimeout)
	}
	if c.OutcomeWriteAttempts < 1 {
		fail("IDEMIO_OUTCOME_WRITE_ATTEMPTS must be at least 1")
	}
	if c.PayloadBytes < 1 {
		fail("IDEMIO_LIMITS_PAYLOAD_BYTES must be positive")
	}
	if c.ManifestDir == "" {
		fail("IDEMIO_MANIFEST_DIR is required; conflict semantics and error classification " +
			"are declared, never inferred")
	}
	if c.ConflictLockTimeout <= 0 {
		fail("IDEMIO_CONFLICT_LOCK_TIMEOUT_MS must be positive; without a bound a hot " +
			"resource consumes one connection per waiter")
	}
	if c.ReadMaxSpan <= 0 {
		fail("IDEMIO_READ_MAX_SPAN must be positive")
	}
	if c.PartitionAhead < 4*7*24*time.Hour {
		fail("IDEMIO_PARTITION_AHEAD (%s) must be at least the four weeks at which "+
			"IdemioPartitionHeadroomLow pages", c.PartitionAhead)
	}
	if c.RetentionRowsPerSec < 1 {
		fail("IDEMIO_RETENTION_ROWS_PER_SEC must be positive")
	}

	if len(problems) > 0 {
		return errors.New("configuration: " + strings.Join(problems, "; "))
	}
	return nil
}

func withDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func duration(key string, fallbackMS int64, fail func(string, ...any)) time.Duration {
	return time.Duration(integer(key, fallbackMS, fail)) * time.Millisecond
}

func goDuration(key string, fallback time.Duration, fail func(string, ...any)) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		fail("%s is not a duration: %v", key, err)
		return fallback
	}
	return parsed
}

func integer(key string, fallback int64, fail func(string, ...any)) int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		fail("%s is not an integer: %v", key, err)
		return fallback
	}
	return parsed
}
