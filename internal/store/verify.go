package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const expectedKeyConstraint = "agent_id,idempotency_key"

const constraintColumns = `
	SELECT string_agg(a.attname, ',' ORDER BY k.ord)
	  FROM pg_constraint c
	  JOIN LATERAL unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord) ON true
	  JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
	 WHERE c.conrelid = 'idempotency_keys'::regclass AND c.contype IN ('p', 'u')`

const latestPartitionBound = `
	SELECT max((regexp_match(pg_get_expr(c.relpartbound, c.oid), 'TO \(''([^'']+)''\)'))[1]::timestamptz)
	  FROM pg_class c
	  JOIN pg_inherits i ON i.inhrelid = c.oid
	 WHERE i.inhparent = $1::regclass`

func VerifyUniqueConstraint(ctx context.Context, pool *pgxpool.Pool) error {
	var columns string
	if err := pool.QueryRow(ctx, constraintColumns).Scan(&columns); err != nil {
		return fmt.Errorf("read idempotency_keys constraint: %w", err)
	}
	if columns != expectedKeyConstraint {
		return fmt.Errorf("idempotency_keys unique constraint is (%s), want (%s)",
			columns, expectedKeyConstraint)
	}
	return nil
}

func PartitionHeadroom(ctx context.Context, pool *pgxpool.Pool, table string) (time.Duration, error) {
	var latest time.Time
	if err := pool.QueryRow(ctx, latestPartitionBound, table).Scan(&latest); err != nil {
		return 0, fmt.Errorf("read %s partition bounds: %w", table, err)
	}
	return time.Until(latest), nil
}

func VerifyPartitionHeadroom(ctx context.Context, pool *pgxpool.Pool, minimum time.Duration) error {
	for _, table := range []string{"write_intents", "conflicts", "payload_access_audit"} {
		headroom, err := PartitionHeadroom(ctx, pool, table)
		if err != nil {
			return err
		}
		if headroom < minimum {
			return fmt.Errorf("%s has %s of partition headroom, want at least %s",
				table, headroom.Round(time.Hour), minimum)
		}
	}
	return nil
}
