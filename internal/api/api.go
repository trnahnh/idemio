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
	"github.com/trnahnh/idemio/internal/latency"
	"github.com/trnahnh/idemio/internal/manifest"
	"github.com/trnahnh/idemio/internal/resultstore"
	"github.com/trnahnh/idemio/internal/telemetry"
)

const (
	headerIdempotencyKey = "Idempotency-Key"
	pendingPollFloor     = 20 * time.Millisecond
)

type Server struct {
	cfg        config.Config
	pool       *pgxpool.Pool
	downstream *downstream.Client
	manifests  *manifest.Store
	results    *resultstore.Store
	latency    *latency.Tracker
	metrics    *telemetry.Metrics
	logger     *slog.Logger
}

func New(cfg config.Config, pool *pgxpool.Pool, client *downstream.Client,
	manifests *manifest.Store, results *resultstore.Store, metrics *telemetry.Metrics,
	logger *slog.Logger) *Server {

	return &Server{
		cfg:        cfg,
		pool:       pool,
		downstream: client,
		manifests:  manifests,
		results:    results,
		latency:    latency.NewTracker(),
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

// The pattern, not the path: a per-resource label would put caller-controlled cardinality
// into a histogram.
func route(r *http.Request) string {
	if pattern := r.Pattern; pattern != "" {
		return pattern
	}
	return "unmatched"
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
		started := time.Now()
		next.ServeHTTP(recording, r)

		s.metrics.Responses.WithLabelValues(strconv.Itoa(recording.status)).Inc()
		s.metrics.RequestDuration.WithLabelValues(route(r)).Observe(time.Since(started).Seconds())
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
		s.writeExisting(r.Context(), w, key, req.ResourceType, s.awaitCompletion(r.Context(), result.Record))
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
	took := time.Since(started)
	s.metrics.DownstreamDuration.
		WithLabelValues(outcome.Disposition.String()).
		Observe(took.Seconds())
	s.metrics.ObserveResultSize(len(outcome.Result))

	// ADR-0004. A refused connection returns in about a millisecond and is not a service
	// latency, so it never feeds the advice.
	if outcome.Disposition != downstream.Failed {
		s.latency.Observe(req.ResourceType, took)
	}

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

	placeCtx, cancelPlace := context.WithTimeout(detached, 5*time.Second)
	placement := s.results.Place(placeCtx, agentID, key, outcome.Result)
	cancelPlace()

	switch {
	case placement.Offloaded:
		s.metrics.ResultsOffloaded.Inc()
	case placement.FellBack:
		s.metrics.OffloadFallbacks.Inc()
		s.logger.Warn("result could not be offloaded; stored inline over the cap rather than lost",
			"agent_id", agentID, "key", key, "bytes", len(outcome.Result))
	}

	backoff := 50 * time.Millisecond
	for attempt := 1; attempt <= s.cfg.OutcomeWriteAttempts; attempt++ {
		writeCtx, cancel := context.WithTimeout(detached, 2*time.Second)
		updated, err := claim.Complete(writeCtx, s.pool, agentID, key, status,
			placement.Inline, placement.Ref, outcome.Detail)
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

func (s *Server) writeExisting(ctx context.Context, w http.ResponseWriter, key, resourceType string,
	record claim.Record) {

	switch record.Status {
	case claim.StatusDone:
		result, ok := s.resolveResult(ctx, w, key, record)
		if !ok {
			return
		}
		s.metrics.Replays.Inc()
		writeJSON(w, http.StatusOK, map[string]any{
			"idempotency_key": key,
			"status":          string(record.Status),
			"result":          result,
			"replayed":        true,
		})
	case claim.StatusPending:
		wait := s.latency.RetryAfter(resourceType)
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(wait.Round(time.Second).Seconds()))))
		w.Header().Set("Location", "/v1/writes/"+key)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"idempotency_key": key,
			"status":          string(record.Status),
			"retry_after_ms":  wait.Milliseconds(),
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

// A result that was offloaded and cannot be fetched is not a missing write. The key is
// terminal and the write definitely happened, so this reports a failure of this layer —
// never anything from the not-executed family, which would be a lie about a write that ran.
func (s *Server) resolveResult(ctx context.Context, w http.ResponseWriter, key string,
	record claim.Record) (json.RawMessage, bool) {

	result, err := s.results.Resolve(ctx, record.Result, record.ResultRef)
	if err != nil {
		s.metrics.ResultFetchFailures.Inc()
		s.logger.Error("stored result could not be fetched", "key", key,
			"result_ref", record.ResultRef, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"idempotency_key": key,
			"status":          string(record.Status),
			"reason":          "result_unavailable",
			"retryable":       true,
			"detail": "The write completed and its outcome is recorded, but the stored result " +
				"could not be read. Retrying the same key replays it and cannot re-execute.",
		})
		return nil, false
	}
	return result, true
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

	result, ok := s.resolveResult(r.Context(), w, key, record)
	if !ok {
		return
	}

	body := map[string]any{
		"idempotency_key": key,
		"status":          string(record.Status),
		"attempt_count":   record.AttemptCount,
	}
	if len(result) > 0 {
		body["result"] = result
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
