package maintenance

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Archiver interface {
	Archive(ctx context.Context, table, partition string) error
}

type Retention struct {
	Keys          time.Duration
	Intents       time.Duration
	Conflicts     time.Duration
	Audit         time.Duration
	RowsPerSecond int
}

func (r Retention) window(table string) time.Duration {
	switch table {
	case "write_intents":
		return r.Intents
	case "conflicts":
		return r.Conflicts
	default:
		return r.Audit
	}
}

const expireKeys = `
	WITH expired AS (
	    SELECT agent_id, idempotency_key
	      FROM idempotency_keys
	     WHERE created_at < now() - make_interval(secs => $1)
	     LIMIT $2
	)
	DELETE FROM idempotency_keys k
	 USING expired e
	 WHERE k.agent_id = e.agent_id AND k.idempotency_key = e.idempotency_key`

const batchSize = 1000

// Hash partitions cannot be dropped by age (ADR-0009), so keys expire by DELETE. The rate
// is matched to ingest rather than run as a daily burst, which is what keeps autovacuum
// from falling permanently behind.
func (r Retention) ExpireKeys(ctx context.Context, pool *pgxpool.Pool, budget time.Duration) (int64, error) {
	deadline := time.Now().Add(budget)
	interval := time.Duration(float64(batchSize) / float64(r.RowsPerSecond) * float64(time.Second))

	var deleted int64
	for time.Now().Before(deadline) {
		tag, err := pool.Exec(ctx, expireKeys, r.Keys.Seconds(), batchSize)
		if err != nil {
			return deleted, fmt.Errorf("expire keys: %w", err)
		}
		deleted += tag.RowsAffected()
		if tag.RowsAffected() < batchSize {
			return deleted, nil
		}

		select {
		case <-ctx.Done():
			return deleted, ctx.Err()
		case <-time.After(interval):
		}
	}
	return deleted, nil
}

const expiredPartitions = `
	SELECT c.relname,
	       (regexp_match(pg_get_expr(c.relpartbound, c.oid), 'TO \(''([^'']+)''\)'))[1]::timestamptz
	  FROM pg_class c
	  JOIN pg_inherits i ON i.inhrelid = c.oid
	 WHERE i.inhparent = $1::regclass`

type Retired struct {
	Table     string
	Partition string
	Archived  bool
}

// Detach, then archive, then drop. A partition whose export fails stays detached rather
// than dropped: cold data never read back is not archived, it is deleted with extra steps
// (ADR-0009).
func (r Retention) RetireExpired(ctx context.Context, pool *pgxpool.Pool, archiver Archiver,
	logger *slog.Logger) ([]Retired, error) {

	var retired []Retired

	for _, table := range Tables {
		cutoff := time.Now().Add(-r.window(table.Name))

		expired, err := r.expired(ctx, pool, table.Name, cutoff)
		if err != nil {
			return retired, err
		}

		for _, partition := range expired {
			if archiver == nil {
				logger.Warn("partition is past retention but no archive is configured; leaving it attached",
					"table", table.Name, "partition", partition)
				continue
			}

			detach := fmt.Sprintf("ALTER TABLE %s DETACH PARTITION %s", table.Name, partition)
			if _, err := pool.Exec(ctx, detach); err != nil {
				return retired, fmt.Errorf("detach %s: %w", partition, err)
			}

			if err := archiver.Archive(ctx, table.Name, partition); err != nil {
				logger.Error("archive failed; partition is detached and retained, not dropped",
					"table", table.Name, "partition", partition, "error", err)
				retired = append(retired, Retired{Table: table.Name, Partition: partition})
				continue
			}

			if _, err := pool.Exec(ctx, fmt.Sprintf("DROP TABLE %s", partition)); err != nil {
				return retired, fmt.Errorf("drop %s: %w", partition, err)
			}
			retired = append(retired, Retired{Table: table.Name, Partition: partition, Archived: true})
		}
	}
	return retired, nil
}

func (r Retention) expired(ctx context.Context, pool *pgxpool.Pool, table string,
	cutoff time.Time) ([]string, error) {

	rows, err := pool.Query(ctx, expiredPartitions, table)
	if err != nil {
		return nil, fmt.Errorf("list %s partitions: %w", table, err)
	}
	defer rows.Close()

	var expired []string
	for rows.Next() {
		var name string
		var upper time.Time
		if err := rows.Scan(&name, &upper); err != nil {
			return nil, fmt.Errorf("scan %s partition: %w", table, err)
		}
		if !upper.After(cutoff) {
			expired = append(expired, name)
		}
	}
	return expired, rows.Err()
}
