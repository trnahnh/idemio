package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"
)

// Alertmanager's last hop. The drill asserts against what arrived here rather than against
// Prometheus, because a rule that evaluates but never reaches a receiver pages nobody.
type sink struct {
	mu       sync.Mutex
	received []notification
}

type notification struct {
	Receiver string  `json:"receiver"`
	Status   string  `json:"status"`
	Alerts   []alert `json:"alerts"`
}

type alert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
}

func (s *sink) receive(w http.ResponseWriter, r *http.Request) {
	var body notification
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "malformed notification", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.received = append(s.received, body)
	s.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

func (s *sink) list(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"notifications": s.received})
}

func (s *sink) reset(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.received = nil
	s.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

func main() {
	addr := flag.String("addr", ":9098", "address to listen on")
	flag.Parse()

	s := &sink{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /alerts", s.receive)
	mux.HandleFunc("GET /received", s.list)
	mux.HandleFunc("POST /reset", s.reset)

	if err := http.ListenAndServe(*addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
