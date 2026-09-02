package archive

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Timestamps are stored as microseconds since the epoch, with zero standing for NULL. A
// column type per table keeps the archive columnar, which is the whole reason ADR-0009
// chose Parquet over a row format.
type intentRecord struct {
	IntentID       string   `parquet:"intent_id"`
	AgentID        string   `parquet:"agent_id"`
	IdempotencyKey string   `parquet:"idempotency_key"`
	ResourceType   string   `parquet:"resource_type"`
	ResourceID     string   `parquet:"resource_id"`
	Operation      string   `parquet:"operation"`
	OperationClass string   `parquet:"operation_class"`
	ScopeSelector  []string `parquet:"scope_selector"`
	Payload        string   `parquet:"payload"`
	EmittedAt      int64    `parquet:"emitted_at"`
	PublishedAt    int64    `parquet:"published_at"`
	VoidedAt       int64    `parquet:"voided_at"`
}

type conflictRecord struct {
	ConflictID      string `parquet:"conflict_id"`
	IntentIDA       string `parquet:"intent_id_a"`
	IntentIDB       string `parquet:"intent_id_b"`
	AgentIDA        string `parquet:"agent_id_a"`
	AgentIDB        string `parquet:"agent_id_b"`
	ResourceType    string `parquet:"resource_type"`
	ResourceID      string `parquet:"resource_id"`
	Reason          string `parquet:"reason"`
	Resolution      string `parquet:"resolution"`
	ManifestVersion string `parquet:"manifest_version"`
	DetectedAt      int64  `parquet:"detected_at"`
}

type auditRecord struct {
	AuditID      string   `parquet:"audit_id"`
	Principal    string   `parquet:"principal"`
	CallerRole   string   `parquet:"caller_role"`
	Endpoint     string   `parquet:"endpoint"`
	QueryParams  string   `parquet:"query_params"`
	RecordCount  int32    `parquet:"record_count"`
	IntentIDs    []string `parquet:"intent_ids"`
	StatedReason string   `parquet:"stated_reason"`
	AccessedAt   int64    `parquet:"accessed_at"`
}

func scanIntents(ctx context.Context, pool *pgxpool.Pool, partition string) ([]intentRecord, error) {
	stmt := fmt.Sprintf(`
		SELECT intent_id::text, agent_id, idempotency_key::text, resource_type, resource_id,
		       operation, operation_class::text, coalesce(scope_selector, '{}'), payload::text,
		       emitted_at, published_at, voided_at
		  FROM %s`, partition)

	rows, err := pool.Query(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", partition, err)
	}
	defer rows.Close()

	var found []intentRecord
	for rows.Next() {
		var row intentRecord
		var emitted time.Time
		var published, voided *time.Time

		if err := rows.Scan(&row.IntentID, &row.AgentID, &row.IdempotencyKey, &row.ResourceType,
			&row.ResourceID, &row.Operation, &row.OperationClass, &row.ScopeSelector,
			&row.Payload, &emitted, &published, &voided); err != nil {
			return nil, fmt.Errorf("scan %s: %w", partition, err)
		}
		row.EmittedAt = emitted.UnixMicro()
		row.PublishedAt = micros(published)
		row.VoidedAt = micros(voided)
		found = append(found, row)
	}
	return found, rows.Err()
}

func scanConflicts(ctx context.Context, pool *pgxpool.Pool, partition string) ([]conflictRecord, error) {
	stmt := fmt.Sprintf(`
		SELECT conflict_id::text, intent_id_a::text, intent_id_b::text, agent_id_a, agent_id_b,
		       resource_type, resource_id, reason, resolution::text,
		       coalesce(manifest_version, ''), detected_at
		  FROM %s`, partition)

	rows, err := pool.Query(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", partition, err)
	}
	defer rows.Close()

	var found []conflictRecord
	for rows.Next() {
		var row conflictRecord
		var detected time.Time

		if err := rows.Scan(&row.ConflictID, &row.IntentIDA, &row.IntentIDB, &row.AgentIDA,
			&row.AgentIDB, &row.ResourceType, &row.ResourceID, &row.Reason, &row.Resolution,
			&row.ManifestVersion, &detected); err != nil {
			return nil, fmt.Errorf("scan %s: %w", partition, err)
		}
		row.DetectedAt = detected.UnixMicro()
		found = append(found, row)
	}
	return found, rows.Err()
}

func scanAudit(ctx context.Context, pool *pgxpool.Pool, partition string) ([]auditRecord, error) {
	stmt := fmt.Sprintf(`
		SELECT audit_id::text, principal, caller_role, endpoint, query_params::text,
		       record_count, coalesce(intent_ids::text[], '{}'), coalesce(stated_reason, ''),
		       accessed_at
		  FROM %s`, partition)

	rows, err := pool.Query(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", partition, err)
	}
	defer rows.Close()

	var found []auditRecord
	for rows.Next() {
		var row auditRecord
		var accessed time.Time

		if err := rows.Scan(&row.AuditID, &row.Principal, &row.CallerRole, &row.Endpoint,
			&row.QueryParams, &row.RecordCount, &row.IntentIDs, &row.StatedReason,
			&accessed); err != nil {
			return nil, fmt.Errorf("scan %s: %w", partition, err)
		}
		row.AccessedAt = accessed.UnixMicro()
		found = append(found, row)
	}
	return found, rows.Err()
}

const insertIntent = `
	INSERT INTO %s (intent_id, agent_id, idempotency_key, resource_type, resource_id,
	                operation, operation_class, scope_selector, payload, emitted_at,
	                published_at, voided_at)
	VALUES ($1::uuid, $2, $3::uuid, $4, $5, $6, $7::operation_class, $8, $9::jsonb, $10, $11, $12)`

const insertConflict = `
	INSERT INTO %s (conflict_id, intent_id_a, intent_id_b, agent_id_a, agent_id_b,
	                resource_type, resource_id, reason, resolution, manifest_version, detected_at)
	VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, $8, $9::conflict_resolution, $10, $11)`

const insertAudit = `
	INSERT INTO %s (audit_id, principal, caller_role, endpoint, query_params, record_count,
	                intent_ids, stated_reason, accessed_at)
	VALUES ($1::uuid, $2, $3, $4, $5::jsonb, $6, $7::uuid[], $8, $9)`

func intentValues(row intentRecord) []any {
	return []any{row.IntentID, row.AgentID, row.IdempotencyKey, row.ResourceType, row.ResourceID,
		row.Operation, row.OperationClass, row.ScopeSelector, row.Payload,
		time.UnixMicro(row.EmittedAt).UTC(), fromMicros(row.PublishedAt), fromMicros(row.VoidedAt)}
}

func conflictValues(row conflictRecord) []any {
	return []any{row.ConflictID, row.IntentIDA, row.IntentIDB, row.AgentIDA, row.AgentIDB,
		row.ResourceType, row.ResourceID, row.Reason, row.Resolution, row.ManifestVersion,
		time.UnixMicro(row.DetectedAt).UTC()}
}

func auditValues(row auditRecord) []any {
	return []any{row.AuditID, row.Principal, row.CallerRole, row.Endpoint, row.QueryParams,
		row.RecordCount, row.IntentIDs, row.StatedReason, time.UnixMicro(row.AccessedAt).UTC()}
}
