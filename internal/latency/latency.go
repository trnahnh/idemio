package latency

import (
	"slices"
	"sync"
	"time"
)

const (
	window = 256

	Floor   = 50 * time.Millisecond
	Ceiling = 5 * time.Second
)

// ADR-0004: Retry-After is the p95 of recent downstream latency for the resource_type,
// clamped. Before there is anything to measure, advise the neutral default rather than the
// floor: a cold start is the worst moment to invite aggressive polling.
const Default = time.Second

type Tracker struct {
	mu      sync.Mutex
	samples map[string][]time.Duration
	next    map[string]int
}

func NewTracker() *Tracker {
	return &Tracker{
		samples: make(map[string][]time.Duration),
		next:    make(map[string]int),
	}
}

// Only calls that reached the downstream are observed. A refused connection returns in
// about a millisecond, and feeding those in would pull the advice toward the floor during
// an outage — telling every waiting client to poll hardest exactly when the downstream can
// least absorb it.
func (t *Tracker) Observe(resourceType string, took time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	held := t.samples[resourceType]
	if len(held) < window {
		t.samples[resourceType] = append(held, took)
		return
	}

	at := t.next[resourceType]
	held[at] = took
	t.next[resourceType] = (at + 1) % window
}

func (t *Tracker) RetryAfter(resourceType string) time.Duration {
	t.mu.Lock()
	held := slices.Clone(t.samples[resourceType])
	t.mu.Unlock()

	if len(held) == 0 {
		return Default
	}

	slices.Sort(held)
	at := int(float64(len(held)-1) * 0.95)

	return clamp(held[at])
}

func clamp(d time.Duration) time.Duration {
	if d < Floor {
		return Floor
	}
	if d > Ceiling {
		return Ceiling
	}
	return d
}
