package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trnahnh/idemio/internal/canonical"
	"github.com/trnahnh/idemio/internal/claim"
	"github.com/trnahnh/idemio/internal/config"
	"github.com/trnahnh/idemio/internal/downstream"
	"github.com/trnahnh/idemio/internal/resource"
	"github.com/trnahnh/idemio/internal/telemetry"
)

const (
	headerIdempotencyKey = "Idempotency-Key"
	retryAfter           = time.Second
)

type Server struct {
	cfg        config.Config
	pool       *pgxpool.Pool
	downstream *downstream.Client
	metrics    *telemetry.Metrics
	logger     *slog.Logger
}

func New(cfg config.Config, pool *pgxpool.Pool, client *downstream.Client,
	metrics *telemetry.Metrics, logger *slog.Logger) *Server {

	return &Server{cfg: cfg, pool: pool, downstream: client, metrics: metrics, logger: logger}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/writes", s.handleWrite)
	mux.HandleFunc("GET /v1/writes/{key}", s.handleRead)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("GET /metrics", s.metrics.Handler())
	return s.countResponses(mux)
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) countResponses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		recording := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recording, r)
		s.metrics.Responses.WithLabelValues(strconv.Itoa(recording.status)).Inc()
	})
}

type writeRequest struct {
	AgentID      string          `json:"agent_id"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	Operation    string          `json:"operation"`
	Payload      json.RawMessage `json:"payload"`
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request) {
	caller, ok := readIdentity(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "", "missing_identity", "")
		return
	}
	if caller.Role != RoleAgent {
		writeProblem(w, http.StatusForbidden, "", "role_insufficient", "")
		return
	}

	key := strings.ToLower(r.Header.Get(headerIdempotencyKey))
	if !isUUID(key) {
		writeProblem(w, http.StatusUnprocessableEntity, key, "malformed_idempotency_key",
			"Idempotency-Key must be a UUID.")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.PayloadBytes)

	var req writeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeProblem(w, http.StatusRequestEntityTooLarge, key, "payload_too_large", "")
			return
		}
		writeProblem(w, http.StatusBadRequest, key, "malformed_request", err.Error())
		return
	}
	if req.AgentID == "" || req.ResourceType == "" || req.ResourceID == "" ||
		req.Operation == "" || len(req.Payload) == 0 {
		writeProblem(w, http.StatusBadRequest, key, "missing_field", "")
		return
	}
	if req.AgentID != caller.AgentID {
		writeProblem(w, http.StatusForbidden, key, "agent_id_mismatch", "")
		return
	}

	operationClass, known := resource.ClassOf(req.ResourceType, req.Operation)
	if !known {
		writeProblem(w, http.StatusUnprocessableEntity, key, "unknown_operation",
			"No registered operation for this resource_type.")
		return
	}

	hash, err := canonical.Hash(canonical.Request{
		AgentID:      req.AgentID,
		Operation:    req.Operation,
		Payload:      req.Payload,
		ResourceID:   req.ResourceID,
		ResourceType: req.ResourceType,
	})
	if err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, key, "unhashable_request", err.Error())
		return
	}

	result, err := claim.Claim(r.Context(), s.pool, claim.Request{
		AgentID:        req.AgentID,
		Key:            key,
		RequestHash:    hash,
		ResourceType:   req.ResourceType,
		ResourceID:     req.ResourceID,
		Operation:      req.Operation,
		OperationClass: operationClass,
		Payload:        req.Payload,
	})
	if err != nil {
		s.logger.Error("claim failed", "agent_id", req.AgentID, "key", key, "error", err)
		writeProblem(w, http.StatusInternalServerError, key, "claim_failed", "")
		return
	}

	if result.Collided {
		s.metrics.ClaimCollisions.Inc()
	}

	switch result.Verdict {
	case claim.VerdictMismatch:
		s.metrics.HashMismatches.WithLabelValues(req.AgentID).Inc()
		writeProblem(w, http.StatusUnprocessableEntity, key, "request_hash_mismatch",
			"This key was previously used with a different request body.")
	case claim.VerdictExisting:
		s.writeExisting(w, key, result.Record)
	default:
		if result.Record.AttemptCount > 1 {
			s.metrics.Reclaims.Inc()
		}
		s.execute(r, w, key, req, operationClass, result)
	}
}

func (s *Server) execute(r *http.Request, w http.ResponseWriter, key string, req writeRequest,
	operationClass string, claimed claim.Result) {

	callCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()),
		s.cfg.DownstreamConnectTimeout+s.cfg.DownstreamTimeout)
	defer cancel()

	started := time.Now()
	outcome := s.downstream.Execute(callCtx, downstream.Request{
		AgentID:      req.AgentID,
		Key:          key,
		ResourceType: req.ResourceType,
		ResourceID:   req.ResourceID,
		Operation:    req.Operation,
		Payload:      req.Payload,
	})
	s.metrics.DownstreamDuration.
		WithLabelValues(outcome.Disposition.String()).
		Observe(time.Since(started).Seconds())
	s.metrics.ObserveResultSize(len(outcome.Result))

	status := statusFor(outcome.Disposition)
	s.metrics.Writes.WithLabelValues(string(status)).Inc()
	if !s.persistOutcome(r.Context(), req.AgentID, key, status, outcome) {
		s.logger.Error("outcome not recorded; key left pending for the reconciler",
			"agent_id", req.AgentID, "key", key, "disposition", outcome.Disposition.String())
	}

	s.logger.Info("write completed",
		"agent_id", req.AgentID, "key", key,
		"resource_type", req.ResourceType, "resource_id", req.ResourceID,
		"operation_class", operationClass, "status", string(status))

	switch outcome.Disposition {
	case downstream.Done:
		writeJSON(w, http.StatusCreated, map[string]any{
			"idempotency_key": key,
			"status":          string(claim.StatusDone),
			"result":          outcome.Result,
			"replayed":        false,
			"intent_id":       claimed.IntentID,
		})
	case downstream.Failed:
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"idempotency_key": key,
			"status":          string(claim.StatusFailed),
			"reason":          "downstream_unreachable",
			"retryable":       true,
		})
	default:
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"idempotency_key": key,
			"status":          string(claim.StatusIndeterminate),
			"reason":          "downstream_timeout_after_send",
			"retryable":       false,
			"detail":          "The write may or may not have been applied. Do not retry this key.",
		})
	}
}

func (s *Server) persistOutcome(ctx context.Context, agentID, key string,
	status claim.Status, outcome downstream.Outcome) bool {

	detached := context.WithoutCancel(ctx)

	backoff := 50 * time.Millisecond
	for attempt := 1; attempt <= s.cfg.OutcomeWriteAttempts; attempt++ {
		writeCtx, cancel := context.WithTimeout(detached, 2*time.Second)
		updated, err := claim.Complete(writeCtx, s.pool, agentID, key, status, outcome.Result, outcome.Detail)
		cancel()

		if err == nil {
			return updated
		}
		s.logger.Warn("recording outcome failed",
			"agent_id", agentID, "key", key, "attempt", attempt, "error", err)

		if attempt < s.cfg.OutcomeWriteAttempts {
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	return false
}

func (s *Server) writeExisting(w http.ResponseWriter, key string, record claim.Record) {
	switch record.Status {
	case claim.StatusDone:
		s.metrics.Replays.Inc()
		writeJSON(w, http.StatusOK, map[string]any{
			"idempotency_key": key,
			"status":          string(record.Status),
			"result":          record.Result,
			"replayed":        true,
		})
	case claim.StatusPending:
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
		w.Header().Set("Location", "/v1/writes/"+key)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"idempotency_key": key,
			"status":          string(record.Status),
			"retry_after_ms":  retryAfter.Milliseconds(),
		})
	case claim.StatusRejected:
		writeProblem(w, http.StatusConflict, key, "conflicting_write", record.OutcomeDetail)
	case claim.StatusIndeterminate:
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"idempotency_key": key,
			"status":          string(record.Status),
			"reason":          "indeterminate_outcome",
			"retryable":       false,
			"detail":          "The write may or may not have been applied. Do not retry this key.",
		})
	default:
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"idempotency_key": key,
			"status":          string(record.Status),
			"reason":          "downstream_unreachable",
			"retryable":       true,
		})
	}
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	caller, ok := readIdentity(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "", "missing_identity", "")
		return
	}

	key := strings.ToLower(r.PathValue("key"))
	if !isUUID(key) {
		writeProblem(w, http.StatusUnprocessableEntity, key, "malformed_idempotency_key", "")
		return
	}

	record, found, err := claim.Lookup(r.Context(), s.pool, caller.AgentID, key)
	if err != nil {
		s.logger.Error("lookup failed", "agent_id", caller.AgentID, "key", key, "error", err)
		writeProblem(w, http.StatusInternalServerError, key, "lookup_failed", "")
		return
	}
	if !found {
		writeProblem(w, http.StatusNotFound, key, "unknown_key", "")
		return
	}

	body := map[string]any{
		"idempotency_key": key,
		"status":          string(record.Status),
		"attempt_count":   record.AttemptCount,
	}
	if len(record.Result) > 0 {
		body["result"] = record.Result
	}
	if record.OutcomeDetail != "" {
		body["detail"] = record.OutcomeDetail
	}
	writeJSON(w, http.StatusOK, body)
}

func statusFor(disposition downstream.Disposition) claim.Status {
	switch disposition {
	case downstream.Done:
		return claim.StatusDone
	case downstream.Failed:
		return claim.StatusFailed
	default:
		return claim.StatusIndeterminate
	}
}

func writeProblem(w http.ResponseWriter, status int, key, reason, detail string) {
	body := map[string]any{"status": "rejected", "reason": reason}
	if key != "" {
		body["idempotency_key"] = key
	}
	if detail != "" {
		body["detail"] = detail
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
