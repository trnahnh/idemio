package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"
)

type execution struct {
	Sequence      int       `json:"sequence"`
	CorrelationID string    `json:"correlation_id"`
	ResourceType  string    `json:"resource_type"`
	ResourceID    string    `json:"resource_id"`
	Operation     string    `json:"operation"`
	Behavior      string    `json:"behavior"`
	ReceivedAt    time.Time `json:"received_at"`
}

type ledger struct {
	mu      sync.Mutex
	file    *os.File
	records []execution
	byID    map[string][]execution
	durable bool
}

// The per-execution fsync is what makes this ledger survive a SIGKILL, which is the entire
// reason it can be trusted as the oracle. It is also a hard throughput ceiling, so a load
// run would measure this file rather than idemio. Load mode trades the crash guarantee for
// throughput, and no correctness test may use it.
func openLedger(path string, durable bool) (*ledger, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open ledger %s: %w", path, err)
	}
	return &ledger{file: file, byID: make(map[string][]execution), durable: durable}, nil
}

func (l *ledger) record(e execution) (execution, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	e.Sequence = len(l.records) + 1
	line, err := json.Marshal(e)
	if err != nil {
		return execution{}, fmt.Errorf("encode execution: %w", err)
	}
	if _, err := l.file.Write(append(line, '\n')); err != nil {
		return execution{}, fmt.Errorf("append to ledger: %w", err)
	}
	if l.durable {
		if err := l.file.Sync(); err != nil {
			return execution{}, fmt.Errorf("sync ledger: %w", err)
		}
	}
	l.records = append(l.records, e)
	l.byID[e.CorrelationID] = append(l.byID[e.CorrelationID], e)
	return e, nil
}

// Indexed rather than scanned: a load run probes once per key, and a linear scan would make
// verifying N writes cost O(N^2) in the fixture rather than in the system under test.
func (l *ledger) byCorrelation(id string) []execution {
	l.mu.Lock()
	defer l.mu.Unlock()

	if id == "" {
		return slices.Clone(l.records)
	}
	return slices.Clone(l.byID[id])
}

func (l *ledger) close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.file.Close()
}
