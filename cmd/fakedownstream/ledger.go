package main

import (
	"encoding/json"
	"fmt"
	"os"
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

// The ledger is the sole oracle for what the downstream actually did. Appending to it is
// the execution, and it lives outside idemio's database on purpose: ADR-0012.
type ledger struct {
	mu      sync.Mutex
	file    *os.File
	records []execution
}

func openLedger(path string) (*ledger, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open ledger %s: %w", path, err)
	}
	return &ledger{file: file}, nil
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
	if err := l.file.Sync(); err != nil {
		return execution{}, fmt.Errorf("sync ledger: %w", err)
	}
	l.records = append(l.records, e)
	return e, nil
}

func (l *ledger) byCorrelation(id string) []execution {
	l.mu.Lock()
	defer l.mu.Unlock()

	var found []execution
	for _, e := range l.records {
		if id == "" || e.CorrelationID == id {
			found = append(found, e)
		}
	}
	return found
}

func (l *ledger) close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.file.Close()
}
