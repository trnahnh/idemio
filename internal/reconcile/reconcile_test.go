package reconcile_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trnahnh/idemio/internal/claim"
	"github.com/trnahnh/idemio/internal/correlation"
	"github.com/trnahnh/idemio/internal/faketest"
	"github.com/trnahnh/idemio/internal/probe"
	"github.com/trnahnh/idemio/internal/reconcile"
	"github.com/trnahnh/idemio/internal/telemetry"
	"github.com/trnahnh/idemio/internal/testdb"
)

const (
	agentID    = "agent-checkout-flow"
	keyA       = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	resourceID = "inv_8842"
	staleAfter = time.Hour
)

type stubProber struct {
	outcome probe.Outcome
	result  json.RawMessage
	err     error
}

func (s stubProber) Probe(context.Context, string, string) (probe.Outcome, json.RawMessage, error) {
	return s.outcome, s.result, s.err
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func metricsFor(pool *pgxpool.Pool) *telemetry.Metrics {
	return telemetry.New(pool, 65536)
}

func claimPending(t *testing.T, pool *pgxpool.Pool, resourceType string) {
	t.Helper()

	_, err := claim.Claim(context.Background(), pool, claim.Request{
		AgentID:        agentID,
		Key:            keyA,
		RequestHash:    "sha256-jcs-v1:abc",
		ResourceType:   resourceType,
		ResourceID:     resourceID,
		Operation:      "create_charge",
		OperationClass: "create",
		Payload:        json.RawMessage(`{"amount_cents":4200}`),
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
}

func backdate(t *testing.T, pool *pgxpool.Pool, age time.Duration) {
	t.Helper()

	_, err := pool.Exec(context.Background(),
		"UPDATE idempotency_keys SET claimed_at = now() - make_interval(secs => $1)", age.Seconds())
	if err != nil {
		t.Fatalf("backdate: %v", err)
	}
}

func statusOf(t *testing.T, pool *pgxpool.Pool) claim.Status {
	t.Helper()

	record, found, err := claim.Lookup(context.Background(), pool, agentID, keyA)
	if err != nil || !found {
		t.Fatalf("lookup: found=%v err=%v", found, err)
	}
	return record.Status
}

func TestProbeFindingAnExecutionResolvesToDone(t *testing.T) {
	pool := testdb.New(t)
	claimPending(t, pool, "invoice")
	backdate(t, pool, 2*time.Hour)

	prober := stubProber{outcome: probe.Executed, result: json.RawMessage(`{"sequence":1}`)}
	summary, err := reconcile.New(pool, prober, staleAfter, metricsFor(pool), quietLogger()).Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if summary.Done != 1 {
		t.Fatalf("summary = %+v, want one done", summary)
	}
	if got := statusOf(t, pool); got != claim.StatusDone {
		t.Fatalf("status = %s, want done", got)
	}
}

func TestProbeFindingNoExecutionResolvesToFailed(t *testing.T) {
	pool := testdb.New(t)
	claimPending(t, pool, "invoice")
	backdate(t, pool, 2*time.Hour)

	prober := stubProber{outcome: probe.NotExecuted}
	if _, err := reconcile.New(pool, prober, staleAfter, metricsFor(pool), quietLogger()).Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := statusOf(t, pool); got != claim.StatusFailed {
		t.Fatalf("status = %s, want failed: a proven non-execution is re-claimable", got)
	}
}

func TestUnknownProbeResultEscalatesToIndeterminate(t *testing.T) {
	pool := testdb.New(t)
	claimPending(t, pool, "invoice")
	backdate(t, pool, 2*time.Hour)

	prober := stubProber{outcome: probe.Unknown}
	if _, err := reconcile.New(pool, prober, staleAfter, metricsFor(pool), quietLogger()).Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := statusOf(t, pool); got != claim.StatusIndeterminate {
		t.Fatalf("status = %s, want indeterminate", got)
	}
}

func TestUnprobeableResourceEscalatesToIndeterminate(t *testing.T) {
	pool := testdb.New(t)
	claimPending(t, pool, "unregistered_type")
	backdate(t, pool, 2*time.Hour)

	prober := stubProber{outcome: probe.Executed}
	if _, err := reconcile.New(pool, prober, staleAfter, metricsFor(pool), quietLogger()).Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := statusOf(t, pool); got != claim.StatusIndeterminate {
		t.Fatalf("status = %s, want indeterminate: an unprobeable path escalates", got)
	}
}

func TestUnreachableProbeLeavesTheKeyPending(t *testing.T) {
	pool := testdb.New(t)
	claimPending(t, pool, "invoice")
	backdate(t, pool, 2*time.Hour)

	prober := stubProber{err: errors.New("connection refused")}
	summary, err := reconcile.New(pool, prober, staleAfter, metricsFor(pool), quietLogger()).Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if summary.Unresolved != 1 || summary.Indeterminate != 0 {
		t.Fatalf("summary = %+v, want one unresolved and no escalation", summary)
	}
	if got := statusOf(t, pool); got != claim.StatusPending {
		t.Fatalf("status = %s, want pending", got)
	}
}

func TestFreshPendingKeysAreNotSwept(t *testing.T) {
	pool := testdb.New(t)
	claimPending(t, pool, "invoice")

	prober := stubProber{outcome: probe.NotExecuted}
	summary, err := reconcile.New(pool, prober, staleAfter, metricsFor(pool), quietLogger()).Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if summary.Scanned != 0 {
		t.Fatalf("summary = %+v, want nothing scanned", summary)
	}
	if got := statusOf(t, pool); got != claim.StatusPending {
		t.Fatalf("status = %s, want pending", got)
	}
}

func TestSweepResolvesFromTheDownstreamLedgerWithoutReExecuting(t *testing.T) {
	pool := testdb.New(t)
	fake := faketest.Start(t)
	claimPending(t, pool, "invoice")
	backdate(t, pool, 2*time.Hour)

	executeOnFake(t, fake, correlation.ID(agentID, keyA))
	if got := len(fake.Executions(t, correlation.ID(agentID, keyA))); got != 1 {
		t.Fatalf("setup executed %d times, want 1", got)
	}

	prober := probe.New(fake.DataURL, 3*time.Second)
	if _, err := reconcile.New(pool, prober, staleAfter, metricsFor(pool), quietLogger()).Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if got := statusOf(t, pool); got != claim.StatusDone {
		t.Fatalf("status = %s, want done", got)
	}
	if got := len(fake.Executions(t, correlation.ID(agentID, keyA))); got != 1 {
		t.Fatalf("downstream executed %d times, want 1: the reconciler must never re-execute", got)
	}
}

func executeOnFake(t *testing.T, fake *faketest.Fake, correlationID string) {
	t.Helper()

	body := `{"resource_type":"invoice","resource_id":"` + resourceID +
		`","operation":"create_charge","payload":{"amount_cents":4200}}`

	req, err := http.NewRequest(http.MethodPost, fake.DataURL+"/v1/execute", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Idemio-Correlation-Id", correlationID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	resp.Body.Close()
}

func TestReconcilerBinaryCannotWriteDownstream(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/trnahnh/idemio/cmd/reconciler").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v: %s", err, out)
	}

	for _, dependency := range strings.Fields(string(out)) {
		if dependency == "github.com/trnahnh/idemio/internal/downstream" {
			t.Fatal("cmd/reconciler links internal/downstream; the no-write-path invariant " +
				"is no longer structural")
		}
	}
}
