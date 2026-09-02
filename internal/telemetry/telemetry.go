package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/trnahnh/idemio/internal/config"
)

type Metrics struct {
	registry          *prometheus.Registry
	resultInlineBytes int64
	names             []string

	Writes          *prometheus.CounterVec
	Responses       *prometheus.CounterVec
	Replays         prometheus.Counter
	ClaimCollisions prometheus.Counter
	HashMismatches  *prometheus.CounterVec
	OversizedResult prometheus.Counter
	ProbeFailures   *prometheus.CounterVec
	Reconciled      *prometheus.CounterVec

	DownstreamDuration *prometheus.HistogramVec
	ReclaimAttempts    prometheus.Histogram

	Conflicts          *prometheus.CounterVec
	LockTimeouts       *prometheus.CounterVec
	SerializationWaits *prometheus.CounterVec
	LockWait           *prometheus.HistogramVec

	ManifestReloadFailures prometheus.Counter
	ManifestInfo           *prometheus.GaugeVec

	RelayPublished     prometheus.Counter
	RelayFailures      prometheus.Counter
	PartitionsCreated  *prometheus.CounterVec
	RetentionDeleted   *prometheus.CounterVec
	ArchivedPartitions *prometheus.CounterVec
}

func New(pool *pgxpool.Pool, cfg config.Config, logger *slog.Logger) *Metrics {
	m := &Metrics{
		registry:          prometheus.NewRegistry(),
		resultInlineBytes: cfg.ResultInlineBytes,
	}

	m.Writes = m.counterVec("idemio_writes_total",
		"Writes by terminal status.", "status")
	m.Responses = m.counterVec("idemio_responses_total",
		"API responses by status code.", "code")
	m.Replays = m.counter("idemio_replays_total",
		"Replays served from a stored result. A high rate is the system working.")
	m.ClaimCollisions = m.counter("idemio_claim_collisions_total",
		"Claims that hit an existing key. An ADR-0010 routing trigger above 1% of writes.")
	m.ReclaimAttempts = m.histogram("idemio_reclaim_attempts",
		"Attempt number at each re-claim of a key previously left failed. The tail is the "+
			"signal: one key retried many times is a flapping downstream.",
		[]float64{1, 2, 3, 5, 8, 13, 21, 34})
	m.HashMismatches = m.counterVec("idemio_hash_mismatches_total",
		"Reused keys carrying a different request body. Labelled by resource type, never by "+
			"agent: per-agent detail lives in the conflicts and intents read APIs, which are "+
			"not a fixed-cardinality store.", "resource_type")
	m.OversizedResult = m.counter("idemio_oversized_results_total",
		"Results stored inline above limits.result_inline_bytes. Phase 1 sets the cap from this.")
	m.ProbeFailures = m.counterVec("idemio_probe_failures_total",
		"Probes that could not be reached, by resource type.", "resource_type")
	m.Reconciled = m.counterVec("idemio_reconciled_total",
		"Stale pending keys resolved by the reconciler, by outcome.", "outcome")
	m.DownstreamDuration = m.histogramVec("idemio_downstream_duration_seconds",
		"Downstream call latency by disposition.", "disposition")

	m.Conflicts = m.counterVec("idemio_conflicts_total",
		"Conflicting write pairs detected, by resolution. 'observed' is shadow mode, which "+
			"rejects nothing.", "resource_type", "resolution")
	m.LockTimeouts = m.counterVec("idemio_lock_timeouts_total",
		"Claims abandoned waiting for the resource lock. Nothing was written; the agent "+
			"received a retryable 503.", "resource_type")
	m.SerializationWaits = m.counterVec("idemio_serialization_waits_total",
		"Same-agent conflicts that waited for the earlier write to finish, by outcome.", "outcome")
	m.LockWait = m.histogramVecWith("idemio_lock_wait_seconds",
		"Time spent acquiring the per-resource advisory lock. The p99 is one of the two "+
			"ADR-0010 routing triggers.",
		[]float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
		"resource_type")

	m.ManifestReloadFailures = m.counter("idemio_manifest_reload_failures_total",
		"Manifest reloads rejected by validation. The previous version stays live.")
	m.ManifestInfo = m.gaugeVec("idemio_manifest_info",
		"Always 1, labelled with the manifest version this process is serving.", "version")

	m.RelayPublished = m.counter("idemio_relay_published_total",
		"Intents published to the broker. Publication is at-least-once.")
	m.RelayFailures = m.counter("idemio_relay_failures_total",
		"Relay cycles that failed to publish. The write path is unaffected by design.")
	m.PartitionsCreated = m.counterVec("idemio_partitions_created_total",
		"Range partitions created ahead of need, by table.", "table")
	m.RetentionDeleted = m.counterVec("idemio_retention_deleted_total",
		"Rows deleted or partitions dropped by the retention sweep, by table.", "table")
	m.ArchivedPartitions = m.counterVec("idemio_archived_partitions_total",
		"Partitions exported to object storage before being dropped, by table.", "table")

	staleAfter := m.gauge("idemio_reconcile_stale_after_seconds",
		"The configured reconcile.stale_after. Alert rules compare the pending age against "+
			"this rather than restating the threshold.")
	staleAfter.Set(cfg.ReconcileStaleAfter.Seconds())

	m.registry.MustRegister(&databaseCollector{pool: pool, cfg: cfg, logger: logger})
	m.names = append(m.names,
		indeterminateKeysName, pendingKeysName, oldestPendingAgeName, partitionHeadroomName,
		unpublishedIntentsName, relayLagName, retentionLagName)

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

func (m *Metrics) gauge(name, help string) prometheus.Gauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: help})
	m.registry.MustRegister(g)
	m.names = append(m.names, name)
	return g
}

func (m *Metrics) histogram(name, help string, buckets []float64) prometheus.Histogram {
	h := prometheus.NewHistogram(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets})
	m.registry.MustRegister(h)
	m.names = append(m.names, name)
	return h
}

func (m *Metrics) counterVec(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
	m.registry.MustRegister(c)
	m.names = append(m.names, name)
	return c
}

func (m *Metrics) gaugeVec(name, help string, labels ...string) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
	m.registry.MustRegister(g)
	m.names = append(m.names, name)
	return g
}

func (m *Metrics) histogramVecWith(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    name,
		Help:    help,
		Buckets: buckets,
	}, labels)
	m.registry.MustRegister(h)
	m.names = append(m.names, name)
	return h
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
	indeterminateKeysName  = "idemio_indeterminate_keys"
	pendingKeysName        = "idemio_pending_keys"
	oldestPendingAgeName   = "idemio_oldest_pending_age_seconds"
	partitionHeadroomName  = "idemio_partition_headroom_seconds"
	unpublishedIntentsName = "idemio_unpublished_intents"
	relayLagName           = "idemio_relay_lag_seconds"
	retentionLagName       = "idemio_retention_lag_seconds"
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
	unpublishedIntents = prometheus.NewDesc(
		unpublishedIntentsName,
		"Intents not yet published to the broker. Read from the outbox watermark, so a relay "+
			"that has stopped is visible from either binary.",
		nil, nil)
	relayLag = prometheus.NewDesc(
		relayLagName,
		"Age of the oldest unpublished intent.",
		nil, nil)
	retentionLag = prometheus.NewDesc(
		retentionLagName,
		"How far past its hot window the oldest surviving row is. Sustained growth means the "+
			"sweep is losing to ingest.",
		[]string{"table"}, nil)
)

type databaseCollector struct {
	pool   *pgxpool.Pool
	cfg    config.Config
	logger *slog.Logger
}

func (c *databaseCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- indeterminateKeys
	ch <- pendingKeys
	ch <- oldestPendingAge
	ch <- partitionHeadroom
	ch <- unpublishedIntents
	ch <- relayLag
	ch <- retentionLag
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

	c.collectOutbox(ctx, ch)
	c.collectRetentionLag(ctx, ch)
}

func (c *databaseCollector) collectOutbox(ctx context.Context, ch chan<- prometheus.Metric) {
	const outbox = `
		SELECT count(*),
		       coalesce(extract(epoch FROM now() - min(emitted_at)), 0)
		  FROM write_intents
		 WHERE published_at IS NULL`

	var backlog, lag float64
	if err := c.pool.QueryRow(ctx, outbox).Scan(&backlog, &lag); err != nil {
		c.logger.Error("outbox gauges unavailable this scrape", "error", err)
		return
	}
	ch <- prometheus.MustNewConstMetric(unpublishedIntents, prometheus.GaugeValue, backlog)
	ch <- prometheus.MustNewConstMetric(relayLag, prometheus.GaugeValue, lag)
}

func (c *databaseCollector) collectRetentionLag(ctx context.Context, ch chan<- prometheus.Metric) {
	windows := map[string]struct {
		column string
		hot    time.Duration
	}{
		"idempotency_keys":     {"created_at", c.cfg.RetentionKeys},
		"write_intents":        {"emitted_at", c.cfg.RetentionIntents},
		"conflicts":            {"detected_at", c.cfg.RetentionConflicts},
		"payload_access_audit": {"accessed_at", c.cfg.RetentionAudit},
	}

	for table, window := range windows {
		stmt := fmt.Sprintf(
			"SELECT coalesce(extract(epoch FROM (now() - make_interval(secs => $1)) - min(%s)), 0) FROM %s",
			window.column, table)

		var lag float64
		if err := c.pool.QueryRow(ctx, stmt, window.hot.Seconds()).Scan(&lag); err != nil {
			c.logger.Error("retention lag unavailable this scrape", "table", table, "error", err)
			continue
		}
		if lag < 0 {
			lag = 0
		}
		ch <- prometheus.MustNewConstMetric(retentionLag, prometheus.GaugeValue, lag, table)
	}
}
