package telemetry

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	registry          *prometheus.Registry
	resultInlineBytes int64
	names             []string

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

func New(pool *pgxpool.Pool, resultInlineBytes int64, logger *slog.Logger) *Metrics {
	m := &Metrics{
		registry:          prometheus.NewRegistry(),
		resultInlineBytes: resultInlineBytes,
	}

	m.Writes = m.counterVec("idemio_writes_total",
		"Writes by terminal status.", "status")
	m.Responses = m.counterVec("idemio_responses_total",
		"API responses by status code.", "code")
	m.Replays = m.counter("idemio_replays_total",
		"Replays served from a stored result. A high rate is the system working.")
	m.ClaimCollisions = m.counter("idemio_claim_collisions_total",
		"Claims that hit an existing key. An ADR-0010 routing trigger above 1% of writes.")
	m.Reclaims = m.counter("idemio_reclaims_total",
		"Re-claims of keys previously left failed.")
	m.HashMismatches = m.counterVec("idemio_hash_mismatches_total",
		"Reused keys carrying a different request body, by agent.", "agent_id")
	m.OversizedResult = m.counter("idemio_oversized_results_total",
		"Results stored inline above limits.result_inline_bytes. Phase 1 sets the cap from this.")
	m.ProbeFailures = m.counterVec("idemio_probe_failures_total",
		"Probes that could not be reached, by resource type.", "resource_type")
	m.Reconciled = m.counterVec("idemio_reconciled_total",
		"Stale pending keys resolved by the reconciler, by outcome.", "outcome")
	m.DownstreamDuration = m.histogramVec("idemio_downstream_duration_seconds",
		"Downstream call latency by disposition.", "disposition")

	m.registry.MustRegister(&databaseCollector{pool: pool, logger: logger})
	m.names = append(m.names,
		indeterminateKeysName, pendingKeysName, oldestPendingAgeName, partitionHeadroomName)

	slices.Sort(m.names)
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *Metrics) Names() []string {
	return slices.Clone(m.names)
}

func (m *Metrics) ObserveResultSize(size int) {
	if int64(size) > m.resultInlineBytes {
		m.OversizedResult.Inc()
	}
}

func (m *Metrics) counter(name, help string) prometheus.Counter {
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: name, Help: help})
	m.registry.MustRegister(c)
	m.names = append(m.names, name)
	return c
}

func (m *Metrics) counterVec(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
	m.registry.MustRegister(c)
	m.names = append(m.names, name)
	return c
}

func (m *Metrics) histogramVec(name, help string, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    name,
		Help:    help,
		Buckets: prometheus.DefBuckets,
	}, labels)
	m.registry.MustRegister(h)
	m.names = append(m.names, name)
	return h
}

const (
	indeterminateKeysName = "idemio_indeterminate_keys"
	pendingKeysName       = "idemio_pending_keys"
	oldestPendingAgeName  = "idemio_oldest_pending_age_seconds"
	partitionHeadroomName = "idemio_partition_headroom_seconds"
)

var (
	indeterminateKeys = prometheus.NewDesc(
		indeterminateKeysName,
		"Keys whose outcome is unknown. The correct value is zero; any sustained value pages.",
		nil, nil)
	pendingKeys = prometheus.NewDesc(
		pendingKeysName,
		"Keys currently claimed with a downstream call in flight.",
		nil, nil)
	oldestPendingAge = prometheus.NewDesc(
		oldestPendingAgeName,
		"Age of the oldest pending key. Compared against reconcile.stale_after.",
		nil, nil)
	partitionHeadroom = prometheus.NewDesc(
		partitionHeadroomName,
		"Time until the newest range partition ends. A missing partition is a write outage.",
		[]string{"table"}, nil)
)

type databaseCollector struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
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
	if err := c.pool.QueryRow(ctx, counts).Scan(&indeterminate, &pending, &oldest); err != nil {
		c.logger.Error("correctness gauges unavailable this scrape", "error", err)
	} else {
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
			c.logger.Error("partition headroom unavailable this scrape", "table", table, "error", err)
			continue
		}
		ch <- prometheus.MustNewConstMetric(partitionHeadroom, prometheus.GaugeValue,
			time.Until(latest).Seconds(), table)
	}
}
