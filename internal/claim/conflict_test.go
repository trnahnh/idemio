package claim_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trnahnh/idemio/internal/claim"
	"github.com/trnahnh/idemio/internal/manifest"
	"github.com/trnahnh/idemio/internal/testdb"
)

func key(n int) string {
	return fmt.Sprintf("7c9e6679-7425-40de-944b-e07fc1f9%04d", n)
}

type writer struct {
	agent    string
	key      string
	class    string
	scope    []string
	resource string
	enforce  bool
}

func (w writer) request() claim.Request {
	resource := w.resource
	if resource == "" {
		resource = "inv_8842"
	}
	return claim.Request{
		AgentID:      w.agent,
		Key:          w.key,
		RequestHash:  "sha256-jcs-v1:" + w.key,
		ResourceType: "invoice",
		ResourceID:   resource,
		Operation:    "op_" + w.class,
		Declared:     manifest.Operation{Class: w.class, Scope: w.scope},
		Payload:      json.RawMessage(`{"amount_cents":4200}`),
		Window:       5 * time.Second,
		Enforce:      w.enforce,
		LockTimeout:  5 * time.Second,
	}
}

func mustClaim(t *testing.T, pool *pgxpool.Pool, w writer) claim.Result {
	t.Helper()

	result, err := claim.Claim(context.Background(), pool, w.request())
	if err != nil {
		t.Fatalf("claim %s: %v", w.key, err)
	}
	return result
}

func conflictRows(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()

	rows, err := pool.Query(context.Background(),
		"SELECT resolution::text, count(*) FROM conflicts GROUP BY 1")
	if err != nil {
		t.Fatalf("count conflicts: %v", err)
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var resolution string
		var count int
		if err := rows.Scan(&resolution, &count); err != nil {
			t.Fatalf("scan conflicts: %v", err)
		}
		counts[resolution] = count
	}
	return counts
}

// The failure this guards is mutual rejection: with the lock taken after the intent insert
// both writers see each other and both lose, leaving the resource unwritable (ADR-0015).
func TestConcurrentConflictingWritersLeaveExactlyOneWinner(t *testing.T) {
	pool := testdb.New(t)

	const racers = 8
	verdicts := make([]claim.Verdict, racers)

	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)

	for i := range racers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			verdicts[i] = mustClaim(t, pool, writer{
				agent:   fmt.Sprintf("agent-%d", i),
				key:     key(i),
				class:   manifest.ClassReplace,
				enforce: true,
			}).Verdict
		}()
	}
	start.Done()
	done.Wait()

	var claimed, rejected int
	for _, verdict := range verdicts {
		switch verdict {
		case claim.VerdictClaimed:
			claimed++
		case claim.VerdictRejected:
			rejected++
		default:
			t.Errorf("unexpected verdict %s", verdict)
		}
	}

	if claimed != 1 {
		t.Fatalf("%d writers were claimed, want exactly 1: %v", claimed, verdicts)
	}
	if rejected != racers-1 {
		t.Fatalf("%d writers were rejected, want %d", rejected, racers-1)
	}
}

func TestARejectionIsTerminalAndCarriesItsOwnBody(t *testing.T) {
	pool := testdb.New(t)

	mustClaim(t, pool, writer{agent: "agent-a", key: key(1), class: manifest.ClassReplace, enforce: true})
	rejected := mustClaim(t, pool, writer{agent: "agent-b", key: key(2), class: manifest.ClassReplace, enforce: true})

	if rejected.Verdict != claim.VerdictRejected {
		t.Fatalf("verdict = %s, want rejected", rejected.Verdict)
	}

	var body map[string]any
	if err := json.Unmarshal(rejected.Record.Result, &body); err != nil {
		t.Fatalf("rejection body is not JSON: %v", err)
	}
	for _, field := range []string{"conflict_id", "conflicting_intent_id", "reason", "detail"} {
		if body[field] == nil || body[field] == "" {
			t.Errorf("rejection body has no %s; the 409 could not be replayed in full", field)
		}
	}

	// The stored body is what a replay returns, so it must survive a re-read of the row.
	stored, found, err := claim.Lookup(context.Background(), pool, "agent-b", key(2))
	if err != nil || !found {
		t.Fatalf("lookup: found=%v err=%v", found, err)
	}
	if stored.Status != claim.StatusRejected {
		t.Fatalf("stored status = %s, want rejected", stored.Status)
	}
	if string(stored.Result) != string(rejected.Record.Result) {
		t.Fatal("the stored 409 body differs from the one returned; a replay would not match")
	}
}

// One rejection must not cascade. A write that never executed is not something the next
// writer can conflict with (ADR-0015).
func TestARejectedWriteDoesNotRejectTheNextOne(t *testing.T) {
	pool := testdb.New(t)

	mustClaim(t, pool, writer{agent: "agent-a", key: key(1), class: manifest.ClassAppend, enforce: true})

	rejected := mustClaim(t, pool, writer{agent: "agent-b", key: key(2), class: manifest.ClassReplace, enforce: true})
	if rejected.Verdict != claim.VerdictRejected {
		t.Fatalf("second verdict = %s, want rejected", rejected.Verdict)
	}

	third := mustClaim(t, pool, writer{agent: "agent-c", key: key(3), class: manifest.ClassAppend, enforce: true})
	if third.Verdict != claim.VerdictClaimed {
		t.Fatalf("third verdict = %s, want claimed: a rejected intent is still in the window",
			third.Verdict)
	}
}

func TestAFailedWriteStopsBlockingTheWindow(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	mustClaim(t, pool, writer{agent: "agent-a", key: key(1), class: manifest.ClassReplace, enforce: true})

	updated, err := claim.Complete(ctx, pool, "agent-a", key(1), claim.StatusFailed, nil, "unreachable")
	if err != nil || !updated {
		t.Fatalf("complete: updated=%v err=%v", updated, err)
	}

	second := mustClaim(t, pool, writer{agent: "agent-b", key: key(2), class: manifest.ClassReplace, enforce: true})
	if second.Verdict != claim.VerdictClaimed {
		t.Fatalf("verdict = %s, want claimed: a write that provably never executed is still "+
			"counting against the next one", second.Verdict)
	}
}

func TestAnIndeterminateWriteKeepsBlockingTheWindow(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	mustClaim(t, pool, writer{agent: "agent-a", key: key(1), class: manifest.ClassReplace, enforce: true})

	if _, err := claim.Complete(ctx, pool, "agent-a", key(1),
		claim.StatusIndeterminate, nil, "unknown"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	second := mustClaim(t, pool, writer{agent: "agent-b", key: key(2), class: manifest.ClassReplace, enforce: true})
	if second.Verdict != claim.VerdictRejected {
		t.Fatalf("verdict = %s, want rejected: an outcome nobody knows may have executed",
			second.Verdict)
	}
}

func TestShadowModeRecordsTheConflictWithoutRejecting(t *testing.T) {
	pool := testdb.New(t)

	mustClaim(t, pool, writer{agent: "agent-a", key: key(1), class: manifest.ClassReplace})
	second := mustClaim(t, pool, writer{agent: "agent-b", key: key(2), class: manifest.ClassReplace})

	if second.Verdict != claim.VerdictClaimed {
		t.Fatalf("verdict = %s, want claimed: shadow mode must not change behaviour", second.Verdict)
	}
	if second.Observed != 1 {
		t.Fatalf("observed = %d, want 1", second.Observed)
	}

	counts := conflictRows(t, pool)
	if counts[string(claim.ResolutionObserved)] != 1 {
		t.Fatalf("observed conflict rows = %d, want 1", counts[string(claim.ResolutionObserved)])
	}
	if counts[string(claim.ResolutionRejected)] != 0 {
		t.Fatal("shadow mode wrote a rejected conflict row")
	}
}

func TestSameAgentConflictAsksToSerialize(t *testing.T) {
	pool := testdb.New(t)

	mustClaim(t, pool, writer{agent: "agent-a", key: key(1), class: manifest.ClassReplace, enforce: true})
	second := mustClaim(t, pool, writer{agent: "agent-a", key: key(2), class: manifest.ClassReplace, enforce: true})

	if second.Verdict != claim.VerdictSerialize {
		t.Fatalf("verdict = %s, want serialize", second.Verdict)
	}
	if second.Blocking.Key != key(1) {
		t.Fatalf("blocking key = %s, want %s", second.Blocking.Key, key(1))
	}

	// Nothing was claimed, so the caller can retry the whole transaction.
	if _, found, _ := claim.Lookup(context.Background(), pool, "agent-a", key(2)); found {
		t.Fatal("a serializing request claimed its key; it must roll back and retry")
	}
}

// Once the earlier write is terminal there is nothing left to wait for, which is what stops
// the retry loop rather than merely bounding it.
func TestSameAgentConflictProceedsOnceTheEarlierWriteIsDone(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	mustClaim(t, pool, writer{agent: "agent-a", key: key(1), class: manifest.ClassReplace, enforce: true})
	if _, err := claim.Complete(ctx, pool, "agent-a", key(1), claim.StatusDone,
		json.RawMessage(`{"ok":true}`), ""); err != nil {
		t.Fatalf("complete: %v", err)
	}

	second := mustClaim(t, pool, writer{agent: "agent-a", key: key(2), class: manifest.ClassReplace, enforce: true})
	if second.Verdict != claim.VerdictClaimed {
		t.Fatalf("verdict = %s, want claimed", second.Verdict)
	}
	if counts := conflictRows(t, pool); counts[string(claim.ResolutionSerialized)] != 1 {
		t.Fatalf("serialized conflict rows = %d, want 1", counts[string(claim.ResolutionSerialized)])
	}
}

// ADR-0015: the lock is taken before anything is written, so a timeout leaves no trace and
// the honest answer is the retryable one.
func TestLockTimeoutWritesNothing(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer holder.Rollback(ctx)

	if _, err := holder.Exec(ctx,
		"SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", "invoice:inv_8842"); err != nil {
		t.Fatalf("hold lock: %v", err)
	}

	request := writer{agent: "agent-a", key: key(1), class: manifest.ClassCreate}.request()
	request.LockTimeout = 100 * time.Millisecond

	result, err := claim.Claim(ctx, pool, request)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if result.Verdict != claim.VerdictLockTimeout {
		t.Fatalf("verdict = %s, want lock_timeout", result.Verdict)
	}

	if _, found, _ := claim.Lookup(ctx, pool, "agent-a", key(1)); found {
		t.Fatal("a request that timed out on the lock still claimed its key")
	}

	var intents int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM write_intents").Scan(&intents); err != nil {
		t.Fatalf("count intents: %v", err)
	}
	if intents != 0 {
		t.Fatalf("intents = %d, want 0: nothing may be written before the lock is held", intents)
	}
}

func TestCompatibleOperationsBothProceed(t *testing.T) {
	pool := testdb.New(t)

	first := mustClaim(t, pool, writer{agent: "agent-a", key: key(1),
		class: manifest.ClassMutate, scope: []string{"status"}, enforce: true})
	second := mustClaim(t, pool, writer{agent: "agent-b", key: key(2),
		class: manifest.ClassMutate, scope: []string{"billing_address"}, enforce: true})

	if first.Verdict != claim.VerdictClaimed || second.Verdict != claim.VerdictClaimed {
		t.Fatalf("verdicts = %s, %s; want both claimed. Disjoint scopes must not conflict, or "+
			"the matrix is a per-resource mutex", first.Verdict, second.Verdict)
	}
	if counts := conflictRows(t, pool); len(counts) != 0 {
		t.Fatalf("compatible writes recorded conflicts: %v", counts)
	}
}

func TestConflictsOnOtherResourcesDoNotInterfere(t *testing.T) {
	pool := testdb.New(t)

	first := mustClaim(t, pool, writer{agent: "agent-a", key: key(1),
		class: manifest.ClassReplace, resource: "inv_1", enforce: true})
	second := mustClaim(t, pool, writer{agent: "agent-b", key: key(2),
		class: manifest.ClassReplace, resource: "inv_2", enforce: true})

	if first.Verdict != claim.VerdictClaimed || second.Verdict != claim.VerdictClaimed {
		t.Fatalf("verdicts = %s, %s; want both claimed", first.Verdict, second.Verdict)
	}
}
