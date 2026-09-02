package archive_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trnahnh/idemio/internal/archive"
	"github.com/trnahnh/idemio/internal/maintenance"
	"github.com/trnahnh/idemio/internal/testdb"
)

const endpointEnv = "IDEMIO_TEST_ARCHIVE_ENDPOINT"

func options(t *testing.T) archive.Options {
	t.Helper()

	endpoint := strings.TrimSpace(os.Getenv(endpointEnv))
	if endpoint == "" {
		t.Fatalf("%s is not set; start the stack with 'docker compose up -d' and export it. "+
			"The restore drill fails rather than skips: an archive nobody has read back is "+
			"deletion with extra steps.", endpointEnv)
	}

	return archive.Options{
		Endpoint:  endpoint,
		Bucket:    fmt.Sprintf("idemio-test-%d", time.Now().UnixNano()),
		AccessKey: envOr("IDEMIO_TEST_ARCHIVE_ACCESS_KEY", "idemio"),
		SecretKey: envOr("IDEMIO_TEST_ARCHIVE_SECRET_KEY", "idemio-secret"),
	}
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

const oldPartition = "write_intents_20200101"

func seedOldPartition(t *testing.T, pool *pgxpool.Pool, rows int) {
	t.Helper()
	ctx := context.Background()

	_, err := pool.Exec(ctx, "CREATE TABLE "+oldPartition+
		" PARTITION OF write_intents FOR VALUES FROM ('2020-01-01') TO ('2020-01-08')")
	if err != nil {
		t.Fatalf("create partition: %v", err)
	}

	const insert = `
		INSERT INTO write_intents
		    (agent_id, idempotency_key, resource_type, resource_id, operation, operation_class,
		     scope_selector, payload, emitted_at)
		VALUES ($1, gen_random_uuid(), 'invoice', $2, 'update_status', 'mutate',
		        ARRAY['status'], $3::jsonb, $4)`

	for i := range rows {
		_, err := pool.Exec(ctx, insert,
			fmt.Sprintf("agent-%d", i),
			fmt.Sprintf("inv_%d", i),
			fmt.Sprintf(`{"amount_cents":%d,"note":"row %d"}`, 100+i, i),
			time.Date(2020, 1, 2, 3, 4, i, 0, time.UTC))
		if err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}
}

// ROADMAP Phase 1 exit criterion 6. The drill ends in a query against restored rows, not in
// a successful upload.
func TestAnExpiredPartitionSurvivesArchiveAndRestore(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	const rows = 25
	seedOldPartition(t, pool, rows)

	archiver, err := archive.New(ctx, pool, options(t))
	if err != nil {
		t.Fatalf("connect archive: %v", err)
	}

	retention := maintenance.Retention{
		Keys:          90 * 24 * time.Hour,
		Intents:       90 * 24 * time.Hour,
		Conflicts:     365 * 24 * time.Hour,
		Audit:         365 * 24 * time.Hour,
		RowsPerSecond: 1000,
	}

	retired, err := retention.RetireExpired(ctx, pool, archiver,
		slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if len(retired) != 1 || !retired[0].Archived {
		t.Fatalf("retired = %+v, want one archived partition", retired)
	}

	var live int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM write_intents").Scan(&live); err != nil {
		t.Fatalf("count live intents: %v", err)
	}
	if live != 0 {
		t.Fatalf("live intents = %d, want 0: the partition was not dropped", live)
	}

	restored, err := archiver.Restore(ctx, "write_intents", oldPartition, "restored_intents")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored != rows {
		t.Fatalf("restored %d rows, want %d", restored, rows)
	}

	var count int
	var agent, operation, class string
	var scope []string
	var payload string
	var emitted time.Time
	err = pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM restored_intents),
		       agent_id, operation, operation_class::text, scope_selector, payload::text, emitted_at
		  FROM restored_intents WHERE resource_id = 'inv_7'`).
		Scan(&count, &agent, &operation, &class, &scope, &payload, &emitted)
	if err != nil {
		t.Fatalf("query restored partition: %v", err)
	}

	if count != rows {
		t.Errorf("restored table holds %d rows, want %d", count, rows)
	}
	if agent != "agent-7" || operation != "update_status" || class != "mutate" {
		t.Errorf("restored row = %s/%s/%s, want agent-7/update_status/mutate", agent, operation, class)
	}
	if len(scope) != 1 || scope[0] != "status" {
		t.Errorf("restored scope = %v, want [status]", scope)
	}
	if !strings.Contains(payload, `"note": "row 7"`) && !strings.Contains(payload, `"note":"row 7"`) {
		t.Errorf("restored payload = %s, want the original body", payload)
	}
	if !emitted.Equal(time.Date(2020, 1, 2, 3, 4, 7, 0, time.UTC)) {
		t.Errorf("restored emitted_at = %s, want the original timestamp", emitted)
	}
}

func TestAnUnconfiguredArchiveIsNotAnError(t *testing.T) {
	pool := testdb.New(t)

	archiver, err := archive.New(context.Background(), pool, archive.Options{})
	if err != nil {
		t.Fatalf("new archive: %v", err)
	}
	if archiver != nil {
		t.Fatal("an archive was built with no endpoint; retention would drop into nothing")
	}
}
