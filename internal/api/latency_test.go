//go:build latency

package api_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"
)

const (
	samples      = 300
	warmup       = 30
	budgetP50    = 15 * time.Millisecond
	budgetP99    = 60 * time.Millisecond
	payloadBody  = `{"amount_cents":4200,"currency":"EUR"}`
	executeRoute = "/v1/execute"
)

func randomKey(t *testing.T) string {
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

func percentile(durations []time.Duration, p float64) time.Duration {
	sorted := slices.Clone(durations)
	slices.Sort(sorted)

	index := int(float64(len(sorted)-1) * p)
	return sorted[index]
}

// Two shapes, because they measure different things. Spread traffic is the ordinary write
// path. A single resource is the worst case ADR-0015 names explicitly: every write
// serializes on one lock and every write sees a full conflict window.
func timeThroughIdemio(t *testing.T, h *harness, count int, spread bool) []time.Duration {
	t.Helper()

	var timings []time.Duration
	for i := range count {
		key := randomKey(t)
		resource := resourceID
		if spread {
			resource = fmt.Sprintf("inv_%d", i)
		}

		started := time.Now()
		resp := h.writeOp(t, agentID, key, "invoice", resource, "create_charge", payloadBody)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		timings = append(timings, time.Since(started))

		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201", resp.StatusCode)
		}
	}
	return timings
}

func timeDirectToDownstream(t *testing.T, h *harness, count int) []time.Duration {
	t.Helper()

	body := `{"resource_type":"invoice","resource_id":"` + resourceID +
		`","operation":"create_charge","payload":` + payloadBody + `}`

	var timings []time.Duration
	for range count {
		started := time.Now()
		req, err := http.NewRequest(http.MethodPost, h.fake.DataURL+executeRoute,
			bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("X-Idemio-Correlation-Id", randomKey(t))

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("direct call: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		timings = append(timings, time.Since(started))
	}
	return timings
}

// ROADMAP exit criterion 5. This measures overhead against the fake on one machine, which is
// not the "real volume" the criterion asks for. It is the floor: if the budget is already
// missed here, it will not be met under load.
func TestWritePathOverheadIsWithinBudget(t *testing.T) {
	h := newHarness(t, 5*time.Second)

	timeThroughIdemio(t, h, warmup, true)
	timeDirectToDownstream(t, h, warmup)

	throughIdemio := timeThroughIdemio(t, h, samples, true)
	direct := timeDirectToDownstream(t, h, samples)

	overheadP50 := percentile(throughIdemio, 0.50) - percentile(direct, 0.50)
	overheadP99 := percentile(throughIdemio, 0.99) - percentile(direct, 0.99)

	t.Logf("n=%d  through idemio p50=%s p99=%s | direct p50=%s p99=%s | overhead p50=%s p99=%s",
		samples,
		percentile(throughIdemio, 0.50).Round(time.Microsecond),
		percentile(throughIdemio, 0.99).Round(time.Microsecond),
		percentile(direct, 0.50).Round(time.Microsecond),
		percentile(direct, 0.99).Round(time.Microsecond),
		overheadP50.Round(time.Microsecond),
		overheadP99.Round(time.Microsecond))

	// Only the p50 is asserted. At 300 samples the p99 is the third-worst observation, so a
	// single scheduling hiccup on a shared machine dominates it — a CI run once measured a
	// 20ms p99 on a *direct* localhost call whose p50 was 576µs. A threshold that fails on
	// the runner's mood teaches people to ignore a red build, which costs more than the
	// check is worth. The p50 still catches the regression that matters here: an extra
	// round trip on the claim path moves it immediately.
	//
	// The p99 budget is measured by the load harness instead (`make load`), where the tail
	// is drawn from thousands of samples and is run deliberately on a known machine.
	if overheadP50 > budgetP50 {
		t.Errorf("p50 overhead %s exceeds the %s budget", overheadP50, budgetP50)
	}
	if overheadP99 > budgetP99 {
		t.Logf("p99 overhead %s is above the %s budget; not a failure here, but check "+
			"`make load` on a quiet machine before dismissing it", overheadP99, budgetP99)
	}
}

// Not a budget check. ADR-0015 accepts that a single hot resource_id serializes and is
// throughput-limited by design; this measures how expensive that actually is, so the
// ceiling is a number rather than a warning.
func TestHotResourceCostIsMeasured(t *testing.T) {
	h := newHarness(t, 5*time.Second)

	timeThroughIdemio(t, h, warmup, false)
	hot := timeThroughIdemio(t, h, samples, false)
	direct := timeDirectToDownstream(t, h, samples)

	t.Logf("n=%d on one resource_id  p50=%s p99=%s | direct p50=%s | overhead p50=%s p99=%s",
		samples,
		percentile(hot, 0.50).Round(time.Microsecond),
		percentile(hot, 0.99).Round(time.Microsecond),
		percentile(direct, 0.50).Round(time.Microsecond),
		(percentile(hot, 0.50) - percentile(direct, 0.50)).Round(time.Microsecond),
		(percentile(hot, 0.99) - percentile(direct, 0.99)).Round(time.Microsecond))
}
