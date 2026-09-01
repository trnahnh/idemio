//go:build killtest

package reconcile_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/trnahnh/idemio/internal/claim"
	"github.com/trnahnh/idemio/internal/correlation"
	"github.com/trnahnh/idemio/internal/faketest"
	"github.com/trnahnh/idemio/internal/probe"
	"github.com/trnahnh/idemio/internal/reconcile"
	"github.com/trnahnh/idemio/internal/testdb"
)

// ROADMAP exit criterion 2, the gate it calls the one that matters most: kill the replica
// mid-downstream-call and prove the write is never executed twice. Verified against the
// downstream's ledger, never against idemio's own record.
//
// Process.Kill maps to TerminateProcess on Windows, which is as uncatchable and unmaskable
// as SIGKILL, so this is faithful on both platforms with no OS-specific code.
func TestKillMidCallNeverProducesASecondExecution(t *testing.T) {
	pool, databaseURL := testdb.NewWithURL(t)
	fake := faketest.Start(t)
	fake.Script(t, resourceID, "hang")

	address := startIdemio(t, databaseURL, fake.DataURL)
	correlationID := correlation.ID(agentID, keyA)

	inFlight := make(chan struct{})
	go func() {
		defer close(inFlight)
		writeTo(t, address)
	}()

	waitForDownstreamCall(t, fake, correlationID)
	killIdemio(t)
	<-inFlight

	if got := len(fake.Executions(t, correlationID)); got != 1 {
		t.Fatalf("downstream executed %d times before reconciliation, want 1", got)
	}
	if status := statusOf(t, pool); status != claim.StatusPending {
		t.Fatalf("status after kill = %s, want pending", status)
	}

	prober := probe.New(fake.DataURL, 3*time.Second)
	summary, err := reconcile.New(pool, prober, time.Millisecond, metricsFor(pool), quietLogger()).Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if summary.Scanned != 1 {
		t.Fatalf("summary = %+v, want one key scanned", summary)
	}

	if status := statusOf(t, pool); status != claim.StatusDone {
		t.Fatalf("status after reconciliation = %s, want done: the probe found the execution", status)
	}
	if got := len(fake.Executions(t, correlationID)); got != 1 {
		t.Fatalf("downstream executed %d times, want 1: reconciliation re-executed the write", got)
	}
}

var idemioProcess *exec.Cmd

func startIdemio(t *testing.T, databaseURL, downstreamURL string) string {
	t.Helper()

	binary := filepath.Join(t.TempDir(), "idemio")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, "github.com/trnahnh/idemio/cmd/idemio")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build idemio: %v: %s", err, output)
	}

	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(),
		"IDEMIO_DATABASE_URL="+databaseURL,
		"IDEMIO_DOWNSTREAM_BASE_URL="+downstreamURL,
		"IDEMIO_AUTH_MODE=trusted_header",
		"IDEMIO_LISTEN_ADDR=127.0.0.1:0",
		"IDEMIO_DOWNSTREAM_CONNECT_TIMEOUT_MS=300",
		"IDEMIO_DOWNSTREAM_TIMEOUT_MS=30000",
		"IDEMIO_RECONCILE_STALE_AFTER=10m",
		"IDEMIO_RECONCILE_INTERVAL=30s",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start idemio: %v", err)
	}
	idemioProcess = cmd
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if address, ok := strings.CutPrefix(scanner.Text(), "listen="); ok {
			return address
		}
	}
	t.Fatal("idemio never reported a listen address")
	return ""
}

func killIdemio(t *testing.T) {
	t.Helper()

	if err := idemioProcess.Process.Kill(); err != nil {
		t.Fatalf("kill idemio: %v", err)
	}
	idemioProcess.Wait()
}

func writeTo(t *testing.T, address string) {
	body := `{"agent_id":"` + agentID + `","resource_type":"invoice","resource_id":"` + resourceID +
		`","operation":"create_charge","payload":{"amount_cents":4200}}`

	req, err := http.NewRequest(http.MethodPost, "http://"+address+"/v1/writes", strings.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Idempotency-Key", keyA)
	req.Header.Set("X-Idemio-Agent-Id", agentID)
	req.Header.Set("X-Idemio-Role", "agent")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// Waiting on the downstream's ledger rather than on a timer: the kill has to land while the
// call is genuinely in flight, or the test proves nothing.
func waitForDownstreamCall(t *testing.T, fake *faketest.Fake, correlationID string) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if len(fake.Executions(t, correlationID)) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("downstream never received the call")
}
