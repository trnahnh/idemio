package probe_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/trnahnh/idemio/internal/correlation"
	"github.com/trnahnh/idemio/internal/faketest"
	"github.com/trnahnh/idemio/internal/probe"
)

const (
	agentID    = "agent-checkout-flow"
	keyA       = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	resourceID = "inv_8842"
)

func execute(t *testing.T, fake *faketest.Fake, key string) {
	t.Helper()

	body := `{"resource_type":"invoice","resource_id":"` + resourceID +
		`","operation":"create_charge","payload":{"amount_cents":4200}}`

	request, err := http.NewRequest(http.MethodPost, fake.DataURL+"/v1/execute",
		strings.NewReader(body))
	if err != nil {
		t.Fatalf("build execute request: %v", err)
	}
	request.Header.Set("X-Idemio-Correlation-Id", correlation.ID(agentID, key))

	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	resp.Body.Close()
}

// A probe that finds nothing must say so definitively. Answering Unknown here would escalate
// a recoverable key to indeterminate, which is terminal and needs a human.
func TestNoExecutionIsNotExecuted(t *testing.T) {
	fake := faketest.Start(t)

	outcome, result, err := probe.New(fake.DataURL, 5*time.Second).
		Probe(context.Background(), "/probe", agentID, keyA)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if outcome != probe.NotExecuted {
		t.Fatalf("outcome = %s, want not_executed", outcome)
	}
	if len(result) != 0 {
		t.Errorf("result = %s, want nothing", result)
	}
}

func TestAnExecutionIsFoundAndCarriesItsRecord(t *testing.T) {
	fake := faketest.Start(t)
	execute(t, fake, keyA)

	outcome, result, err := probe.New(fake.DataURL, 5*time.Second).
		Probe(context.Background(), "/probe", agentID, keyA)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if outcome != probe.Executed {
		t.Fatalf("outcome = %s, want executed", outcome)
	}
	if !strings.Contains(string(result), correlation.ID(agentID, keyA)) {
		t.Errorf("result = %s, want the execution record for this key", result)
	}
}

// The probe is scoped by correlation id, or crash recovery would attribute one key's
// execution to another and resolve a write that never happened.
func TestAnotherKeysExecutionIsNotVisible(t *testing.T) {
	fake := faketest.Start(t)
	execute(t, fake, keyA)

	other := "a1b2c3d4-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	outcome, _, err := probe.New(fake.DataURL, 5*time.Second).
		Probe(context.Background(), "/probe", agentID, other)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if outcome != probe.NotExecuted {
		t.Fatalf("outcome = %s for a key that never ran, want not_executed", outcome)
	}
}

// An unreachable probe must error rather than answer. The reconciler leaves such a key
// pending and retries; treating silence as "not executed" would re-claim a live write.
func TestAnUnreachableProbeErrorsRatherThanAnswering(t *testing.T) {
	outcome, _, err := probe.New("http://127.0.0.1:1", time.Second).
		Probe(context.Background(), "/probe", agentID, keyA)

	if err == nil {
		t.Fatal("an unreachable probe reported an outcome")
	}
	if outcome != probe.Unknown {
		t.Fatalf("outcome = %s, want unknown: the zero value must be the safe one", outcome)
	}
}

func TestAWrongPathIsAnErrorNotAnAnswer(t *testing.T) {
	fake := faketest.Start(t)
	execute(t, fake, keyA)

	outcome, _, err := probe.New(fake.DataURL, 5*time.Second).
		Probe(context.Background(), "/no-such-probe", agentID, keyA)

	if err == nil {
		t.Fatal("probing a path that does not exist reported an outcome")
	}
	if outcome != probe.Unknown {
		t.Fatalf("outcome = %s, want unknown", outcome)
	}
}
