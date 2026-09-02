package api

import (
	"context"
	"fmt"
	"net/url"

	"github.com/jackc/pgx/v5"
)

type auditEntry struct {
	principal string
	role      string
	endpoint  string
	params    url.Values
	ids       []string
	reason    string
}

const insertAudit = `
	INSERT INTO payload_access_audit
	    (principal, caller_role, endpoint, query_params, record_count, intent_ids, stated_reason)
	VALUES ($1, $2, $3, $4, $5, $6::uuid[], $7)`

// The audit row commits with the read that produced it, so a payload cannot be returned
// unaudited (ADR-0011). It records which records were read, never their contents.
func recordPayloadAccess(ctx context.Context, tx pgx.Tx, entry auditEntry) error {
	params := make(map[string]string, len(entry.params))
	for key := range entry.params {
		params[key] = entry.params.Get(key)
	}

	_, err := tx.Exec(ctx, insertAudit, entry.principal, entry.role, entry.endpoint,
		params, len(entry.ids), entry.ids, entry.reason)
	if err != nil {
		return fmt.Errorf("record payload access: %w", err)
	}
	return nil
}
