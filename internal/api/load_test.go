//go:build loadtest

package api_test

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/trnahnh/idemio/internal/correlation"
	"github.com/trnahnh/idemio/internal/faketest"
	"github.com/trnahnh/idemio/internal/resultstore"
	"github.com/trnahnh/idemio/internal/testdb"
)

const (
	defaultRate     = 200
	defaultDuration = 20 * time.Second
	defaultSpread   = 50
)

func setting(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return fallback
	}
	return value
}

func uuid(t *testing.T) string {
	t.Helper()

	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("random key: %v", err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80

	h := hex.EncodeToString(buf)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

type sample struct {
	took   time.Duration
	status int
}

// Open loop: requests are issued on a schedule that does not depend on how long responses
// take. A closed loop would send fewer requests as the system slowed, so the queue would
// never build and the percentiles would flatter the system exactly when it was struggling.
func TestWritePathUnderConcurrentLoad(t *testing.T) {
	rate := setting("IDEMIO_LOAD_RATE", defaultRate)
	duration := time.Duration(setting("IDEMIO_LOAD_SECONDS", int(defaultDuration.Seconds()))) * time.Second
	spread := setting("IDEMIO_LOAD_RESOURCES", defaultSpread)

	// The fake runs without its per-execution fsync here, or the ledger would be the
	// bottleneck under measurement rather than idemio.
	h := assemble(t, testdb.New(t), faketest.StartForLoad(t), 10*time.Second, 0,
		resultstore.New(nil, defaultInlineCap), defaultInlineCap)

	total := rate * int(duration.Seconds())
	keys := make([]string, total)
	for i := range keys {
		keys[i] = uuid(t)
	}

	var mu sync.Mutex
	samples := make([]sample, 0, total)

	var issued sync.WaitGroup
	interval := time.Second / time.Duration(rate)
	started := time.Now()

	for i := range total {
		due := started.Add(time.Duration(i) * interval)
		if wait := time.Until(due); wait > 0 {
			time.Sleep(wait)
		}

		issued.Add(1)
		go func(i int) {
			defer issued.Done()

			at := time.Now()
			resp := h.writeOp(t, agentID, keys[i], "invoice",
				fmt.Sprintf("inv_%d", i%spread), "create_charge", `{"amount_cents":4200}`)
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			mu.Lock()
			samples = append(samples, sample{took: time.Since(at), status: resp.StatusCode})
			mu.Unlock()
		}(i)
	}
	issued.Wait()

	elapsed := time.Since(started)
	byStatus := make(map[int]int)
	durations := make([]time.Duration, 0, len(samples))
	for _, s := range samples {
		byStatus[s.status]++
		durations = append(durations, s.took)
	}
	slices.Sort(durations)

	at := func(p float64) time.Duration {
		if len(durations) == 0 {
			return 0
		}
		return durations[int(float64(len(durations)-1)*p)].Round(time.Microsecond)
	}

	t.Logf("issued=%d over %s at a target of %d/s across %d resources",
		total, elapsed.Round(time.Millisecond), rate, spread)
	t.Logf("achieved=%.0f/s  p50=%s p95=%s p99=%s max=%s",
		float64(len(samples))/elapsed.Seconds(), at(0.50), at(0.95), at(0.99), at(1))
	t.Logf("statuses=%v", byStatus)

	// The reported latency is environment-dependent and is not asserted. The guarantee is
	// not: every key must have executed exactly once downstream, whatever the concurrency.
	assertExactlyOnce(t, h, keys)

	var pending int
	if err := h.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM idempotency_keys WHERE status = 'pending'").Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 0 {
		t.Errorf("%d keys are still pending after the run drained", pending)
	}
}

func assertExactlyOnce(t *testing.T, h *harness, keys []string) {
	t.Helper()

	executions := h.fake.Executions(t, "")
	counts := make(map[string]int, len(executions))
	for _, e := range executions {
		counts[e.CorrelationID]++
	}

	var missing, duplicated int
	for _, key := range keys {
		switch counts[correlation.ID(agentID, key)] {
		case 1:
		case 0:
			missing++
		default:
			duplicated++
		}
	}

	if duplicated > 0 {
		t.Fatalf("%d keys executed more than once downstream: the at-most-once guarantee "+
			"broke under concurrency, which is the only thing this test is really for", duplicated)
	}
	if missing > 0 {
		t.Errorf("%d keys never reached the downstream", missing)
	}
	if got := len(executions); got != len(keys)-missing {
		t.Errorf("ledger holds %d executions for %d keys", got, len(keys)-missing)
	}
}
