package testdb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trnahnh/idemio/internal/store"
	"github.com/trnahnh/idemio/migrations"
)

const urlEnv = "IDEMIO_TEST_DATABASE_URL"

var (
	templateOnce sync.Once
	templateName string
	templateErr  error
)

func New(t *testing.T) *pgxpool.Pool {
	t.Helper()

	pool, _ := NewWithURL(t)
	return pool
}

func NewWithURL(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()

	adminURL := requireURL(t)
	template := ensureTemplate(t, adminURL)

	name := "idemio_t_" + randomSuffix(t)
	admin := connect(t, adminURL)
	defer admin.Close()

	stmt := fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, template)
	if _, err := admin.Exec(context.Background(), stmt); err != nil {
		t.Fatalf("clone template into %s: %v", name, err)
	}

	dsn := withDatabase(t, adminURL, name)
	pool := connect(t, dsn)
	t.Cleanup(func() {
		pool.Close()
		dropper := connect(t, adminURL)
		defer dropper.Close()
		drop := fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", name)
		if _, err := dropper.Exec(context.Background(), drop); err != nil {
			t.Logf("drop %s: %v", name, err)
		}
	})
	return pool, dsn
}

func NewEmpty(t *testing.T) *pgxpool.Pool {
	t.Helper()

	adminURL := requireURL(t)
	name := "idemio_e_" + randomSuffix(t)

	admin := connect(t, adminURL)
	defer admin.Close()

	stmt := fmt.Sprintf("CREATE DATABASE %s", name)
	if _, err := admin.Exec(context.Background(), stmt); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}

	pool := connect(t, withDatabase(t, adminURL, name))
	t.Cleanup(func() {
		pool.Close()
		dropper := connect(t, adminURL)
		defer dropper.Close()
		drop := fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", name)
		if _, err := dropper.Exec(context.Background(), drop); err != nil {
			t.Logf("drop %s: %v", name, err)
		}
	})
	return pool
}

func Truncate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	const stmt = `TRUNCATE idempotency_keys, write_intents, conflicts, payload_access_audit,
		manifest_activations`
	if _, err := pool.Exec(context.Background(), stmt); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func requireURL(t *testing.T) string {
	t.Helper()

	raw := strings.TrimSpace(os.Getenv(urlEnv))
	if raw == "" {
		t.Fatalf("%s is not set; start the stack with 'docker compose up -d' and export it. "+
			"Database tests fail rather than skip: a green run must mean the guarantee was exercised.", urlEnv)
	}
	return raw
}

func ensureTemplate(t *testing.T, adminURL string) string {
	t.Helper()

	templateOnce.Do(func() {
		fingerprint, err := migrationsFingerprint()
		if err != nil {
			templateErr = err
			return
		}
		name := "idemio_template_" + fingerprint
		if err := buildTemplate(adminURL, name); err != nil {
			templateErr = err
			return
		}
		templateName = name
	})
	if templateErr != nil {
		t.Fatalf("build template: %v", templateErr)
	}
	return templateName
}

func buildTemplate(adminURL, name string) error {
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		return fmt.Errorf("connect admin: %w", err)
	}
	defer pool.Close()

	admin, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire admin connection: %w", err)
	}
	defer admin.Release()

	lockKey := int64(hashToUint32(name))
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		return fmt.Errorf("acquire template lock: %w", err)
	}
	defer func() {
		if _, err := admin.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey); err != nil {
			log.Printf("release template lock: %v", err)
		}
	}()

	var exists bool
	err = admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists)
	if err != nil {
		return fmt.Errorf("look up template: %w", err)
	}
	if exists {
		return nil
	}

	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", name)); err != nil {
		return fmt.Errorf("create template: %w", err)
	}

	target, err := replaceDatabase(adminURL, name)
	if err != nil {
		return err
	}
	targetPool, err := pgxpool.New(ctx, target)
	if err != nil {
		return fmt.Errorf("connect template: %w", err)
	}
	defer targetPool.Close()

	if err := store.Migrate(ctx, targetPool); err != nil {
		return fmt.Errorf("migrate template: %w", err)
	}
	return nil
}

func migrationsFingerprint() (string, error) {
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return "", fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(names)

	sum := sha256.New()
	for _, name := range names {
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", name, err)
		}
		sum.Write([]byte(name))
		sum.Write(body)
	}
	return hex.EncodeToString(sum.Sum(nil))[:12], nil
}

func hashToUint32(s string) uint32 {
	sum := sha256.Sum256([]byte(s))
	return uint32(sum[0])<<24 | uint32(sum[1])<<16 | uint32(sum[2])<<8 | uint32(sum[3])
}

func connect(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return pool
}

func withDatabase(t *testing.T, dsn, name string) string {
	t.Helper()

	out, err := replaceDatabase(dsn, name)
	if err != nil {
		t.Fatalf("rewrite dsn: %v", err)
	}
	return out
}

func replaceDatabase(dsn, name string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", urlEnv, err)
	}
	parsed.Path = "/" + name
	return parsed.String(), nil
}

func randomSuffix(t *testing.T) string {
	t.Helper()

	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(buf)
}

const pooledEnv = "IDEMIO_TEST_POOLED_ADDR"

// The request path must survive transaction-mode pooling; the migration path must not run
// through it at all (ADR-0013). Only the first is testable, so only the first is tested.
func Pooled(t *testing.T, direct string) *pgxpool.Pool {
	t.Helper()

	addr := strings.TrimSpace(os.Getenv(pooledEnv))
	if addr == "" {
		t.Fatalf("%s is not set; start the stack with 'docker compose up -d' and export it. "+
			"Pooled tests fail rather than skip: transaction-mode pooling is the deployment "+
			"shape, and a suite that never exercises it proves nothing about it.", pooledEnv)
	}

	parsed, err := url.Parse(direct)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	parsed.Host = addr

	pool, err := pgxpool.New(context.Background(), parsed.String())
	if err != nil {
		t.Fatalf("connect through the pooler: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
