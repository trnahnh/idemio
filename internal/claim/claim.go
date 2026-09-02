package claim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trnahnh/idemio/internal/conflict"
	"github.com/trnahnh/idemio/internal/manifest"
)

type Verdict string

const (
	VerdictClaimed     Verdict = "claimed"
	VerdictExisting    Verdict = "existing"
	VerdictMismatch    Verdict = "mismatch"
	VerdictRejected    Verdict = "rejected"
	VerdictSerialize   Verdict = "serialize"
	VerdictLockTimeout Verdict = "lock_timeout"

	VerdictSerializeTimeout Verdict = "serialize_timeout"
)

type Status string

const (
	StatusPending       Status = "pending"
	StatusDone          Status = "done"
	StatusFailed        Status = "failed"
	StatusIndeterminate Status = "indeterminate"
	StatusRejected      Status = "rejected"
)

func (s Status) terminal() bool { return s != StatusPending }

type Resolution string

const (
	ResolutionSerialized Resolution = "serialized"
	ResolutionRejected   Resolution = "rejected"
	ResolutionObserved   Resolution = "observed"
)

type Request struct {
	AgentID         string
	Key             string
	RequestHash     string
	ResourceType    string
	ResourceID      string
	Operation       string
	Declared        manifest.Operation
	Payload         json.RawMessage
	Window          time.Duration
	Enforce         bool
	ManifestVersion string
	LockTimeout     time.Duration
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

type Blocking struct {
	AgentID string
	Key     string
}

type Result struct {
	Verdict  Verdict
	Record   Record
	IntentID string
	Collided bool
	LockWait time.Duration
	Blocking Blocking
	Observed int
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
	    (agent_id, idempotency_key, resource_type, resource_id, operation,
	     operation_class, scope_selector, payload)
	VALUES ($1, $2::uuid, $3, $4, $5, $6::operation_class, $7, $8)
	RETURNING intent_id::text, emitted_at`

const selectWindow = `
	SELECT intent_id::text, agent_id, idempotency_key::text,
	       operation_class::text, scope_selector
	  FROM write_intents
	 WHERE resource_type = $1
	   AND resource_id   = $2
	   AND emitted_at > now() - make_interval(secs => $3)
	   AND voided_at IS NULL
	   AND intent_id <> $4::uuid
	 ORDER BY emitted_at DESC`

const selectStatuses = `
	SELECT idempotency_key::text, status
	  FROM idempotency_keys
	 WHERE agent_id = $1 AND idempotency_key = ANY($2::uuid[])`

const insertConflict = `
	INSERT INTO conflicts
	    (intent_id_a, intent_id_b, agent_id_a, agent_id_b, resource_type, resource_id,
	     reason, resolution, manifest_version)
	VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8::conflict_resolution, $9)
	RETURNING conflict_id::text`

const rejectKey = `
	UPDATE idempotency_keys
	   SET status = 'rejected', completed_at = now(), result = $3, outcome_detail = $4
	 WHERE agent_id = $1 AND idempotency_key = $2::uuid
	RETURNING result`

const voidIntent = `
	UPDATE write_intents SET voided_at = now()
	 WHERE intent_id = $1::uuid AND emitted_at = $2`

const lockNotAvailable = "55P03"

type intent struct {
	id       string
	agentID  string
	key      string
	declared manifest.Operation
}

func Claim(ctx context.Context, pool *pgxpool.Pool, req Request) (Result, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("begin claim: %w", err)
	}
	defer tx.Rollback(ctx)

	lockWait, err := lockResource(ctx, tx, req)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == lockNotAvailable {
			return Result{Verdict: VerdictLockTimeout, LockWait: lockWait}, nil
		}
		return Result{}, err
	}

	result, fresh, err := claimKey(ctx, tx, req)
	if err != nil {
		return Result{}, err
	}
	result.LockWait = lockWait
	if !fresh {
		return result, nil
	}

	intentID, emittedAt, err := recordIntent(ctx, tx, req)
	if err != nil {
		return Result{}, err
	}
	result.IntentID = intentID

	outcome, err := resolveConflicts(ctx, tx, req, intentID, emittedAt)
	if err != nil {
		return Result{}, err
	}
	result.Verdict = outcome.Verdict
	result.Observed = outcome.Observed
	result.Blocking = outcome.Blocking

	if outcome.Verdict == VerdictSerialize {
		return result, nil
	}
	if outcome.Verdict == VerdictRejected {
		result.Record.Status = StatusRejected
		result.Record.Result = outcome.Record.Result
		result.Record.OutcomeDetail = outcome.Record.OutcomeDetail
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit claim: %w", err)
	}
	return result, nil
}

// ADR-0015: first statement of the transaction. Locking after the intent insert lets two
// conflicting writers each observe the other and both lose.
//
// The timeout is set in the same statement rather than a preceding one, because a round
// trip here is a round trip on every write. MATERIALIZED is what makes that safe: it forces
// the CTE to be evaluated before the lock is requested, which a bare subquery does not
// guarantee.
const lockResourceStmt = `
	WITH timeout AS MATERIALIZED (SELECT set_config('lock_timeout', $1, true))
	SELECT pg_advisory_xact_lock(hashtextextended($2, 0)) FROM timeout`

func lockResource(ctx context.Context, tx pgx.Tx, req Request) (time.Duration, error) {
	timeout := strconv.FormatInt(req.LockTimeout.Milliseconds(), 10)

	started := time.Now()
	_, err := tx.Exec(ctx, lockResourceStmt, timeout, req.ResourceType+":"+req.ResourceID)
	return time.Since(started), err
}

func claimKey(ctx context.Context, tx pgx.Tx, req Request) (Result, bool, error) {
	won, err := insertClaim(ctx, tx, req)
	if err != nil {
		return Result{}, false, err
	}
	if won {
		return Result{
			Verdict: VerdictClaimed,
			Record: Record{
				AgentID:      req.AgentID,
				Key:          req.Key,
				RequestHash:  req.RequestHash,
				Status:       StatusPending,
				AttemptCount: 1,
			},
		}, true, nil
	}

	existing, err := readRecord(ctx, tx, req.AgentID, req.Key)
	if err != nil {
		return Result{}, false, err
	}
	if existing.RequestHash != req.RequestHash {
		return Result{Verdict: VerdictMismatch, Record: existing, Collided: true}, false, nil
	}
	if existing.Status != StatusFailed {
		return Result{Verdict: VerdictExisting, Record: existing, Collided: true}, false, nil
	}

	attempt, reclaimed, err := reclaim(ctx, tx, req)
	if err != nil {
		return Result{}, false, err
	}
	existing.Status = StatusPending
	if !reclaimed {
		return Result{Verdict: VerdictExisting, Record: existing, Collided: true}, false, nil
	}

	existing.AttemptCount = attempt
	existing.Result = nil
	existing.OutcomeDetail = ""
	return Result{Verdict: VerdictClaimed, Record: existing, Collided: true}, true, nil
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

func recordIntent(ctx context.Context, tx pgx.Tx, req Request) (string, time.Time, error) {
	var intentID string
	var emittedAt time.Time

	err := tx.QueryRow(ctx, insertIntent,
		req.AgentID, req.Key, req.ResourceType, req.ResourceID, req.Operation,
		req.Declared.Class, req.Declared.Scope, []byte(req.Payload),
	).Scan(&intentID, &emittedAt)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("insert intent: %w", err)
	}
	return intentID, emittedAt, nil
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

func conflictingIntents(ctx context.Context, tx pgx.Tx, req Request, exclude string) ([]intent, error) {
	rows, err := tx.Query(ctx, selectWindow,
		req.ResourceType, req.ResourceID, req.Window.Seconds(), exclude)
	if err != nil {
		return nil, fmt.Errorf("read conflict window: %w", err)
	}
	defer rows.Close()

	var found []intent
	for rows.Next() {
		var candidate intent
		if err := rows.Scan(&candidate.id, &candidate.agentID, &candidate.key,
			&candidate.declared.Class, &candidate.declared.Scope); err != nil {
			return nil, fmt.Errorf("scan conflict window: %w", err)
		}
		if conflict.Compatible(req.Declared, candidate.declared) {
			continue
		}
		found = append(found, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read conflict window: %w", err)
	}
	return found, nil
}
