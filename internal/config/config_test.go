package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/trnahnh/idemio/internal/config"
)

func valid() config.Config {
	return config.Config{
		DatabaseURL:              "postgres://localhost/idemio",
		ListenAddr:               "127.0.0.1:8080",
		AuthMode:                 config.AuthModeTrustedHeader,
		DownstreamBaseURL:        "http://127.0.0.1:9000",
		ClaimPendingWait:         0,
		ReconcileStaleAfter:      5 * time.Minute,
		ReconcileInterval:        30 * time.Second,
		DownstreamConnectTimeout: time.Second,
		DownstreamTimeout:        10 * time.Second,
		PayloadBytes:             262144,
		ResultInlineBytes:        65536,
		OutcomeWriteAttempts:     3,
		ManifestDir:              "manifests",
		ManifestReloadInterval:   30 * time.Second,
		ConflictLockTimeout:      250 * time.Millisecond,
		ReadMaxSpan:              31 * 24 * time.Hour,
		PartitionAhead:           8 * 7 * 24 * time.Hour,
		RetentionKeys:            90 * 24 * time.Hour,
		RetentionIntents:         90 * 24 * time.Hour,
		RetentionConflicts:       365 * 24 * time.Hour,
		RetentionAudit:           365 * 24 * time.Hour,
		RetentionRowsPerSec:      500,
		KafkaTopic:               "idemio.write-intents",
		RelayInterval:            time.Second,
		RelayBatch:               500,
	}
}

// The bound on a same-agent wait is derived, not configured, so it cannot be set below the
// call budget it is waiting on (ADR-0015).
func TestSerializeWaitTracksTheDownstreamBudget(t *testing.T) {
	cfg := valid()

	want := cfg.DownstreamConnectTimeout + cfg.DownstreamTimeout
	if got := cfg.SerializeWait(); got != want {
		t.Fatalf("SerializeWait() = %s, want %s", got, want)
	}
}

func TestPartitionAheadBelowThePagingThresholdRefusesToBoot(t *testing.T) {
	cfg := valid()
	cfg.PartitionAhead = 7 * 24 * time.Hour

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a partition horizon shorter than the alert threshold was accepted")
	}
	if !strings.Contains(err.Error(), "IDEMIO_PARTITION_AHEAD") {
		t.Errorf("error does not name the offending key: %v", err)
	}
}

func TestAMissingManifestDirRefusesToBoot(t *testing.T) {
	cfg := valid()
	cfg.ManifestDir = ""

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a process with no manifest directory was accepted; it could serve no write")
	}
	if !strings.Contains(err.Error(), "IDEMIO_MANIFEST_DIR") {
		t.Errorf("error does not name the offending key: %v", err)
	}
}

func TestDocumentedDefaultsAreValid(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("documented defaults rejected: %v", err)
	}
}

func TestStaleAfterBelowCallBudgetRefusesToBoot(t *testing.T) {
	cfg := valid()
	cfg.ReconcileStaleAfter = 20 * time.Second

	err := cfg.Validate()
	if err == nil {
		t.Fatal("stale_after below 3x the call budget was accepted")
	}
	if !strings.Contains(err.Error(), "IDEMIO_RECONCILE_STALE_AFTER") {
		t.Errorf("error does not name the offending key: %v", err)
	}
}

func TestStaleAfterBelowTwiceIntervalRefusesToBoot(t *testing.T) {
	cfg := valid()
	cfg.ReconcileInterval = 4 * time.Minute

	if err := cfg.Validate(); err == nil {
		t.Fatal("stale_after below twice the sweep interval was accepted")
	}
}

func TestPendingWaitAtOrAboveTimeoutRefusesToBoot(t *testing.T) {
	cfg := valid()
	cfg.ClaimPendingWait = cfg.DownstreamTimeout

	if err := cfg.Validate(); err == nil {
		t.Fatal("pending wait at the downstream timeout was accepted")
	}
}

func TestUnsetAuthModeRefusesToBoot(t *testing.T) {
	cfg := valid()
	cfg.AuthMode = ""

	err := cfg.Validate()
	if err == nil {
		t.Fatal("empty auth mode was accepted")
	}
	if !strings.Contains(err.Error(), "IDEMIO_AUTH_MODE") {
		t.Errorf("error does not name the offending key: %v", err)
	}
}

func TestAllProblemsAreReportedTogether(t *testing.T) {
	cfg := valid()
	cfg.DatabaseURL = ""
	cfg.AuthMode = ""
	cfg.ReconcileStaleAfter = time.Second

	err := cfg.Validate()
	if err == nil {
		t.Fatal("invalid configuration was accepted")
	}
	for _, key := range []string{"IDEMIO_DATABASE_URL", "IDEMIO_AUTH_MODE", "IDEMIO_RECONCILE_STALE_AFTER"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error omits %s: %v", key, err)
		}
	}
}
