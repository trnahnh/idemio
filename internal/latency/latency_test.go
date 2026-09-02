package latency_test

import (
	"testing"
	"time"

	"github.com/trnahnh/idemio/internal/latency"
)

func TestWithNoSamplesTheAdviceIsTheNeutralDefault(t *testing.T) {
	if got := latency.NewTracker().RetryAfter("invoice"); got != latency.Default {
		t.Fatalf("RetryAfter = %s, want %s: a cold start is the worst moment to invite "+
			"aggressive polling", got, latency.Default)
	}
}

func TestTheAdviceTracksTheTail(t *testing.T) {
	tracker := latency.NewTracker()

	// 90/10 rather than 95/5: at exactly 95% the p95 sits on the boundary and either
	// answer is defensible, which would make this test agree with any implementation.
	for range 90 {
		tracker.Observe("invoice", 200*time.Millisecond)
	}
	for range 10 {
		tracker.Observe("invoice", 2*time.Second)
	}

	got := tracker.RetryAfter("invoice")
	if got < time.Second {
		t.Fatalf("RetryAfter = %s; the p95 should reflect the slow tail, not the median", got)
	}
}

func TestTheAdviceIsClamped(t *testing.T) {
	fast := latency.NewTracker()
	for range 10 {
		fast.Observe("invoice", time.Millisecond)
	}
	if got := fast.RetryAfter("invoice"); got != latency.Floor {
		t.Errorf("RetryAfter = %s, want the %s floor", got, latency.Floor)
	}

	slow := latency.NewTracker()
	for range 10 {
		slow.Observe("invoice", time.Minute)
	}
	if got := slow.RetryAfter("invoice"); got != latency.Ceiling {
		t.Errorf("RetryAfter = %s, want the %s ceiling", got, latency.Ceiling)
	}
}

// The window is declared per resource_type, so a slow integration must not make a fast one
// advise its clients to back off.
func TestResourceTypesDoNotShareAWindow(t *testing.T) {
	tracker := latency.NewTracker()

	for range 20 {
		tracker.Observe("invoice", 100*time.Millisecond)
		tracker.Observe("subscription", 3*time.Second)
	}

	if invoice, subscription := tracker.RetryAfter("invoice"), tracker.RetryAfter("subscription"); invoice >= subscription {
		t.Fatalf("invoice advises %s and subscription %s; the windows are shared", invoice, subscription)
	}
}

// The window is bounded, so a downstream that recovers stops being judged by its worst hour.
func TestOldSamplesFallOutOfTheWindow(t *testing.T) {
	tracker := latency.NewTracker()

	for range 300 {
		tracker.Observe("invoice", 4*time.Second)
	}
	for range 300 {
		tracker.Observe("invoice", 100*time.Millisecond)
	}

	if got := tracker.RetryAfter("invoice"); got > time.Second {
		t.Fatalf("RetryAfter = %s after the downstream recovered; the window is not bounded", got)
	}
}

func TestObservationsAreSafeUnderConcurrency(t *testing.T) {
	tracker := latency.NewTracker()

	done := make(chan struct{})
	for range 8 {
		go func() {
			for range 500 {
				tracker.Observe("invoice", 100*time.Millisecond)
				tracker.RetryAfter("invoice")
			}
			done <- struct{}{}
		}()
	}
	for range 8 {
		<-done
	}
}
