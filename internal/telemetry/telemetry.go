package telemetry

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	registry          *prometheus.Registry
	resultInlineBytes int64

	Writes          *prometheus.CounterVec
	Responses       *prometheus.CounterVec
	Replays         prometheus.Counter
	ClaimCollisions prometheus.Counter
	Reclaims        prometheus.Counter
	HashMismatches  *prometheus.CounterVec
	OversizedResult prometheus.Counter
	ProbeFailures   *prometheus.CounterVec
	Reconciled      *prometheus.CounterVec

	DownstreamDuration *prometheus.HistogramVec
}

func New(pool *pgxpool.Pool, resultInlineBytes int64) *Metrics {
	registry := prometheus.NewRegistry()

	m := &Metrics{
		registry: registry,
		Writes: counterVec(registry, "idemio_writes_total",
			"Writes by terminal status.", "status"),
		Responses: counterVec(registry, "idemio_responses_total",
			"API responses by status code.", "code"),
		Replays: counter(registry, "idemio_replays_total",
			"Replays served from a stored result. A high rate is the system working."),
		ClaimCollisions: counter(registry, "idemio_claim_collisions_total",
			"Claims that hit an existing key. An ADR-0010 routing trigger above 1% of writes."),
		Reclaims: counter(registry, "idemio_reclaims_total",
			"Re-claims of keys previously left failed."),
		HashMismatches: counterVec(registry, "idemio_hash_mismatches_total",
			"Reused keys carrying a different request body, by agent.", "agent_id"),
		OversizedResult: counter(registry, "idemio_oversized_results_total",
			"Results stored inline above limits.result_inline_bytes. Phase 1 sets the cap from this."),
		ProbeFailures: counterVec(registry, "idemio_probe_failures_total",
			"Probes that could not be reached, by resource type.", "resource_type"),
		Reconciled: counterVec(registry, "idemio_reconciled_total",
			"Stale pending keys resolved by the reconciler, by outcome.", "outcome"),
		DownstreamDuration: histogramVec(registry, "idemio_downstream_duration_seconds",
			"Downstream call latency by disposition.", "disposition"),
	}

	registry.MustRegister(&databaseCollector{pool: pool})
	m.resultInlineBytes = resultInlineBytes
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *Metrics) ObserveResultSize(size int) {
	if int64(size) > m.resultInlineBytes {
		m.OversizedResult.Inc()
	}
}

func counter(registry *prometheus.Registry, name, help string) prometheus.Counter {
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: name, Help: help})
	registry.MustRegister(c)
	return c
}

func counterVec(registry *prometheus.Registry, name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
	registry.MustRegister(c)
	return c
}

func histogramVec(registry *prometheus.Registry, name, help string, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    name,
		Help:    help,
		Buckets: prometheus.DefBuckets,
	}, labels)
	registry.MustRegister(h)
	return h
}

var (
	indeterminateKeys = prometheus.NewDesc(
		"idemio_indeterminate_keys",
		"Keys whose outcome is unknown. The correct value is zero; any sustained value pages.",
		nil, nil)
	pendingKeys = prometheus.NewDesc(
		"idemio_pending_keys",
		"Keys currently claimed with a downstream call in flight.",
		nil, nil)
	oldestPendingAge = prometheus.NewDesc(
		"idemio_oldest_pending_age_seconds",
		"Age of the oldest pending key. Compared against reconcile.stale_after.",
		nil, nil)
	partitionHeadroom = prometheus.NewDesc(
		"idemio_partition_headroom_seconds",
		"Time until the newest range partition ends. A missing partition is a write outage.",
		[]string{"table"}, nil)
)

type databaseCollector struct {
	pool *pgxpool.Pool
}

func (c *databaseCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- indeterminateKeys
	ch <- pendingKeys
	ch <- oldestPendingAge
	ch <- partitionHeadroom
}

func (c *databaseCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	const counts = `
		SELECT count(*) FILTER (WHERE status = 'indeterminate'),
		       count(*) FILTER (WHERE status = 'pending'),
		       coalesce(extract(epoch FROM now() - min(claimed_at) FILTER (WHERE status = 'pending')), 0)
		  FROM idempotency_keys`

	var indeterminate, pending, oldest float64
	if err := c.pool.QueryRow(ctx, counts).Scan(&indeterminate, &pending, &oldest); err == nil {
		ch <- prometheus.MustNewConstMetric(indeterminateKeys, prometheus.GaugeValue, indeterminate)
		ch <- prometheus.MustNewConstMetric(pendingKeys, prometheus.GaugeValue, pending)
		ch <- prometheus.MustNewConstMetric(oldestPendingAge, prometheus.GaugeValue, oldest)
	}

	const headroom = `
		SELECT max((regexp_match(pg_get_expr(c.relpartbound, c.oid), 'TO \(''([^'']+)''\)'))[1]::timestamptz)
		  FROM pg_class c
		  JOIN pg_inherits i ON i.inhrelid = c.oid
		 WHERE i.inhparent = $1::regclass`

	for _, table := range []string{"write_intents", "conflicts", "payload_access_audit"} {
		var latest time.Time
		if err := c.pool.QueryRow(ctx, headroom, table).Scan(&latest); err != nil {
			continue
		}
		ch <- prometheus.MustNewConstMetric(partitionHeadroom, prometheus.GaugeValue,
			time.Until(latest).Seconds(), table)
	}
}
