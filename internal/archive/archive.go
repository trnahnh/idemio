package archive

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/parquet-go/parquet-go"

	"github.com/trnahnh/idemio/internal/objectstore"
)

type Archive struct {
	pool    *pgxpool.Pool
	objects *objectstore.Client
}

func New(pool *pgxpool.Pool, objects *objectstore.Client) *Archive {
	if objects == nil {
		return nil
	}
	return &Archive{pool: pool, objects: objects}
}

func objectName(table, partition string) string {
	return fmt.Sprintf("%s/%s.parquet", table, partition)
}

func (a *Archive) Archive(ctx context.Context, table, partition string) error {
	body, err := a.export(ctx, table, partition)
	if err != nil {
		return err
	}

	return a.objects.Put(ctx, objectName(table, partition), body,
		"application/vnd.apache.parquet")
}

func (a *Archive) export(ctx context.Context, table, partition string) ([]byte, error) {
	switch table {
	case "write_intents":
		return exportRows[intentRecord](ctx, a.pool, scanIntents, partition)
	case "conflicts":
		return exportRows[conflictRecord](ctx, a.pool, scanConflicts, partition)
	case "payload_access_audit":
		return exportRows[auditRecord](ctx, a.pool, scanAudit, partition)
	default:
		return nil, fmt.Errorf("no archive schema for %s", table)
	}
}

func exportRows[T any](ctx context.Context, pool *pgxpool.Pool,
	scan func(context.Context, *pgxpool.Pool, string) ([]T, error), partition string) ([]byte, error) {

	rows, err := scan(ctx, pool, partition)
	if err != nil {
		return nil, err
	}

	var buffer bytes.Buffer
	writer := parquet.NewGenericWriter[T](&buffer)
	if len(rows) > 0 {
		if _, err := writer.Write(rows); err != nil {
			return nil, fmt.Errorf("write parquet for %s: %w", partition, err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close parquet for %s: %w", partition, err)
	}
	return buffer.Bytes(), nil
}

// An archive nobody can read back is deletion with extra steps (ADR-0009). Restore lands
// the partition in a standalone table so the drill ends in a query, not a download.
func (a *Archive) Restore(ctx context.Context, table, partition, into string) (int, error) {
	body, err := a.objects.Get(ctx, objectName(table, partition))
	if err != nil {
		return 0, fmt.Errorf("fetch archive for %s: %w", partition, err)
	}

	create := fmt.Sprintf("CREATE TABLE %s (LIKE %s)", into, table)
	if _, err := a.pool.Exec(ctx, create); err != nil {
		return 0, fmt.Errorf("create %s: %w", into, err)
	}

	switch table {
	case "write_intents":
		return restoreRows(ctx, a.pool, body, into, insertIntent, intentValues)
	case "conflicts":
		return restoreRows(ctx, a.pool, body, into, insertConflict, conflictValues)
	case "payload_access_audit":
		return restoreRows(ctx, a.pool, body, into, insertAudit, auditValues)
	default:
		return 0, fmt.Errorf("no archive schema for %s", table)
	}
}

func restoreRows[T any](ctx context.Context, pool *pgxpool.Pool, body []byte, into, statement string,
	values func(T) []any) (int, error) {

	rows, err := parquet.Read[T](bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return 0, fmt.Errorf("read parquet: %w", err)
	}

	for _, row := range rows {
		if _, err := pool.Exec(ctx, fmt.Sprintf(statement, into), values(row)...); err != nil {
			return 0, fmt.Errorf("restore row into %s: %w", into, err)
		}
	}
	return len(rows), nil
}

func micros(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.UnixMicro()
}

func fromMicros(value int64) *time.Time {
	if value == 0 {
		return nil
	}
	t := time.UnixMicro(value).UTC()
	return &t
}
