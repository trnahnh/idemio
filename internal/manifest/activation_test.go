package manifest_test

import (
	"context"
	"testing"

	"github.com/trnahnh/idemio/internal/fixtures"
	"github.com/trnahnh/idemio/internal/manifest"
	"github.com/trnahnh/idemio/internal/testdb"
)

// ROADMAP Phase 1 exit criterion 4: the change has to appear in the audit log, not only
// take effect. Git owns what the manifest said; this owns where and when it was live.
func TestAnActivationIsRecordedOncePerProcessAndVersion(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	snapshot := fixtures.ManifestStore(t).Current()
	principal := manifest.Principal()

	for range 3 {
		if err := manifest.RecordActivation(ctx, pool, snapshot, principal); err != nil {
			t.Fatalf("record activation: %v", err)
		}
	}

	var rows int
	var version, recorded string
	var types []string
	err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM manifest_activations),
		       manifest_version, principal, resource_types
		  FROM manifest_activations`).Scan(&rows, &version, &recorded, &types)
	if err != nil {
		t.Fatalf("read activations: %v", err)
	}

	if rows != 1 {
		t.Fatalf("activation rows = %d, want 1: a restart loop would flood the audit log", rows)
	}
	if version != snapshot.Version() || recorded != principal {
		t.Errorf("activation = %s by %s, want %s by %s", version, recorded, snapshot.Version(), principal)
	}
	if len(types) != 2 {
		t.Errorf("resource_types = %v, want both declared types", types)
	}
}

func TestADifferentVersionIsANewActivation(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	principal := manifest.Principal()
	original := fixtures.ManifestStore(t).Current()
	changed := fixtures.ManifestStore(t, fixtures.Window(9000)).Current()

	if original.Version() == changed.Version() {
		t.Fatal("patching the manifest did not change its version")
	}

	for _, snapshot := range []*manifest.Snapshot{original, changed} {
		if err := manifest.RecordActivation(ctx, pool, snapshot, principal); err != nil {
			t.Fatalf("record activation: %v", err)
		}
	}

	var rows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM manifest_activations").Scan(&rows); err != nil {
		t.Fatalf("count activations: %v", err)
	}
	if rows != 2 {
		t.Fatalf("activation rows = %d, want 2: a manifest change left no trace", rows)
	}
}
