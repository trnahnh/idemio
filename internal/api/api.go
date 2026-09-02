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
	"github.com/trnahnh/idemio/internal/manifest"
	"github.com/trnahnh/idemio/internal/telemetry"
)

const (
	headerIdempotencyKey = "Idempotency-Key"
	retryAfter           = time.Second
	pendingPollFloor     = 20 * time.Millisecond
)

type Server struct {
	cfg        config.Config
	pool       *pgxpool.Pool
	downstream *downstream.Client
	manifests  *manifest.Store
	metrics    *telemetry.Metrics
	logger     *slog.Logger
}

func New(cfg config.Config, pool *pgxpool.Pool, client *downstream.Client,
	manifests *manifest.Store, metrics *telemetry.Metrics, logger *slog.Logger) *Server {

	return &Server{
		cfg:        cfg,
		pool:       pool,
		downstream: client,
		manifests:  manifests,
		metrics:    metrics,
		logger:     logger,
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/writes", s.handleWrite)
	mux.HandleFunc("GET /v1/writes/{key}", s.handleRead)
	mux.HandleFunc("GET /v1/resources/{resource_type}/{resource_id}/intents", s.handleIntents)
	mux.HandleFunc("GET /v1/conflicts", s.handleConflicts)
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

	snapshot := s.manifests.Current()
	definition, declared, known := lookupOperation(snapshot, req.ResourceType, req.Operation)
	if !known {
		writeProblem(w, http.StatusUnprocessableEntity, key, "unknown_operation",
			"No manifest declares this operation for this resource_type. A write whose "+
				"downstream responses cannot be classified is never admitted.")
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

	result, err := s.claimWithSerialization(r.Context(), claim.Request{
		AgentID:         req.AgentID,
		Key:             key,
		RequestHash:     hash,
		ResourceType:    req.ResourceType,
		ResourceID:      req.ResourceID,
		Operation:       req.Operation,
		Declared:        declared,
		Payload:         req.Payload,
		Window:          definition.ConflictWindow,
		Enforce:         definition.Enforce,
		ManifestVersion: snapshot.Version(),
		LockTimeout:     s.cfg.ConflictLockTimeout,
	})
	if err != nil {
		s.logger.Error("claim failed", "agent_id", req.AgentID, "key", key, "error", err)
		writeProblem(w, http.StatusInternalServerError, key, "claim_failed", "")
		return
	}

	if result.Collided {
		s.metrics.ClaimCollisions.Inc()
	}
	s.metrics.LockWait.WithLabelValues(req.ResourceType).Observe(result.LockWait.Seconds())
	if result.Observed > 0 {
		s.metrics.Conflicts.WithLabelValues(req.ResourceType, string(claim.ResolutionObserved)).
			Add(float64(result.Observed))
	}

	switch result.Verdict {
	case claim.VerdictMismatch:
		s.metrics.HashMismatches.WithLabelValues(req.ResourceType).Inc()
		writeProblem(w, http.StatusUnprocessableEntity, key, "request_hash_mismatch",
			"This key was previously used with a different request body.")
	case claim.VerdictExisting:
		s.writeExisting(w, key, s.awaitCompletion(r.Context(), result.Record))
	case claim.VerdictRejected:
		s.metrics.Conflicts.WithLabelValues(req.ResourceType, string(claim.ResolutionRejected)).Inc()
		s.metrics.Writes.WithLabelValues(string(claim.StatusRejected)).Inc()
		writeRaw(w, http.StatusConflict, result.Record.Result)
	case claim.VerdictLockTimeout:
		s.metrics.LockTimeouts.WithLabelValues(req.ResourceType).Inc()
		writeNotExecuted(w, key, "resource_busy")
	case claim.VerdictSerializeTimeout:
		writeNotExecuted(w, key, "serialization_wait_expired")
	default:
		if result.Record.AttemptCount > 1 {
			s.metrics.ReclaimAttempts.Observe(float64(result.Record.AttemptCount))
		}
		s.execute(r, w, key, req, definition, result)
	}
}

func lookupOperation(snapshot *manifest.Snapshot, resourceType, operation string) (
	manifest.Definition, manifest.Operation, bool) {

	definition, ok := snapshot.Lookup(resourceType)
	if !ok {
		return manifest.Definition{}, manifest.Operation{}, false
	}
	declared, ok := definition.Operations[operation]
	return definition, declared, ok
}

// ADR-0015: a same-agent conflict waits for the earlier write to finish and then re-runs
// the whole transaction from the lock. The lock is not held while waiting.
func (s *Server) claimWithSerialization(ctx context.Context, req claim.Request) (claim.Result, error) {
	deadline := time.Now().Add(s.cfg.SerializeWait())

	for {
		result, err := claim.Claim(ctx, s.pool, req)
		if err != nil || result.Verdict != claim.VerdictSerialize {
			return result, err
		}
		if !s.waitForTerminal(ctx, result.Blocking, deadline) {
			s.metrics.SerializationWaits.WithLabelValues("timeout").Inc()
			result.Verdict = claim.VerdictSerializeTimeout
			return result, nil
		}
		s.metrics.SerializationWaits.WithLabelValues("resolved").Inc()
	}
}

func (s *Server) waitForTerminal(ctx context.Context, blocking claim.Blocking, deadline time.Time) bool {
	for time.Now().Before(deadline) {
		record, found, err := claim.Lookup(ctx, s.pool, blocking.AgentID, blocking.Key)
		if err != nil {
			return false
		}
		if !found || record.Status != claim.StatusPending {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(pendingPollFloor):
		}
	}
	return false
}

func (s *Server) execute(r *http.Request, w http.ResponseWriter, key string, req writeRequest,
	definition manifest.Definition, claimed claim.Result) {

	callCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()),
		s.cfg.DownstreamConnectTimeout+s.cfg.DownstreamTimeout)
	defer cancel()

	started := time.Now()
	outcome := s.downstream.Execute(callCtx, downstream.Request{
		AgentID:        req.AgentID,
		Key:            key,
		ResourceType:   req.ResourceType,
		ResourceID:     req.ResourceID,
		Operation:      req.Operation,
		Payload:        req.Payload,
		Classification: definition.Errors,
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
		"operation_class", definition.Operations[req.Operation].Class, "status", string(status))

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

func (s *Server) awaitCompletion(ctx context.Context, record claim.Record) claim.Record {
	if record.Status != claim.StatusPending || s.cfg.ClaimPendingWait <= 0 {
		return record
	}

	deadline := time.Now().Add(s.cfg.ClaimPendingWait)
	interval := s.cfg.ClaimPendingWait / 20
	if interval < pendingPollFloor {
		interval = pendingPollFloor
	}

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return record
		case <-time.After(interval):
		}

		current, found, err := claim.Lookup(ctx, s.pool, record.AgentID, record.Key)
		if err != nil || !found {
			return record
		}
		if current.Status != claim.StatusPending {
			return current
		}
	}
	return record
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
		if len(record.Result) == 0 {
			writeProblem(w, http.StatusConflict, key, "conflicting_write", record.OutcomeDetail)
			return
		}
		writeRaw(w, http.StatusConflict, record.Result)
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

func writeNotExecuted(w http.ResponseWriter, key, reason string) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"idempotency_key": key,
		"status":          string(claim.StatusFailed),
		"reason":          reason,
		"retryable":       true,
		"detail":          "No claim was made and nothing was sent downstream. Retry the same key.",
	})
}

func writeRaw(w http.ResponseWriter, status int, body json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
