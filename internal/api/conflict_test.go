package api_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	thirdAgent = "agent-reporting"
	keyC       = "b2c3d4e5-6f7a-4b8c-9d0e-1f2a3b4c5d6e"
	keyD       = "c3d4e5f6-7a8b-4c9d-8e0f-2a3b4c5d6e7f"
)

func conflictCount(t *testing.T, pool *pgxpool.Pool, resolution string) int {
	t.Helper()

	var count int
	err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM conflicts WHERE resolution::text = $1", resolution).Scan(&count)
	if err != nil {
		t.Fatalf("count conflicts: %v", err)
	}
	return count
}

// ROADMAP Phase 1 exit criterion 1. The oracle is the fake's execution ledger, never
// idemio's own record of what it believes happened.
func TestIncompatibleWritesRejectTheSecondAndExecuteOnce(t *testing.T) {
	h := newEnforcingHarness(t)

	first := h.writeOp(t, agentID, keyA, "invoice", resourceID, "create_charge", `{"amount_cents":4200}`)
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", first.StatusCode)
	}
	first.Body.Close()

	second := h.writeOp(t, otherAgent, keyB, "invoice", resourceID, "add_line_item", `{"sku":"x"}`)
	defer second.Body.Close()

	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second status = %d, want 409", second.StatusCode)
	}
	body := decode(t, second)
	if body["reason"] != "conflicting_write" {
		t.Errorf("reason = %v, want conflicting_write", body["reason"])
	}
	if body["conflict_id"] == nil || body["conflicting_intent_id"] == nil {
		t.Error("the 409 does not name the conflict it lost to")
	}

	if got := conflictCount(t, h.pool, "rejected"); got != 1 {
		t.Errorf("rejected conflict rows = %d, want 1", got)
	}
	if got := h.executions(t, agentID, keyA); got != 1 {
		t.Errorf("first write executed %d times downstream, want 1", got)
	}
	if got := h.executions(t, otherAgent, keyB); got != 0 {
		t.Fatalf("the rejected write executed %d times downstream, want 0", got)
	}
}

// ROADMAP Phase 1 exit criterion 2. This is the criterion that separates a compatibility
// matrix from a per-resource mutex.
func TestCompatibleWritesBothExecute(t *testing.T) {
	cases := []struct {
		name              string
		firstOp, secondOp string
		firstBody         string
		secondBody        string
	}{
		{"two appends", "add_line_item", "add_line_item", `{"sku":"a"}`, `{"sku":"b"}`},
		{"append beside a mutate", "add_line_item", "update_status", `{"sku":"a"}`, `{"status":"paid"}`},
		{"disjoint mutates", "update_status", "update_billing_address",
			`{"status":"paid"}`, `{"billing_address":"1 Main St"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newEnforcingHarness(t)

			first := h.writeOp(t, agentID, keyA, "invoice", resourceID, tc.firstOp, tc.firstBody)
			second := h.writeOp(t, otherAgent, keyB, "invoice", resourceID, tc.secondOp, tc.secondBody)
			defer first.Body.Close()
			defer second.Body.Close()

			if first.StatusCode != http.StatusCreated || second.StatusCode != http.StatusCreated {
				t.Fatalf("statuses = %d, %d; want 201 and 201. Compatible writes to one resource "+
					"must both proceed", first.StatusCode, second.StatusCode)
			}
			if got := conflictCount(t, h.pool, "rejected"); got != 0 {
				t.Errorf("rejected conflict rows = %d, want 0", got)
			}

			if got := h.executions(t, agentID, keyA); got != 1 {
				t.Errorf("first write executed %d times, want 1", got)
			}
			if got := h.executions(t, otherAgent, keyB); got != 1 {
				t.Errorf("second write executed %d times, want 1", got)
			}
		})
	}
}

// ROADMAP Phase 1 exit criterion 3. Serialization is only real if the downstream sees the
// second write after the first one finished, which is what the ledger timestamps prove.
func TestSameAgentConflictsSerializeDownstream(t *testing.T) {
	h := newEnforcingHarness(t)
	h.fake.Script(t, resourceID, "slow", "succeed")

	var wait sync.WaitGroup
	statuses := make([]int, 2)

	wait.Add(1)
	go func() {
		defer wait.Done()
		resp := h.writeOp(t, agentID, keyA, "invoice", resourceID, "create_charge", `{"amount_cents":1}`)
		defer resp.Body.Close()
		statuses[0] = resp.StatusCode
	}()

	time.Sleep(100 * time.Millisecond)

	wait.Add(1)
	go func() {
		defer wait.Done()
		resp := h.writeOp(t, agentID, keyB, "invoice", resourceID, "create_charge", `{"amount_cents":2}`)
		defer resp.Body.Close()
		statuses[1] = resp.StatusCode
	}()
	wait.Wait()

	if statuses[0] != http.StatusCreated || statuses[1] != http.StatusCreated {
		t.Fatalf("statuses = %v, want both 201: a same-agent conflict serializes, never rejects",
			statuses)
	}
	if got := conflictCount(t, h.pool, "serialized"); got != 1 {
		t.Errorf("serialized conflict rows = %d, want 1", got)
	}

	executions := h.fake.Executions(t, "")
	if len(executions) != 2 {
		t.Fatalf("downstream saw %d executions, want 2", len(executions))
	}

	// The first write is scripted slow, so overlapping calls would land within milliseconds
	// of each other. Serialization is the only way the gap exceeds the first call's duration.
	gap := executions[1].ReceivedAt.Sub(executions[0].ReceivedAt)
	if gap < 300*time.Millisecond {
		t.Fatalf("the two calls reached the downstream %s apart; the second did not wait for "+
			"the first to finish, so 'serialized' describes a lock and not an outcome", gap)
	}
}

func TestARejectionReplaysIdenticallyForever(t *testing.T) {
	h := newEnforcingHarness(t)

	first := h.writeOp(t, agentID, keyA, "invoice", resourceID, "create_charge", `{"amount_cents":4200}`)
	first.Body.Close()

	rejected := h.writeOp(t, otherAgent, keyB, "invoice", resourceID, "add_line_item", `{"sku":"x"}`)
	original := decode(t, rejected)
	rejected.Body.Close()

	replay := h.writeOp(t, otherAgent, keyB, "invoice", resourceID, "add_line_item", `{"sku":"x"}`)
	defer replay.Body.Close()

	if replay.StatusCode != http.StatusConflict {
		t.Fatalf("replay status = %d, want 409: a rejection is terminal", replay.StatusCode)
	}
	repeated := decode(t, replay)
	for _, field := range []string{"conflict_id", "conflicting_intent_id", "reason", "detail"} {
		if original[field] != repeated[field] {
			t.Errorf("%s changed on replay: %v then %v", field, original[field], repeated[field])
		}
	}

	if got := h.executions(t, otherAgent, keyB); got != 0 {
		t.Fatalf("a replayed rejection executed %d times downstream, want 0", got)
	}
}

// ADR-0013: enforcement is off until a manifest says otherwise, so onboarding can watch
// what the matrix would do before it does it.
func TestShadowModeObservesWithoutRejecting(t *testing.T) {
	h := newHarness(t, 5*time.Second)

	first := h.writeOp(t, agentID, keyA, "invoice", resourceID, "create_charge", `{"amount_cents":4200}`)
	second := h.writeOp(t, otherAgent, keyB, "invoice", resourceID, "add_line_item", `{"sku":"x"}`)
	defer first.Body.Close()
	defer second.Body.Close()

	if second.StatusCode != http.StatusCreated {
		t.Fatalf("second status = %d, want 201: shadow mode must not reject", second.StatusCode)
	}
	if got := conflictCount(t, h.pool, "observed"); got != 1 {
		t.Errorf("observed conflict rows = %d, want 1", got)
	}
	if got := conflictCount(t, h.pool, "rejected"); got != 0 {
		t.Errorf("rejected conflict rows = %d, want 0", got)
	}
	if got := h.executions(t, otherAgent, keyB); got != 1 {
		t.Errorf("the observed write executed %d times, want 1", got)
	}
}

// ADR-0014: a write whose downstream responses cannot be classified is never admitted.
func TestAnUndeclaredOperationIsRejectedBeforeTheClaim(t *testing.T) {
	h := newHarness(t, 5*time.Second)

	cases := []struct{ resourceType, operation string }{
		{"invoice", "delete_everything"},
		{"never_onboarded", "create_charge"},
	}

	for _, tc := range cases {
		resp := h.writeOp(t, agentID, keyA, tc.resourceType, resourceID, tc.operation, `{"a":1}`)
		body := decode(t, resp)
		resp.Body.Close()

		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s/%s status = %d, want 422", tc.resourceType, tc.operation, resp.StatusCode)
		}
		if body["reason"] != "unknown_operation" {
			t.Errorf("%s/%s reason = %v, want unknown_operation", tc.resourceType, tc.operation, body["reason"])
		}
		if got := h.executions(t, agentID, keyA); got != 0 {
			t.Fatalf("%s/%s executed %d times downstream, want 0", tc.resourceType, tc.operation, got)
		}
	}
}

// The window is declared per resource_type, so two types on one resource id never interact.
func TestConflictDetectionIsScopedToTheResourceType(t *testing.T) {
	h := newEnforcingHarness(t)

	first := h.writeOp(t, agentID, keyA, "invoice", resourceID, "create_charge", `{"amount_cents":4200}`)
	second := h.writeOp(t, otherAgent, keyB, "subscription", resourceID, "create_subscription", `{"plan":"pro"}`)
	defer first.Body.Close()
	defer second.Body.Close()

	if first.StatusCode != http.StatusCreated || second.StatusCode != http.StatusCreated {
		t.Fatalf("statuses = %d, %d; want both 201", first.StatusCode, second.StatusCode)
	}
}

// A refusal the manifest classifies as not-executed leaves a re-claimable key, and the
// ledger must agree that nothing ran.
func TestASubscriptionRefusalIsFailedNotIndeterminate(t *testing.T) {
	h := newHarness(t, 5*time.Second)
	h.fake.Script(t, "sub_1", "refuse")

	resp := h.writeOp(t, agentID, keyC, "subscription", "sub_1", "create_subscription", `{"plan":"pro"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: 429 is declared not-executed for subscription",
			resp.StatusCode)
	}
	if got := h.executions(t, agentID, keyC); got != 0 {
		t.Fatalf("a refused write executed %d times downstream, want 0", got)
	}

	retry := h.writeOp(t, agentID, keyC, "subscription", "sub_1", "create_subscription", `{"plan":"pro"}`)
	defer retry.Body.Close()

	if retry.StatusCode != http.StatusCreated {
		t.Fatalf("retry status = %d, want 201: failed is the only re-claimable status",
			retry.StatusCode)
	}
}
