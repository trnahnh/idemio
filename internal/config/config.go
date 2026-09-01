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

	ReconcileStaleAfter time.Duration
	ReconcileInterval   time.Duration

	DownstreamBaseURL        string
	DownstreamConnectTimeout time.Duration
	DownstreamTimeout        time.Duration

	PayloadBytes      int64
	ResultInlineBytes int64

	OutcomeWriteAttempts int
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
