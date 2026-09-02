package claim

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trnahnh/idemio/internal/conflict"
)

func resolveConflicts(ctx context.Context, tx pgx.Tx, req Request,
	intentID string, emittedAt time.Time) (Result, error) {

	candidates, err := conflictingIntents(ctx, tx, req, intentID)
	if err != nil {
		return Result{}, err
	}
	if len(candidates) == 0 {
		return Result{Verdict: VerdictClaimed}, nil
	}

	// Shadow mode observes without altering behaviour, so it neither rejects nor waits
	// (ADR-0013).
	if !req.Enforce {
		if _, err := recordConflicts(ctx, tx, req, intentID, candidates, ResolutionObserved); err != nil {
			return Result{}, err
		}
		return Result{Verdict: VerdictClaimed, Observed: len(candidates)}, nil
	}

	var others, same []intent
	for _, candidate := range candidates {
		if candidate.agentID == req.AgentID {
			same = append(same, candidate)
		} else {
			others = append(others, candidate)
		}
	}

	if len(others) > 0 {
		return reject(ctx, tx, req, intentID, emittedAt, others, candidates)
	}

	blocking, err := firstPending(ctx, tx, req.AgentID, same)
	if err != nil {
		return Result{}, err
	}
	if blocking != "" {
		return Result{Verdict: VerdictSerialize, Blocking: Blocking{AgentID: req.AgentID, Key: blocking}}, nil
	}

	if _, err := recordConflicts(ctx, tx, req, intentID, same, ResolutionSerialized); err != nil {
		return Result{}, err
	}
	return Result{Verdict: VerdictClaimed}, nil
}

func reject(ctx context.Context, tx pgx.Tx, req Request, intentID string, emittedAt time.Time,
	others, all []intent) (Result, error) {

	conflictID, err := recordConflicts(ctx, tx, req, intentID, all, ResolutionRejected)
	if err != nil {
		return Result{}, err
	}

	primary := others[0]
	detail := fmt.Sprintf("%s on %s:%s",
		conflict.Reason(req.Declared, primary.declared), req.ResourceType, req.ResourceID)

	body, err := json.Marshal(map[string]any{
		"idempotency_key":       req.Key,
		"status":                string(StatusRejected),
		"reason":                "conflicting_write",
		"detail":                detail,
		"conflicting_intent_id": primary.id,
		"conflict_id":           conflictID,
	})
	if err != nil {
		return Result{}, fmt.Errorf("encode rejection: %w", err)
	}

	// Read the body back as jsonb stored it, so the first 409 is byte-identical to every
	// replay of it rather than merely equivalent.
	var stored []byte
	if err := tx.QueryRow(ctx, rejectKey, req.AgentID, req.Key, body, detail).Scan(&stored); err != nil {
		return Result{}, fmt.Errorf("reject key: %w", err)
	}
	if _, err := tx.Exec(ctx, voidIntent, intentID, emittedAt); err != nil {
		return Result{}, fmt.Errorf("void rejected intent: %w", err)
	}

	return Result{
		Verdict: VerdictRejected,
		Record:  Record{Result: stored, OutcomeDetail: detail},
	}, nil
}

// A hot resource can hold hundreds of live intents inside its window, and pairing against
// every one of them makes conflict recording quadratic in the write rate. The verdict is
// still computed against the whole window; only the recording is capped.
const maxRecordedConflicts = 10

// The conflict_id returned is the one named in the 409 body: the pair an operator is most
// likely to want first. Candidates arrive most-recent-first, so the cap keeps the pairs an
// operator would look at anyway.
func recordConflicts(ctx context.Context, tx pgx.Tx, req Request, intentID string,
	candidates []intent, resolution Resolution) (string, error) {

	if len(candidates) > maxRecordedConflicts {
		candidates = candidates[:maxRecordedConflicts]
	}

	var primary string
	for _, candidate := range candidates {
		var conflictID string
		err := tx.QueryRow(ctx, insertConflict,
			intentID, candidate.id, req.AgentID, candidate.agentID,
			req.ResourceType, req.ResourceID,
			conflict.Reason(req.Declared, candidate.declared),
			string(resolution), req.ManifestVersion,
		).Scan(&conflictID)
		if err != nil {
			return "", fmt.Errorf("record conflict: %w", err)
		}
		if primary == "" {
			primary = conflictID
		}
	}
	return primary, nil
}

// A same-agent conflict waits only on a write still in flight. Once the other key is
// terminal there is nothing left to serialize behind, which is what stops the retry in
// ADR-0015 from looping.
func firstPending(ctx context.Context, tx pgx.Tx, agentID string, candidates []intent) (string, error) {
	keys := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		keys = append(keys, candidate.key)
	}

	rows, err := tx.Query(ctx, selectStatuses, agentID, keys)
	if err != nil {
		return "", fmt.Errorf("read conflicting key statuses: %w", err)
	}
	defer rows.Close()

	var pending string
	for rows.Next() {
		var key string
		var status Status
		if err := rows.Scan(&key, &status); err != nil {
			return "", fmt.Errorf("scan conflicting key status: %w", err)
		}
		if !status.terminal() && pending == "" {
			pending = key
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read conflicting key statuses: %w", err)
	}
	return pending, nil
}

const completeKey = `
	UPDATE idempotency_keys
	   SET status = $3::key_status, result = $4, result_ref = $5, outcome_detail = $6,
	       completed_at = now()
	 WHERE agent_id = $1 AND idempotency_key = $2::uuid AND status = 'pending'`

const voidLatestIntent = `
	WITH latest AS (
	    SELECT intent_id, emitted_at
	      FROM write_intents
	     WHERE agent_id = $1 AND idempotency_key = $2::uuid AND voided_at IS NULL
	     ORDER BY emitted_at DESC
	     LIMIT 1
	)
	UPDATE write_intents w SET voided_at = now()
	  FROM latest l
	 WHERE w.intent_id = l.intent_id AND w.emitted_at = l.emitted_at`

// A failed write provably never reached downstream business logic, so its intent must stop
// counting against the next writer (ADR-0015). Status and voiding commit together or not
// at all.
func Complete(ctx context.Context, pool *pgxpool.Pool, agentID, key string,
	status Status, result json.RawMessage, resultRef, detail string) (bool, error) {

	var storedResult, storedRef, storedDetail any
	if len(result) > 0 {
		storedResult = []byte(result)
	}
	if resultRef != "" {
		storedRef = resultRef
	}
	if detail != "" {
		storedDetail = detail
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin complete: %w", err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, completeKey, agentID, key, string(status),
		storedResult, storedRef, storedDetail)
	if err != nil {
		return false, fmt.Errorf("complete key: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}

	if status == StatusFailed {
		if _, err := tx.Exec(ctx, voidLatestIntent, agentID, key); err != nil {
			return false, fmt.Errorf("void failed intent: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit complete: %w", err)
	}
	return true, nil
}
