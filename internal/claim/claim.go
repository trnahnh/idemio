package claim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Verdict string

const (
	VerdictClaimed  Verdict = "claimed"
	VerdictExisting Verdict = "existing"
	VerdictMismatch Verdict = "mismatch"
)

type Status string

const (
	StatusPending       Status = "pending"
	StatusDone          Status = "done"
	StatusFailed        Status = "failed"
	StatusIndeterminate Status = "indeterminate"
	StatusRejected      Status = "rejected"
)

type Request struct {
	AgentID        string
	Key            string
	RequestHash    string
	ResourceType   string
	ResourceID     string
	Operation      string
	OperationClass string
	Payload        json.RawMessage
}

type Record struct {
	AgentID       string
	Key           string
	RequestHash   string
	Status        Status
	Result        json.RawMessage
	OutcomeDetail string
	AttemptCount  int
}

type Result struct {
	Verdict  Verdict
	Record   Record
	IntentID string
	// Collided records that the claim insert hit an existing row, which is the ADR-0010
	// routing trigger regardless of how the collision was then resolved.
	Collided bool
}

const insertKey = `
	INSERT INTO idempotency_keys
	    (agent_id, idempotency_key, request_hash, resource_type, resource_id, operation)
	VALUES ($1, $2::uuid, $3, $4, $5, $6)
	ON CONFLICT (agent_id, idempotency_key) DO NOTHING
	RETURNING status`

const selectKey = `
	SELECT request_hash, status, result, coalesce(outcome_detail, ''), attempt_count
	  FROM idempotency_keys
	 WHERE agent_id = $1 AND idempotency_key = $2::uuid`

const reclaimKey = `
	UPDATE idempotency_keys
	   SET status = 'pending', claimed_at = now(), completed_at = NULL,
	       attempt_count = attempt_count + 1, result = NULL, result_ref = NULL,
	       outcome_detail = NULL
	 WHERE agent_id = $1 AND idempotency_key = $2::uuid AND status = 'failed'
	RETURNING attempt_count`

const insertIntent = `
	INSERT INTO write_intents
	    (agent_id, idempotency_key, resource_type, resource_id, operation, operation_class, payload)
	VALUES ($1, $2::uuid, $3, $4, $5, $6::operation_class, $7)
	RETURNING intent_id`

// The claim commits before any downstream call and is never held open across one. The
// unique constraint on (agent_id, idempotency_key) is the at-most-once guarantee itself.
func Claim(ctx context.Context, pool *pgxpool.Pool, req Request) (Result, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("begin claim: %w", err)
	}
	defer tx.Rollback(ctx)

	won, err := insertClaim(ctx, tx, req)
	if err != nil {
		return Result{}, err
	}

	if !won {
		existing, err := readRecord(ctx, tx, req.AgentID, req.Key)
		if err != nil {
			return Result{}, err
		}
		if existing.RequestHash != req.RequestHash {
			return Result{Verdict: VerdictMismatch, Record: existing, Collided: true}, nil
		}
		if existing.Status != StatusFailed {
			return Result{Verdict: VerdictExisting, Record: existing, Collided: true}, nil
		}

		attempt, reclaimed, err := reclaim(ctx, tx, req)
		if err != nil {
			return Result{}, err
		}
		if !reclaimed {
			existing.Status = StatusPending
			return Result{Verdict: VerdictExisting, Record: existing, Collided: true}, nil
		}
		existing.Status = StatusPending
		existing.AttemptCount = attempt
		existing.Result = nil
		existing.OutcomeDetail = ""

		intentID, err := recordIntent(ctx, tx, req)
		if err != nil {
			return Result{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Result{}, fmt.Errorf("commit reclaim: %w", err)
		}
		return Result{Verdict: VerdictClaimed, Record: existing, IntentID: intentID, Collided: true}, nil
	}

	intentID, err := recordIntent(ctx, tx, req)
	if err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit claim: %w", err)
	}

	return Result{
		Verdict: VerdictClaimed,
		Record: Record{
			AgentID:      req.AgentID,
			Key:          req.Key,
			RequestHash:  req.RequestHash,
			Status:       StatusPending,
			AttemptCount: 1,
		},
		IntentID: intentID,
	}, nil
}

func insertClaim(ctx context.Context, tx pgx.Tx, req Request) (bool, error) {
	var status Status
	err := tx.QueryRow(ctx, insertKey,
		req.AgentID, req.Key, req.RequestHash,
		req.ResourceType, req.ResourceID, req.Operation,
	).Scan(&status)

	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("insert claim: %w", err)
	}
	return true, nil
}

func reclaim(ctx context.Context, tx pgx.Tx, req Request) (int, bool, error) {
	var attempt int
	err := tx.QueryRow(ctx, reclaimKey, req.AgentID, req.Key).Scan(&attempt)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("reclaim key: %w", err)
	}
	return attempt, true, nil
}

func recordIntent(ctx context.Context, tx pgx.Tx, req Request) (string, error) {
	var intentID string
	err := tx.QueryRow(ctx, insertIntent,
		req.AgentID, req.Key, req.ResourceType, req.ResourceID,
		req.Operation, req.OperationClass, []byte(req.Payload),
	).Scan(&intentID)
	if err != nil {
		return "", fmt.Errorf("insert intent: %w", err)
	}
	return intentID, nil
}

func readRecord(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, agentID, key string) (Record, error) {
	record := Record{AgentID: agentID, Key: key}
	var result []byte

	err := q.QueryRow(ctx, selectKey, agentID, key).Scan(
		&record.RequestHash, &record.Status, &result,
		&record.OutcomeDetail, &record.AttemptCount,
	)
	if err != nil {
		return Record{}, fmt.Errorf("read key: %w", err)
	}
	record.Result = json.RawMessage(result)
	return record, nil
}

// Lookup backs GET /v1/writes/{key}. Scoped to the agent so a key belonging to another
// agent is indistinguishable from one that does not exist.
func Lookup(ctx context.Context, pool *pgxpool.Pool, agentID, key string) (Record, bool, error) {
	record, err := readRecord(ctx, pool, agentID, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	return record, true, nil
}

const completeKey = `
	UPDATE idempotency_keys
	   SET status = $3::key_status, result = $4, outcome_detail = $5, completed_at = now()
	 WHERE agent_id = $1 AND idempotency_key = $2::uuid AND status = 'pending'`

// Complete is guarded on status = 'pending' so a terminal outcome is never overwritten,
// including by a late writer racing the reconciler.
func Complete(ctx context.Context, pool *pgxpool.Pool, agentID, key string, status Status, result json.RawMessage, detail string) (bool, error) {
	var storedResult, storedDetail any
	if len(result) > 0 {
		storedResult = []byte(result)
	}
	if detail != "" {
		storedDetail = detail
	}

	tag, err := pool.Exec(ctx, completeKey, agentID, key, string(status), storedResult, storedDetail)
	if err != nil {
		return false, fmt.Errorf("complete key: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
