package archive

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/parquet-go/parquet-go"
)

type Archive struct {
	pool   *pgxpool.Pool
	client *minio.Client
	bucket string
}

type Options struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	UseSSL    bool
}

func (o Options) configured() bool {
	return o.Endpoint != "" && o.Bucket != ""
}

func New(ctx context.Context, pool *pgxpool.Pool, opts Options) (*Archive, error) {
	if !opts.configured() {
		return nil, nil
	}

	client, err := minio.New(opts.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(opts.AccessKey, opts.SecretKey, ""),
		Secure: opts.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("connect object storage: %w", err)
	}

	exists, err := client.BucketExists(ctx, opts.Bucket)
	if err != nil {
		return nil, fmt.Errorf("check bucket %s: %w", opts.Bucket, err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, opts.Bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, fmt.Errorf("create bucket %s: %w", opts.Bucket, err)
		}
	}
	return &Archive{pool: pool, client: client, bucket: opts.Bucket}, nil
}

func objectName(table, partition string) string {
	return fmt.Sprintf("%s/%s.parquet", table, partition)
}

func (a *Archive) Archive(ctx context.Context, table, partition string) error {
	body, err := a.export(ctx, table, partition)
	if err != nil {
		return err
	}

	name := objectName(table, partition)
	_, err = a.client.PutObject(ctx, a.bucket, name, bytes.NewReader(body), int64(len(body)),
		minio.PutObjectOptions{ContentType: "application/vnd.apache.parquet"})
	if err != nil {
		return fmt.Errorf("upload %s: %w", name, err)
	}
	return nil
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
	object, err := a.client.GetObject(ctx, a.bucket, objectName(table, partition), minio.GetObjectOptions{})
	if err != nil {
		return 0, fmt.Errorf("fetch archive for %s: %w", partition, err)
	}
	defer object.Close()

	var buffer bytes.Buffer
	if _, err := buffer.ReadFrom(object); err != nil {
		return 0, fmt.Errorf("read archive for %s: %w", partition, err)
	}

	create := fmt.Sprintf("CREATE TABLE %s (LIKE %s)", into, table)
	if _, err := a.pool.Exec(ctx, create); err != nil {
		return 0, fmt.Errorf("create %s: %w", into, err)
	}

	switch table {
	case "write_intents":
		return restoreRows(ctx, a.pool, buffer.Bytes(), into, insertIntent, intentValues)
	case "conflicts":
		return restoreRows(ctx, a.pool, buffer.Bytes(), into, insertConflict, conflictValues)
	case "payload_access_audit":
		return restoreRows(ctx, a.pool, buffer.Bytes(), into, insertAudit, auditValues)
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
