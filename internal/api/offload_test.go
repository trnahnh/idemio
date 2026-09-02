package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func storedRef(t *testing.T, h *harness, agent, key string) string {
	t.Helper()

	var ref *string
	err := h.pool.QueryRow(context.Background(),
		"SELECT result_ref FROM idempotency_keys WHERE agent_id = $1 AND idempotency_key = $2::uuid",
		agent, key).Scan(&ref)
	if err != nil {
		t.Fatalf("read result_ref: %v", err)
	}
	if ref == nil {
		return ""
	}
	return *ref
}

func storedInline(t *testing.T, h *harness, agent, key string) int {
	t.Helper()

	var length int
	err := h.pool.QueryRow(context.Background(),
		"SELECT coalesce(length(result::text), 0) FROM idempotency_keys "+
			"WHERE agent_id = $1 AND idempotency_key = $2::uuid", agent, key).Scan(&length)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	return length
}

// The fake echoes a sequence number, so a tiny cap is what forces a result over it rather
// than a contrived payload.
const tinyCap = 8

func TestAnOversizedResultIsOffloadedAndReplaysIdentically(t *testing.T) {
	h := newOffloadHarness(t, tinyCap)

	first := h.write(t, agentID, keyA, `{"amount_cents":4200}`)
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", first.StatusCode)
	}
	created := decode(t, first)
	first.Body.Close()

	ref := storedRef(t, h, agentID, keyA)
	if ref == "" {
		t.Fatal("the result was not offloaded despite exceeding the cap")
	}
	if got := storedInline(t, h, agentID, keyA); got != 0 {
		t.Fatalf("inline result is %d bytes, want 0: the row still holds what was offloaded", got)
	}

	replay := h.write(t, agentID, keyA, `{"amount_cents":4200}`)
	defer replay.Body.Close()

	if replay.StatusCode != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", replay.StatusCode)
	}
	replayed := decode(t, replay)

	original, _ := json.Marshal(created["result"])
	returned, _ := json.Marshal(replayed["result"])
	if string(original) != string(returned) {
		t.Fatalf("replay returned %s, want the stored result %s", returned, original)
	}
	if got := h.executions(t, agentID, keyA); got != 1 {
		t.Fatalf("executions = %d, want 1: a replay re-executed", got)
	}
}

func TestAnOffloadedResultIsReadableThroughTheGetEndpoint(t *testing.T) {
	h := newOffloadHarness(t, tinyCap)

	h.write(t, agentID, keyA, `{"amount_cents":4200}`).Body.Close()

	resp := h.read(t, agentID, keyA)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := decode(t, resp)
	if body["result"] == nil {
		t.Fatal("GET returned no result for an offloaded write")
	}
}

// The cap is a performance guard, not a correctness one. Losing a result because storage
// was unreachable would be strictly worse than exceeding a size limit.
func TestAnUnreachableObjectStoreFallsBackToInline(t *testing.T) {
	h := newUnreachableOffloadHarness(t, tinyCap)

	resp := h.write(t, agentID, keyA, `{"amount_cents":4200}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: the write completed and must be recorded", resp.StatusCode)
	}
	if ref := storedRef(t, h, agentID, keyA); ref != "" {
		t.Fatalf("result_ref = %q, want empty: nothing reached object storage", ref)
	}
	if got := storedInline(t, h, agentID, keyA); got == 0 {
		t.Fatal("the result was lost rather than stored inline over the cap")
	}

	replay := h.write(t, agentID, keyA, `{"amount_cents":4200}`)
	defer replay.Body.Close()
	if replay.StatusCode != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", replay.StatusCode)
	}
}

// The key is terminal and the write definitely ran, so this reports a failure of this layer.
// Answering from the not-executed family would be a lie about a write that happened.
func TestAnUnreadableOffloadedResultIsNotReportedAsNotExecuted(t *testing.T) {
	h := newOffloadHarness(t, tinyCap)

	h.write(t, agentID, keyA, `{"amount_cents":4200}`).Body.Close()
	if ref := storedRef(t, h, agentID, keyA); ref == "" {
		t.Fatal("the result was not offloaded, so this test proves nothing")
	}

	// Point the row at an object that was never written.
	_, err := h.pool.Exec(context.Background(),
		"UPDATE idempotency_keys SET result_ref = 'results/missing.json' "+
			"WHERE agent_id = $1 AND idempotency_key = $2::uuid", agentID, keyA)
	if err != nil {
		t.Fatalf("rewrite ref: %v", err)
	}

	resp := h.write(t, agentID, keyA, `{"amount_cents":4200}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: 503 would assert the write never executed", resp.StatusCode)
	}
	body := decode(t, resp)
	if body["reason"] != "result_unavailable" {
		t.Errorf("reason = %v, want result_unavailable", body["reason"])
	}
	if body["status"] == string("failed") {
		t.Error("the response claims the write failed; it completed and its outcome is recorded")
	}
	if got := h.executions(t, agentID, keyA); got != 1 {
		t.Fatalf("executions = %d, want 1: an unreadable result caused a re-execution", got)
	}
}

func TestASmallResultIsStillStoredInline(t *testing.T) {
	h := newOffloadHarness(t, 1<<20)

	h.write(t, agentID, keyA, `{"amount_cents":4200}`).Body.Close()

	if ref := storedRef(t, h, agentID, keyA); ref != "" {
		t.Fatalf("result_ref = %q, want empty: a result under the cap must not be offloaded", ref)
	}
	if got := storedInline(t, h, agentID, keyA); got == 0 {
		t.Fatal("a small result was neither inline nor offloaded")
	}
}

func TestTheOffloadedObjectNameIsScopedToTheKey(t *testing.T) {
	h := newOffloadHarness(t, tinyCap)

	h.write(t, agentID, keyA, `{"amount_cents":4200}`).Body.Close()
	h.write(t, otherAgent, keyB, `{"amount_cents":4200}`).Body.Close()

	first := storedRef(t, h, agentID, keyA)
	second := storedRef(t, h, otherAgent, keyB)

	if first == "" || second == "" {
		t.Fatalf("refs = %q and %q, want both set", first, second)
	}
	if first == second {
		t.Fatal("two different keys share one object; one would overwrite the other")
	}
	for _, ref := range []string{first, second} {
		if !strings.HasPrefix(ref, "results/") {
			t.Errorf("ref %q is not under the results prefix", ref)
		}
	}
}
