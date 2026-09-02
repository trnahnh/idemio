package manifest

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

const recordActivation = `
	INSERT INTO manifest_activations (manifest_version, principal, resource_types)
	VALUES ($1, $2, $3)
	ON CONFLICT (principal, manifest_version) DO NOTHING`

// Git owns what a manifest said. This owns which version a process was running when it
// judged a write, which is the fact no review can reconstruct (ADR-0013).
func RecordActivation(ctx context.Context, pool *pgxpool.Pool, snapshot *Snapshot, principal string) error {
	_, err := pool.Exec(ctx, recordActivation, snapshot.Version(), principal, snapshot.Types())
	if err != nil {
		return fmt.Errorf("record manifest activation: %w", err)
	}
	return nil
}

func Principal() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return host + "/" + strconv.Itoa(os.Getpid())
}
