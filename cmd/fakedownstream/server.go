package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

const (
	behaviorSucceed         = "succeed"
	behaviorBusinessFailure = "business-failure"
	behaviorHang            = "hang"
	behaviorSlow            = "slow"

	correlationHeader = "X-Idemio-Correlation-Id"
	hangCap           = 5 * time.Minute
	slowDelay         = 400 * time.Millisecond
)

type scripts struct {
	mu       sync.Mutex
	byResour map[string][]string
}

func newScripts() *scripts {
	return &scripts{byResour: make(map[string][]string)}
}

func (s *scripts) install(resourceID string, behaviors []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.byResour[resourceID] = behaviors
}

func (s *scripts) pop(resourceID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	queued := s.byResour[resourceID]
	if len(queued) == 0 {
		return behaviorSucceed
	}
	s.byResour[resourceID] = queued[1:]
	return queued[0]
}

type executeRequest struct {
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	Operation    string          `json:"operation"`
	Payload      json.RawMessage `json:"payload"`
}

type fake struct {
	ledger  *ledger
	scripts *scripts
}

func (f *fake) dataMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/execute", f.handleExecute)
	mux.HandleFunc("GET /probe", f.handleProbe)
	return mux
}

func (f *fake) controlMux(listener *switchableListener) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /probe", f.handleProbe)
	mux.HandleFunc("POST /control/script", f.handleScript)
	mux.HandleFunc("POST /control/listener", handleListener(listener))
	return mux
}

func (f *fake) handleExecute(w http.ResponseWriter, r *http.Request) {
	var req executeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed body"})
		return
	}

	behavior := f.scripts.pop(req.ResourceID)

	recorded, err := f.ledger.record(execution{
		CorrelationID: r.Header.Get(correlationHeader),
		ResourceType:  req.ResourceType,
		ResourceID:    req.ResourceID,
		Operation:     req.Operation,
		Behavior:      behavior,
		ReceivedAt:    time.Now().UTC(),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	switch behavior {
	case behaviorBusinessFailure:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"status": "rejected",
			"reason": "insufficient_funds",
		})
	case behaviorSlow:
		time.Sleep(slowDelay)
		writeJSON(w, http.StatusOK, map[string]any{
			"status":   "applied",
			"sequence": recorded.Sequence,
		})
	case behaviorHang:
		select {
		case <-r.Context().Done():
		case <-time.After(hangCap):
		}
	default:
		writeJSON(w, http.StatusOK, map[string]any{
			"status":   "applied",
			"sequence": recorded.Sequence,
		})
	}
}

func (f *fake) handleProbe(w http.ResponseWriter, r *http.Request) {
	found := f.ledger.byCorrelation(r.URL.Query().Get("correlation_id"))
	writeJSON(w, http.StatusOK, map[string]any{
		"count":      len(found),
		"executions": found,
	})
}

func (f *fake) handleScript(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ResourceID string   `json:"resource_id"`
		Behaviors  []string `json:"behaviors"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed body"})
		return
	}

	f.scripts.install(req.ResourceID, req.Behaviors)
	w.WriteHeader(http.StatusNoContent)
}

func handleListener(listener *switchableListener) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Up bool `json:"up"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed body"})
			return
		}

		var err error
		if req.Up {
			err = listener.up()
		} else {
			err = listener.down()
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
