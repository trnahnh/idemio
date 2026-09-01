package faketest_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/trnahnh/idemio/internal/faketest"
)

func execute(t *testing.T, fake *faketest.Fake, correlationID, resourceID string) (int, error) {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"resource_type": "order",
		"resource_id":   resourceID,
		"operation":     "charge",
		"payload":       map[string]any{"amount": 4200},
	})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, fake.DataURL+"/v1/execute", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Idemio-Correlation-Id", correlationID)

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func TestScriptedBehavioursRunInOrder(t *testing.T) {
	fake := faketest.Start(t)
	fake.Script(t, "r1", "business-failure", "succeed")

	first, err := execute(t, fake, "corr-1", "r1")
	if err != nil {
		t.Fatalf("first execute: %v", err)
	}
	if first != http.StatusUnprocessableEntity {
		t.Errorf("first status = %d, want 422", first)
	}

	second, err := execute(t, fake, "corr-1", "r1")
	if err != nil {
		t.Fatalf("second execute: %v", err)
	}
	if second != http.StatusOK {
		t.Errorf("second status = %d, want 200", second)
	}

	executions := fake.Executions(t, "corr-1")
	if len(executions) != 2 {
		t.Fatalf("probe reports %d executions, want 2", len(executions))
	}
	if executions[0].Behavior != "business-failure" || executions[1].Behavior != "succeed" {
		t.Errorf("behaviours ran as %s then %s", executions[0].Behavior, executions[1].Behavior)
	}
}

func TestUnscriptedResourceSucceeds(t *testing.T) {
	fake := faketest.Start(t)

	status, err := execute(t, fake, "corr-1", "unscripted")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
}

func TestDownListenerRefusesConnections(t *testing.T) {
	fake := faketest.Start(t)
	fake.SetListener(t, false)

	_, err := execute(t, fake, "corr-1", "r1")
	if err == nil {
		t.Fatal("expected a connection error while the data listener is down")
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		t.Fatalf("error is %T, want a *net.OpError from a refused connection: %v", err, err)
	}
	if executions := fake.Executions(t, ""); len(executions) != 0 {
		t.Fatalf("ledger recorded %d executions, want 0: a refused connection must be provably not executed",
			len(executions))
	}

	fake.SetListener(t, true)

	status, err := execute(t, fake, "corr-1", "r1")
	if err != nil {
		t.Fatalf("execute after listener restored: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status after listener restored = %d, want 200", status)
	}
}

func TestHangRecordsExecutionBeforeBlocking(t *testing.T) {
	fake := faketest.Start(t)
	fake.Script(t, "r1", "hang")

	if _, err := execute(t, fake, "corr-1", "r1"); err == nil {
		t.Fatal("expected the client to time out against a hanging downstream")
	}

	executions := fake.Executions(t, "corr-1")
	if len(executions) != 1 {
		t.Fatalf("probe reports %d executions, want 1: a hung call must still be recorded",
			len(executions))
	}
	if lines := fake.LedgerLines(t); len(lines) != 1 {
		t.Fatalf("ledger holds %d lines, want 1: the record must be durable before the hang", len(lines))
	}
}
