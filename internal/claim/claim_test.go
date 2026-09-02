package claim_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/trnahnh/idemio/internal/claim"
	"github.com/trnahnh/idemio/internal/manifest"
	"github.com/trnahnh/idemio/internal/testdb"
)

func request(key string) claim.Request {
	return claim.Request{
		AgentID:      "agent-checkout-flow",
		Key:          key,
		RequestHash:  "sha256-jcs-v1:abc",
		ResourceType: "invoice",
		ResourceID:   "inv_8842",
		Operation:    "create_charge",
		Declared:     manifest.Operation{Class: manifest.ClassCreate},
		Payload:      json.RawMessage(`{"amount_cents":4200}`),
		Window:       5 * time.Second,
		LockTimeout:  5 * time.Second,
	}
}

const keyA = "7c9e6679-7425-40de-944b-e07fc1f90ae7"

func TestFirstClaimWinsAndSecondSeesPending(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	first, err := claim.Claim(ctx, pool, request(keyA))
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if first.Verdict != claim.VerdictClaimed {
		t.Fatalf("first verdict = %s, want claimed", first.Verdict)
	}
	if first.IntentID == "" {
		t.Error("claim did not record an intent")
	}

	second, err := claim.Claim(ctx, pool, request(keyA))
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if second.Verdict != claim.VerdictExisting {
		t.Fatalf("second verdict = %s, want existing", second.Verdict)
	}
	if second.Record.Status != claim.StatusPending {
		t.Errorf("second status = %s, want pending", second.Record.Status)
	}
}

func TestConcurrentClaimsProduceExactlyOneWinner(t *testing.T) {
	pool := testdb.New(t)

	const racers = 32
	verdicts := make([]claim.Verdict, racers)
	errs := make([]error, racers)

	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)

	for i := range racers {
		done.Go(func() {
			start.Wait()
			result, err := claim.Claim(context.Background(), pool, request(keyA))
			verdicts[i], errs[i] = result.Verdict, err
		})
	}

	start.Done()
	done.Wait()

	winners := 0
	for i, verdict := range verdicts {
		if errs[i] != nil {
			t.Fatalf("racer %d: %v", i, errs[i])
		}
		if verdict == claim.VerdictClaimed {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d racers claimed the key, want exactly 1", winners)
	}

	var intents int
	err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM write_intents WHERE idempotency_key = $1::uuid", keyA).Scan(&intents)
	if err != nil {
		t.Fatalf("count intents: %v", err)
	}
	if intents != 1 {
		t.Fatalf("%d intents written, want 1", intents)
	}
}

func TestDifferentRequestHashIsRejected(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	if _, err := claim.Claim(ctx, pool, request(keyA)); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	changed := request(keyA)
	changed.RequestHash = "sha256-jcs-v1:different"

	result, err := claim.Claim(ctx, pool, changed)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if result.Verdict != claim.VerdictMismatch {
		t.Fatalf("verdict = %s, want mismatch", result.Verdict)
	}
}

func TestFailedKeyIsReclaimable(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	if _, err := claim.Claim(ctx, pool, request(keyA)); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	completed, err := claim.Complete(ctx, pool, "agent-checkout-flow", keyA,
		claim.StatusFailed, nil, "", "downstream_unreachable")
	if err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if !completed {
		t.Fatal("mark failed updated no row")
	}

	result, err := claim.Claim(ctx, pool, request(keyA))
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if result.Verdict != claim.VerdictClaimed {
		t.Fatalf("verdict = %s, want claimed", result.Verdict)
	}
	if result.Record.AttemptCount != 2 {
		t.Errorf("attempt_count = %d, want 2", result.Record.AttemptCount)
	}

	var intents int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM write_intents WHERE idempotency_key = $1::uuid", keyA).Scan(&intents); err != nil {
		t.Fatalf("count intents: %v", err)
	}
	if intents != 2 {
		t.Errorf("%d intents after reclaim, want 2", intents)
	}
}

func TestTerminalStatusesAreNotReclaimable(t *testing.T) {
	for _, status := range []claim.Status{claim.StatusDone, claim.StatusIndeterminate, claim.StatusRejected} {
		t.Run(string(status), func(t *testing.T) {
			pool := testdb.New(t)
			ctx := context.Background()

			if _, err := claim.Claim(ctx, pool, request(keyA)); err != nil {
				t.Fatalf("first claim: %v", err)
			}
			if _, err := claim.Complete(ctx, pool, "agent-checkout-flow", keyA, status,
				json.RawMessage(`{"charge_id":"ch_5521"}`), "", ""); err != nil {
				t.Fatalf("complete as %s: %v", status, err)
			}

			result, err := claim.Claim(ctx, pool, request(keyA))
			if err != nil {
				t.Fatalf("second claim: %v", err)
			}
			if result.Verdict != claim.VerdictExisting {
				t.Fatalf("verdict = %s, want existing", result.Verdict)
			}
			if result.Record.Status != status {
				t.Errorf("status = %s, want %s", result.Record.Status, status)
			}
		})
	}
}

func TestCompleteWillNotOverwriteATerminalOutcome(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	if _, err := claim.Claim(ctx, pool, request(keyA)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := claim.Complete(ctx, pool, "agent-checkout-flow", keyA, claim.StatusDone,
		json.RawMessage(`{"charge_id":"ch_5521"}`), "", ""); err != nil {
		t.Fatalf("first complete: %v", err)
	}

	updated, err := claim.Complete(ctx, pool, "agent-checkout-flow", keyA,
		claim.StatusIndeterminate, nil, "", "late writer")
	if err != nil {
		t.Fatalf("second complete: %v", err)
	}
	if updated {
		t.Fatal("a terminal outcome was overwritten")
	}
}

func TestLookupIsScopedToTheAgent(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	if _, err := claim.Claim(ctx, pool, request(keyA)); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if _, found, err := claim.Lookup(ctx, pool, "agent-checkout-flow", keyA); err != nil || !found {
		t.Fatalf("owner lookup: found=%v err=%v", found, err)
	}
	if _, found, err := claim.Lookup(ctx, pool, "agent-dunning", keyA); err != nil || found {
		t.Fatalf("other agent lookup: found=%v err=%v, want not found", found, err)
	}
}

func TestSameKeyFromAnotherAgentIsADifferentKey(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	if _, err := claim.Claim(ctx, pool, request(keyA)); err != nil {
		t.Fatalf("first agent claim: %v", err)
	}

	other := request(keyA)
	other.AgentID = "agent-dunning"

	result, err := claim.Claim(ctx, pool, other)
	if err != nil {
		t.Fatalf("second agent claim: %v", err)
	}
	if result.Verdict != claim.VerdictClaimed {
		t.Fatalf("verdict = %s, want claimed: namespaces must not collide", result.Verdict)
	}
}
