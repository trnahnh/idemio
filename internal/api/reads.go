package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	defaultLimit = 100
	maxLimit     = 1000

	headerReason = "X-Idemio-Reason"
)

type window struct {
	since  time.Time
	until  time.Time
	limit  int
	cursor *cursor
}

type cursor struct {
	At time.Time
	ID string
}

func encodeCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
}

func decodeCursor(raw string) (*cursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("cursor is not valid base64url")
	}
	at, id, ok := strings.Cut(string(decoded), "|")
	if !ok {
		return nil, fmt.Errorf("cursor is malformed")
	}
	parsed, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return nil, fmt.Errorf("cursor carries no timestamp")
	}
	if !isUUID(strings.ToLower(id)) {
		return nil, fmt.Errorf("cursor carries no identifier")
	}
	return &cursor{At: parsed, ID: id}, nil
}

// ADR-0017: the range is required and capped. A default window would let a caller believe
// they searched a period they did not.
func (s *Server) readWindow(r *http.Request) (window, string) {
	query := r.URL.Query()

	raw := query.Get("since")
	if raw == "" {
		return window{}, "since is required; an unbounded read scans every retained partition"
	}
	since, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return window{}, "since must be an RFC 3339 timestamp"
	}

	until := time.Now()
	if raw := query.Get("until"); raw != "" {
		until, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return window{}, "until must be an RFC 3339 timestamp"
		}
	}
	if !until.After(since) {
		return window{}, "until must be after since"
	}
	if until.Sub(since) > s.cfg.ReadMaxSpan {
		return window{}, fmt.Sprintf("the range spans %s; the maximum is %s. Page with cursor "+
			"rather than widening the window", until.Sub(since).Round(time.Hour), s.cfg.ReadMaxSpan)
	}

	limit := defaultLimit
	if raw := query.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxLimit {
			return window{}, fmt.Sprintf("limit must be between 1 and %d", maxLimit)
		}
	}

	result := window{since: since, until: until, limit: limit}
	if raw := query.Get("cursor"); raw != "" {
		parsed, err := decodeCursor(raw)
		if err != nil {
			return window{}, err.Error()
		}
		result.cursor = parsed
	}
	return result, ""
}

func (s *Server) readerIdentity(w http.ResponseWriter, r *http.Request) (identity, bool) {
	caller, ok := readIdentity(r)
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "", "missing_identity", "")
		return identity{}, false
	}
	if caller.Role != RoleOperator && caller.Role != RoleInvestigator {
		writeProblem(w, http.StatusForbidden, "", "role_insufficient",
			"Read endpoints require operator or investigator.")
		return identity{}, false
	}
	return caller, true
}

// Redaction is two queries rather than one query and a filter, so a caller without the
// role never causes the column to be read (ADR-0011).
const selectIntentsRedacted = `
	SELECT intent_id::text, agent_id, idempotency_key::text, operation,
	       operation_class::text, scope_selector, emitted_at, voided_at, NULL::jsonb
	  FROM write_intents
	 WHERE resource_type = $1 AND resource_id = $2
	   AND emitted_at >= $3 AND emitted_at <= $4
	   AND ($5::timestamptz IS NULL OR (emitted_at, intent_id) < ($5, $6::uuid))
	 ORDER BY emitted_at DESC, intent_id DESC
	 LIMIT $7`

const selectIntentsWithPayload = `
	SELECT intent_id::text, agent_id, idempotency_key::text, operation,
	       operation_class::text, scope_selector, emitted_at, voided_at, payload
	  FROM write_intents
	 WHERE resource_type = $1 AND resource_id = $2
	   AND emitted_at >= $3 AND emitted_at <= $4
	   AND ($5::timestamptz IS NULL OR (emitted_at, intent_id) < ($5, $6::uuid))
	 ORDER BY emitted_at DESC, intent_id DESC
	 LIMIT $7`

type intentRow struct {
	IntentID       string          `json:"intent_id"`
	AgentID        string          `json:"agent_id"`
	IdempotencyKey string          `json:"idempotency_key"`
	Operation      string          `json:"operation"`
	OperationClass string          `json:"operation_class"`
	ScopeSelector  []string        `json:"scope_selector"`
	EmittedAt      time.Time       `json:"emitted_at"`
	Voided         bool            `json:"voided"`
	Payload        json.RawMessage `json:"payload"`
	Redacted       bool            `json:"payload_redacted"`
}

func (s *Server) handleIntents(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.readerIdentity(w, r)
	if !ok {
		return
	}

	bounds, problem := s.readWindow(r)
	if problem != "" {
		writeProblem(w, http.StatusBadRequest, "", "invalid_range", problem)
		return
	}

	unredacted, reason, ok := s.payloadRequest(w, r, caller)
	if !ok {
		return
	}

	statement := selectIntentsRedacted
	if unredacted {
		statement = selectIntentsWithPayload
	}

	resourceType := r.PathValue("resource_type")
	resourceID := r.PathValue("resource_id")

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		s.logger.Error("begin intent read", "error", err)
		writeProblem(w, http.StatusInternalServerError, "", "read_failed", "")
		return
	}
	defer tx.Rollback(r.Context())

	rows, err := s.scanIntents(r.Context(), tx, statement, resourceType, resourceID, bounds, unredacted)
	if err != nil {
		s.logger.Error("read intents", "error", err)
		writeProblem(w, http.StatusInternalServerError, "", "read_failed", "")
		return
	}

	page, hasMore, next := paginate(rows, bounds.limit, func(row intentRow) string {
		return encodeCursor(row.EmittedAt, row.IntentID)
	})

	if unredacted {
		ids := make([]string, 0, len(page))
		for _, row := range page {
			ids = append(ids, row.IntentID)
		}
		if err := recordPayloadAccess(r.Context(), tx, auditEntry{
			principal: caller.AgentID,
			role:      caller.Role,
			endpoint:  r.URL.Path,
			params:    r.URL.Query(),
			ids:       ids,
			reason:    reason,
		}); err != nil {
			s.logger.Error("payload access audit failed; returning no payloads", "error", err)
			writeProblem(w, http.StatusServiceUnavailable, "", "audit_unavailable",
				"Payloads are not returned when the access cannot be audited.")
			return
		}
	}

	if err := tx.Commit(r.Context()); err != nil {
		s.logger.Error("commit intent read", "error", err)
		writeProblem(w, http.StatusInternalServerError, "", "read_failed", "")
		return
	}

	body := map[string]any{
		"resource_type": resourceType,
		"resource_id":   resourceID,
		"intents":       page,
		"has_more":      hasMore,
	}
	if hasMore {
		body["next_cursor"] = next
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) scanIntents(ctx context.Context, tx pgx.Tx, statement, resourceType,
	resourceID string, bounds window, unredacted bool) ([]intentRow, error) {

	var cursorAt any
	var cursorID any
	if bounds.cursor != nil {
		cursorAt = bounds.cursor.At
		cursorID = bounds.cursor.ID
	}

	rows, err := tx.Query(ctx, statement, resourceType, resourceID,
		bounds.since, bounds.until, cursorAt, cursorID, bounds.limit+1)
	if err != nil {
		return nil, fmt.Errorf("query intents: %w", err)
	}
	defer rows.Close()

	var found []intentRow
	for rows.Next() {
		var row intentRow
		var voidedAt *time.Time
		var payload []byte

		if err := rows.Scan(&row.IntentID, &row.AgentID, &row.IdempotencyKey, &row.Operation,
			&row.OperationClass, &row.ScopeSelector, &row.EmittedAt, &voidedAt, &payload); err != nil {
			return nil, fmt.Errorf("scan intent: %w", err)
		}
		row.Voided = voidedAt != nil
		row.Payload = json.RawMessage(payload)
		row.Redacted = !unredacted
		found = append(found, row)
	}
	return found, rows.Err()
}

type conflictRow struct {
	ConflictID      string    `json:"conflict_id"`
	IntentIDA       string    `json:"intent_id_a"`
	IntentIDB       string    `json:"intent_id_b"`
	AgentIDA        string    `json:"agent_id_a"`
	AgentIDB        string    `json:"agent_id_b"`
	ResourceType    string    `json:"resource_type"`
	ResourceID      string    `json:"resource_id"`
	Reason          string    `json:"reason"`
	Resolution      string    `json:"resolution"`
	ManifestVersion string    `json:"manifest_version"`
	DetectedAt      time.Time `json:"detected_at"`
}

const selectConflicts = `
	SELECT conflict_id::text, intent_id_a::text, intent_id_b::text, agent_id_a, agent_id_b,
	       resource_type, resource_id, reason, resolution::text,
	       coalesce(manifest_version, ''), detected_at
	  FROM conflicts
	 WHERE detected_at >= $1 AND detected_at <= $2
	   AND ($3::text IS NULL OR agent_id_a = $3 OR agent_id_b = $3)
	   AND ($4::text IS NULL OR resource_type = $4)
	   AND ($5::text IS NULL OR resolution::text = $5)
	   AND ($6::timestamptz IS NULL OR (detected_at, conflict_id) < ($6, $7::uuid))
	 ORDER BY detected_at DESC, conflict_id DESC
	 LIMIT $8`

func (s *Server) handleConflicts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.readerIdentity(w, r); !ok {
		return
	}

	bounds, problem := s.readWindow(r)
	if problem != "" {
		writeProblem(w, http.StatusBadRequest, "", "invalid_range", problem)
		return
	}
	if r.URL.Query().Get("include") != "" {
		writeProblem(w, http.StatusBadRequest, "", "nothing_to_include",
			"Conflict records carry no payload; read the intents endpoint for request bodies.")
		return
	}

	query := r.URL.Query()
	var cursorAt, cursorID any
	if bounds.cursor != nil {
		cursorAt = bounds.cursor.At
		cursorID = bounds.cursor.ID
	}

	rows, err := s.pool.Query(r.Context(), selectConflicts,
		bounds.since, bounds.until,
		optional(query.Get("agent_id")), optional(query.Get("resource_type")),
		optional(query.Get("resolution")), cursorAt, cursorID, bounds.limit+1)
	if err != nil {
		s.logger.Error("read conflicts", "error", err)
		writeProblem(w, http.StatusInternalServerError, "", "read_failed", "")
		return
	}
	defer rows.Close()

	var found []conflictRow
	for rows.Next() {
		var row conflictRow
		if err := rows.Scan(&row.ConflictID, &row.IntentIDA, &row.IntentIDB,
			&row.AgentIDA, &row.AgentIDB, &row.ResourceType, &row.ResourceID,
			&row.Reason, &row.Resolution, &row.ManifestVersion, &row.DetectedAt); err != nil {
			s.logger.Error("scan conflict", "error", err)
			writeProblem(w, http.StatusInternalServerError, "", "read_failed", "")
			return
		}
		found = append(found, row)
	}
	if err := rows.Err(); err != nil {
		s.logger.Error("read conflicts", "error", err)
		writeProblem(w, http.StatusInternalServerError, "", "read_failed", "")
		return
	}

	page, hasMore, next := paginate(found, bounds.limit, func(row conflictRow) string {
		return encodeCursor(row.DetectedAt, row.ConflictID)
	})

	body := map[string]any{"conflicts": page, "has_more": hasMore}
	if hasMore {
		body["next_cursor"] = next
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) payloadRequest(w http.ResponseWriter, r *http.Request, caller identity) (bool, string, bool) {
	if r.URL.Query().Get("include") != "payload" {
		return false, "", true
	}
	if caller.Role != RoleInvestigator {
		writeProblem(w, http.StatusForbidden, "", "role_insufficient",
			"Unredacted payloads require the investigator role.")
		return false, "", false
	}
	reason := strings.TrimSpace(r.Header.Get(headerReason))
	if reason == "" {
		writeProblem(w, http.StatusBadRequest, "", "reason_required",
			"Reading payloads requires "+headerReason+". An audit trail of unexplained "+
				"accesses records that a payload was read but not why.")
		return false, "", false
	}
	return true, reason, true
}

func paginate[T any](rows []T, limit int, key func(T) string) ([]T, bool, string) {
	if len(rows) <= limit {
		return rows, false, ""
	}
	page := rows[:limit]
	return page, true, key(page[len(page)-1])
}

func optional(value string) any {
	if value == "" {
		return nil
	}
	return value
}
