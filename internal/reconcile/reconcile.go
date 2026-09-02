package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trnahnh/idemio/internal/claim"
	"github.com/trnahnh/idemio/internal/manifest"
	"github.com/trnahnh/idemio/internal/probe"
	"github.com/trnahnh/idemio/internal/telemetry"
)

const defaultBatch = 200

type Prober interface {
	Probe(ctx context.Context, path, agentID, key string) (probe.Outcome, json.RawMessage, error)
}

type Reconciler struct {
	pool       *pgxpool.Pool
	prober     Prober
	manifests  *manifest.Store
	staleAfter time.Duration
	batch      int
	metrics    *telemetry.Metrics
	logger     *slog.Logger
}

type Summary struct {
	Scanned       int
	Done          int
	Failed        int
	Indeterminate int
	Unresolved    int
}

type stale struct {
	agentID      string
	key          string
	resourceType string
}

const selectStale = `
	SELECT agent_id, idempotency_key::text, resource_type
	  FROM idempotency_keys
	 WHERE status = 'pending' AND claimed_at < now() - make_interval(secs => $1)
	 ORDER BY claimed_at
	 LIMIT $2`

func New(pool *pgxpool.Pool, prober Prober, manifests *manifest.Store, staleAfter time.Duration,
	metrics *telemetry.Metrics, logger *slog.Logger) *Reconciler {

	return &Reconciler{
		pool:       pool,
		prober:     prober,
		manifests:  manifests,
		staleAfter: staleAfter,
		batch:      defaultBatch,
		metrics:    metrics,
		logger:     logger,
	}
}

func (r *Reconciler) Run(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			summary, err := r.Sweep(ctx)
			if err != nil {
				r.logger.Error("sweep failed", "error", err)
				continue
			}
			if summary.Scanned > 0 {
				r.logger.Info("sweep complete",
					"scanned", summary.Scanned, "done", summary.Done,
					"failed", summary.Failed, "indeterminate", summary.Indeterminate,
					"unresolved", summary.Unresolved)
			}
		}
	}
}

func (r *Reconciler) Sweep(ctx context.Context) (Summary, error) {
	candidates, err := r.staleKeys(ctx)
	if err != nil {
		return Summary{}, err
	}

	summary := Summary{Scanned: len(candidates)}
	for _, candidate := range candidates {
		r.resolve(ctx, candidate, &summary)
	}
	return summary, nil
}

func (r *Reconciler) staleKeys(ctx context.Context) ([]stale, error) {
	rows, err := r.pool.Query(ctx, selectStale, r.staleAfter.Seconds(), r.batch)
	if err != nil {
		return nil, fmt.Errorf("select stale keys: %w", err)
	}
	defer rows.Close()

	var candidates []stale
	for rows.Next() {
		var candidate stale
		if err := rows.Scan(&candidate.agentID, &candidate.key, &candidate.resourceType); err != nil {
			return nil, fmt.Errorf("scan stale key: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read stale keys: %w", err)
	}
	return candidates, nil
}

func (r *Reconciler) resolve(ctx context.Context, candidate stale, summary *Summary) {
	definition, registered := r.manifests.Current().Lookup(candidate.resourceType)
	if !registered {
		r.escalate(ctx, candidate, "no_probe_registered", summary)
		return
	}

	outcome, result, err := r.prober.Probe(ctx, definition.ProbePath, candidate.agentID, candidate.key)
	if err != nil {
		summary.Unresolved++
		r.metrics.ProbeFailures.WithLabelValues(candidate.resourceType).Inc()
		r.logger.Warn("probe unavailable; key left pending",
			"agent_id", candidate.agentID, "key", candidate.key, "error", err)
		return
	}

	switch outcome {
	case probe.Executed:
		if r.complete(ctx, candidate, claim.StatusDone, result, "resolved_by_probe", summary) {
			summary.Done++
			r.metrics.Reconciled.WithLabelValues("done").Inc()
		}
	case probe.NotExecuted:
		if r.complete(ctx, candidate, claim.StatusFailed, nil, "probe_found_no_execution", summary) {
			summary.Failed++
			r.metrics.Reconciled.WithLabelValues("failed").Inc()
		}
	default:
		r.escalate(ctx, candidate, "probe_returned_unknown", summary)
	}
}

func (r *Reconciler) escalate(ctx context.Context, candidate stale, detail string, summary *Summary) {
	if !r.complete(ctx, candidate, claim.StatusIndeterminate, nil, detail, summary) {
		return
	}
	summary.Indeterminate++
	r.metrics.Reconciled.WithLabelValues("indeterminate").Inc()
	r.logger.Error("key escalated to indeterminate",
		"agent_id", candidate.agentID, "key", candidate.key,
		"resource_type", candidate.resourceType, "detail", detail)
}

func (r *Reconciler) complete(ctx context.Context, candidate stale, status claim.Status,
	result json.RawMessage, detail string, summary *Summary) bool {

	updated, err := claim.Complete(ctx, r.pool, candidate.agentID, candidate.key, status, result, detail)
	if err != nil {
		summary.Unresolved++
		r.logger.Error("recording reconciled outcome failed",
			"agent_id", candidate.agentID, "key", candidate.key, "error", err)
		return false
	}
	if !updated {
		r.logger.Info("key resolved by someone else first",
			"agent_id", candidate.agentID, "key", candidate.key)
	}
	return updated
}
