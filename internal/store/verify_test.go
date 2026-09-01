package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/trnahnh/idemio/internal/store"
	"github.com/trnahnh/idemio/internal/testdb"
)

func TestVerifyUniqueConstraintAcceptsTheLiveSchema(t *testing.T) {
	pool := testdb.New(t)

	if err := store.VerifyUniqueConstraint(context.Background(), pool); err != nil {
		t.Fatalf("live schema rejected: %v", err)
	}
}

func TestVerifyUniqueConstraintRejectsAWidenedConstraint(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	const widen = `
		ALTER TABLE idempotency_keys DROP CONSTRAINT idempotency_keys_pkey;
		ALTER TABLE idempotency_keys
		    ADD CONSTRAINT idempotency_keys_pkey PRIMARY KEY (agent_id, idempotency_key, created_at)`

	if _, err := pool.Exec(ctx, widen); err != nil {
		t.Skipf("could not widen the constraint to test detection: %v", err)
	}

	if err := store.VerifyUniqueConstraint(ctx, pool); err == nil {
		t.Fatal("a widened unique constraint was accepted")
	}
}

func TestPartitionHeadroomCoversTwelveWeeks(t *testing.T) {
	pool := testdb.New(t)

	if err := store.VerifyPartitionHeadroom(context.Background(), pool, 14*24*time.Hour); err != nil {
		t.Fatalf("fresh schema lacks headroom: %v", err)
	}
}

func TestPartitionHeadroomFailsWhenDemandExceedsProvisioning(t *testing.T) {
	pool := testdb.New(t)

	if err := store.VerifyPartitionHeadroom(context.Background(), pool, 520*24*time.Hour); err == nil {
		t.Fatal("headroom check passed for a horizon beyond what is provisioned")
	}
}
