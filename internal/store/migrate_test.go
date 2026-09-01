package store_test

import (
	"context"
	"testing"

	"github.com/trnahnh/idemio/internal/store"
	"github.com/trnahnh/idemio/internal/testdb"
)

func TestMigrateIsIdempotent(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	var applied int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&applied); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if applied != 1 {
		t.Fatalf("schema_migrations rows = %d, want 1", applied)
	}
}

func TestLiveUniqueConstraintIsExactlyAgentAndKey(t *testing.T) {
	pool := testdb.New(t)

	const query = `
		SELECT string_agg(a.attname, ',' ORDER BY k.ord)
		FROM pg_constraint c
		JOIN LATERAL unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord) ON true
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
		WHERE c.conrelid = 'idempotency_keys'::regclass AND c.contype IN ('p', 'u')`

	var columns string
	if err := pool.QueryRow(context.Background(), query).Scan(&columns); err != nil {
		t.Fatalf("read constraint: %v", err)
	}
	if columns != "agent_id,idempotency_key" {
		t.Fatalf("unique constraint = (%s), want (agent_id,idempotency_key)", columns)
	}
}

func TestKeyStatusHasAllFiveValues(t *testing.T) {
	pool := testdb.New(t)

	const query = `
		SELECT string_agg(e.enumlabel, ',' ORDER BY e.enumsortorder)
		FROM pg_type t JOIN pg_enum e ON e.enumtypid = t.oid
		WHERE t.typname = 'key_status'`

	var labels string
	if err := pool.QueryRow(context.Background(), query).Scan(&labels); err != nil {
		t.Fatalf("read enum: %v", err)
	}
	if labels != "pending,done,failed,indeterminate,rejected" {
		t.Fatalf("key_status = %s", labels)
	}
}

func TestNoDefaultPartitionsExist(t *testing.T) {
	pool := testdb.New(t)

	const query = `
		SELECT count(*) FROM pg_class c
		JOIN pg_inherits i ON i.inhrelid = c.oid
		WHERE pg_get_expr(c.relpartbound, c.oid) = 'DEFAULT'`

	var defaults int
	if err := pool.QueryRow(context.Background(), query).Scan(&defaults); err != nil {
		t.Fatalf("count default partitions: %v", err)
	}
	if defaults != 0 {
		t.Fatalf("default partitions = %d, want 0 (ADR-0012)", defaults)
	}
}

func TestPartitionCounts(t *testing.T) {
	pool := testdb.New(t)

	for table, want := range map[string]int{
		"idempotency_keys":     64,
		"write_intents":        12,
		"conflicts":            12,
		"payload_access_audit": 3,
	} {
		const query = `
			SELECT count(*) FROM pg_inherits i
			WHERE i.inhparent = $1::regclass`

		var got int
		if err := pool.QueryRow(context.Background(), query, table).Scan(&got); err != nil {
			t.Fatalf("count partitions of %s: %v", table, err)
		}
		if got != want {
			t.Errorf("%s partitions = %d, want %d", table, got, want)
		}
	}
}
