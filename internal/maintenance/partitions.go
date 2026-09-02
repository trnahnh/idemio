package maintenance

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ADR-0016 replaces pg_partman with this, so the deployed database stays stock and
// partition maintenance is exercised by the same suite as everything else.
type Partitioned struct {
	Name   string
	Layout string
	Days   int
	Months int
}

func (p Partitioned) next(from time.Time) time.Time {
	return from.UTC().AddDate(0, p.Months, p.Days)
}

func (p Partitioned) partitionName(lower time.Time) string {
	return p.Name + "_" + lower.UTC().Format(p.Layout)
}

var Tables = []Partitioned{
	{Name: "write_intents", Layout: "20060102", Days: 7},
	{Name: "conflicts", Layout: "20060102", Days: 7},
	{Name: "payload_access_audit", Layout: "200601", Months: 1},
}

const maintenanceLockKey int64 = 5470921883114002

const upperBound = `
	SELECT coalesce(
	    max((regexp_match(pg_get_expr(c.relpartbound, c.oid), 'TO \(''([^'']+)''\)'))[1]::timestamptz),
	    date_trunc('week', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC')
	  FROM pg_class c
	  JOIN pg_inherits i ON i.inhrelid = c.oid
	 WHERE i.inhparent = $1::regclass`

// Replicas race to create the same partition. The lock makes the loser wait rather than
// fail, which keeps a maintenance error meaningful.
func EnsurePartitions(ctx context.Context, pool *pgxpool.Pool, ahead time.Duration,
	created func(table string)) error {

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin partition maintenance: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", maintenanceLockKey); err != nil {
		return fmt.Errorf("acquire maintenance lock: %w", err)
	}

	horizon := time.Now().Add(ahead)
	for _, table := range Tables {
		if err := extend(ctx, tx, table, horizon, created); err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit partition maintenance: %w", err)
	}
	return nil
}

func extend(ctx context.Context, tx pgx.Tx, table Partitioned, horizon time.Time,
	created func(string)) error {

	var boundary time.Time
	if err := tx.QueryRow(ctx, upperBound, table.Name).Scan(&boundary); err != nil {
		return fmt.Errorf("read %s partition bounds: %w", table.Name, err)
	}

	// Postgres returns the bound in the session timezone. Adding a month to a local-time
	// boundary lands on a different UTC calendar day, which both misaligns the range and
	// collides the generated name with the previous month's.
	boundary = boundary.UTC()

	for boundary.Before(horizon) {
		upper := table.next(boundary)
		stmt := fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')",
			table.partitionName(boundary), table.Name,
			boundary.UTC().Format(time.RFC3339), upper.UTC().Format(time.RFC3339))

		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("create %s partition from %s: %w",
				table.Name, boundary.UTC().Format(time.RFC3339), err)
		}
		boundary = upper
		created(table.Name)
	}
	return nil
}
