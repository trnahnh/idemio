package maintenance_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trnahnh/idemio/internal/maintenance"
	"github.com/trnahnh/idemio/internal/store"
	"github.com/trnahnh/idemio/internal/testdb"
)

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func partitionCount(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()

	var count int
	err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM pg_inherits i WHERE i.inhparent = $1::regclass`, table).Scan(&count)
	if err != nil {
		t.Fatalf("count %s partitions: %v", table, err)
	}
	return count
}

func ensure(t *testing.T, pool *pgxpool.Pool, ahead time.Duration) int {
	t.Helper()

	var created int
	err := maintenance.EnsurePartitions(context.Background(), pool, ahead, func(string) { created++ })
	if err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	return created
}

// ADR-0016: this is what pg_partman was there to do, and the reason for replacing it was to
// make exactly this testable against the database that actually runs.
func TestPartitionsAreCreatedAheadOfNeed(t *testing.T) {
	pool := testdb.New(t)

	// Beyond what migration 0001 lays down, so the maintainer has real work to do rather
	// than finding the horizon already covered.
	const ahead = 20 * 7 * 24 * time.Hour

	if created := ensure(t, pool, ahead); created == 0 {
		t.Fatal("no partitions were created for a horizon past the initial migration")
	}

	for _, table := range maintenance.Tables {
		headroom, err := store.PartitionHeadroom(context.Background(), pool, table.Name)
		if err != nil {
			t.Fatalf("headroom for %s: %v", table.Name, err)
		}
		if headroom < ahead {
			t.Errorf("%s has %s of headroom, want at least %s",
				table.Name, headroom.Round(time.Hour), ahead)
		}
	}
}

// The configured default is already satisfied by migration 0001, which is the state a
// freshly deployed system is in and the one the maintainer must not churn.
func TestTheDefaultHorizonNeedsNoWorkOnAFreshSchema(t *testing.T) {
	pool := testdb.New(t)

	if created := ensure(t, pool, 8*7*24*time.Hour); created != 0 {
		t.Errorf("the maintainer created %d partitions on a fresh schema", created)
	}
}

func TestEnsuringPartitionsTwiceCreatesNothingNew(t *testing.T) {
	pool := testdb.New(t)
	const ahead = 20 * 7 * 24 * time.Hour

	ensure(t, pool, ahead)
	before := partitionCount(t, pool, "write_intents")

	if created := ensure(t, pool, ahead); created != 0 {
		t.Errorf("a second run created %d partitions; replicas race on this every hour", created)
	}
	if after := partitionCount(t, pool, "write_intents"); after != before {
		t.Errorf("partition count moved from %d to %d", before, after)
	}
}

func TestConcurrentMaintenanceDoesNotFail(t *testing.T) {
	pool := testdb.New(t)

	errs := make([]error, 4)
	done := make(chan int, 4)
	for i := range errs {
		go func() {
			errs[i] = maintenance.EnsurePartitions(context.Background(), pool,
				20*7*24*time.Hour, func(string) {})
			done <- i
		}()
	}
	for range errs {
		<-done
	}

	for i, err := range errs {
		if err != nil {
			t.Errorf("replica %d: %v", i, err)
		}
	}
}

func retention() maintenance.Retention {
	return maintenance.Retention{
		Keys:          90 * 24 * time.Hour,
		Intents:       90 * 24 * time.Hour,
		Conflicts:     365 * 24 * time.Hour,
		Audit:         365 * 24 * time.Hour,
		RowsPerSecond: 100000,
	}
}

func createOldPartition(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()

	const name = "write_intents_20200101"
	_, err := pool.Exec(context.Background(),
		"CREATE TABLE "+name+" PARTITION OF write_intents FOR VALUES FROM ('2020-01-01') TO ('2020-01-08')")
	if err != nil {
		t.Fatalf("create old partition: %v", err)
	}
	return name
}

type recordingArchiver struct{ archived []string }

func (a *recordingArchiver) Archive(_ context.Context, _, partition string) error {
	a.archived = append(a.archived, partition)
	return nil
}

func TestExpiredPartitionsAreArchivedThenDropped(t *testing.T) {
	pool := testdb.New(t)
	name := createOldPartition(t, pool)

	archiver := &recordingArchiver{}
	retired, err := retention().RetireExpired(context.Background(), pool, archiver, quiet())
	if err != nil {
		t.Fatalf("retire: %v", err)
	}

	if len(archiver.archived) != 1 || archiver.archived[0] != name {
		t.Fatalf("archived %v, want [%s]", archiver.archived, name)
	}
	if len(retired) != 1 || !retired[0].Archived {
		t.Fatalf("retired = %+v, want one archived partition", retired)
	}

	var exists bool
	if err := pool.QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1)", name).Scan(&exists); err != nil {
		t.Fatalf("look up partition: %v", err)
	}
	if exists {
		t.Error("the archived partition was not dropped")
	}
}

type failingArchiver struct{}

func (failingArchiver) Archive(context.Context, string, string) error {
	return context.DeadlineExceeded
}

// Cold data never read back is not archived, it is deleted with extra steps (ADR-0009). A
// partition whose export failed must survive.
func TestAPartitionWhoseArchiveFailsIsNotDropped(t *testing.T) {
	pool := testdb.New(t)
	name := createOldPartition(t, pool)

	retired, err := retention().RetireExpired(context.Background(), pool, failingArchiver{}, quiet())
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if len(retired) != 1 || retired[0].Archived {
		t.Fatalf("retired = %+v, want one unarchived partition", retired)
	}

	var exists bool
	if err := pool.QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1)", name).Scan(&exists); err != nil {
		t.Fatalf("look up partition: %v", err)
	}
	if !exists {
		t.Fatal("a partition was dropped without a successful archive")
	}
}

func TestNothingIsDroppedWithoutAnArchiveConfigured(t *testing.T) {
	pool := testdb.New(t)
	name := createOldPartition(t, pool)

	if _, err := retention().RetireExpired(context.Background(), pool, nil, quiet()); err != nil {
		t.Fatalf("retire: %v", err)
	}

	var attached bool
	if err := pool.QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE c.relname = $1)",
		name).Scan(&attached); err != nil {
		t.Fatalf("look up partition: %v", err)
	}
	if !attached {
		t.Fatal("a partition was detached with no archive to put it in")
	}
}

func TestCurrentPartitionsAreLeftAlone(t *testing.T) {
	pool := testdb.New(t)
	before := partitionCount(t, pool, "write_intents")

	archiver := &recordingArchiver{}
	if _, err := retention().RetireExpired(context.Background(), pool, archiver, quiet()); err != nil {
		t.Fatalf("retire: %v", err)
	}

	if len(archiver.archived) != 0 {
		t.Fatalf("archived %v, want nothing: none of these partitions is past retention",
			archiver.archived)
	}
	if after := partitionCount(t, pool, "write_intents"); after != before {
		t.Errorf("partition count moved from %d to %d", before, after)
	}
}

func TestExpiredKeysAreDeletedAndCurrentOnesSurvive(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	const insert = `
		INSERT INTO idempotency_keys
		    (agent_id, idempotency_key, request_hash, resource_type, resource_id, operation,
		     status, completed_at, created_at)
		VALUES ($1, gen_random_uuid(), 'sha256-jcs-v1:x', 'invoice', 'inv_1', 'create_charge',
		        'done', now(), now() - make_interval(days => $2))`

	for range 3 {
		if _, err := pool.Exec(ctx, insert, "agent-old", 200); err != nil {
			t.Fatalf("insert expired key: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, insert, "agent-new", 1); err != nil {
		t.Fatalf("insert current key: %v", err)
	}

	deleted, err := retention().ExpireKeys(ctx, pool, nil, 10*time.Second)
	if err != nil {
		t.Fatalf("expire keys: %v", err)
	}
	if deleted != 3 {
		t.Fatalf("deleted %d keys, want 3", deleted)
	}

	var remaining int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM idempotency_keys").Scan(&remaining); err != nil {
		t.Fatalf("count keys: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("remaining keys = %d, want 1", remaining)
	}
}

type recordingDeleter struct{ deleted []string }

func (d *recordingDeleter) Delete(_ context.Context, refs []string) error {
	d.deleted = append(d.deleted, refs...)
	return nil
}

// Offloaded result bodies are Confidential. If the sweep removed the row and left the
// object, the data would outlive the retention policy that governs it.
func TestExpiringAKeyAlsoDeletesItsOffloadedResult(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	const insert = `
		INSERT INTO idempotency_keys
		    (agent_id, idempotency_key, request_hash, resource_type, resource_id, operation,
		     status, completed_at, created_at, result_ref)
		VALUES ($1, gen_random_uuid(), 'sha256-jcs-v1:x', 'invoice', 'inv_1', 'create_charge',
		        'done', now(), now() - make_interval(days => 200), $2)`

	for i := range 3 {
		if _, err := pool.Exec(ctx, insert, "agent-old", fmt.Sprintf("results/%d.json", i)); err != nil {
			t.Fatalf("insert expired key: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, insert, "agent-inline", nil); err != nil {
		t.Fatalf("insert inline key: %v", err)
	}

	deleter := &recordingDeleter{}
	deleted, err := retention().ExpireKeys(ctx, pool, deleter, 10*time.Second)
	if err != nil {
		t.Fatalf("expire keys: %v", err)
	}
	if deleted != 4 {
		t.Fatalf("deleted %d rows, want 4", deleted)
	}
	if len(deleter.deleted) != 3 {
		t.Fatalf("deleted %d objects, want 3: %v", len(deleter.deleted), deleter.deleted)
	}
}

func TestASweepWithNoResultStoreStillExpiresRows(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx, `
		INSERT INTO idempotency_keys
		    (agent_id, idempotency_key, request_hash, resource_type, resource_id, operation,
		     status, completed_at, created_at)
		VALUES ('agent-old', gen_random_uuid(), 'sha256-jcs-v1:x', 'invoice', 'inv_1',
		        'create_charge', 'done', now(), now() - make_interval(days => 200))`)
	if err != nil {
		t.Fatalf("insert expired key: %v", err)
	}

	deleted, err := retention().ExpireKeys(ctx, pool, nil, 10*time.Second)
	if err != nil {
		t.Fatalf("expire keys: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted %d rows, want 1", deleted)
	}
}
