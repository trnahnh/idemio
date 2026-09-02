# Metrics

This document owns the mapping from exported metric to the alert it serves. Metric names
appear here and nowhere else in `docs/`; [SYSTEM_DESIGN.md](SYSTEM_DESIGN.md) explains why
each signal exists and links here for what it is called.

**This file is verified, not maintained by hand.** `TestDocumentedMetricsMatchTheRegistry`
compares the `idemio_*` names in the table below against the names the process actually
registers, in both directions. Adding a metric without documenting it fails the build, and
so does documenting one that does not exist. A row in *Not yet exported* has no metric name,
so it is invisible to that check by construction.

All three binaries expose `/metrics`: `cmd/idemio` on `IDEMIO_LISTEN_ADDR`,
`cmd/reconciler` and `cmd/relay` on `IDEMIO_METRICS_ADDR`. The gauges are read live from the database on each scrape rather than
incremented, so a restart cannot lose them and two replicas cannot double-count.

## Correctness and safety

| Metric | Type | Labels | Alert | Source |
|---|---|---|---|---|
| `idemio_indeterminate_keys` | gauge | — | **Page on any sustained non-zero value.** The correct value is zero; each key is a possible unresolved side effect requiring a human. | [ADR-0005](decisions/0005-downstream-outcome-taxonomy.md), [ADR-0006](decisions/0006-reconciliation-never-resumes.md) |
| `idemio_oldest_pending_age_seconds` | gauge | — | **Page** when it exceeds `idemio_reconcile_stale_after_seconds`. The reconciler is not keeping up. The rule compares two metrics rather than restating the threshold, so raising the configured value cannot leave a stale alert behind. | [ADR-0006](decisions/0006-reconciliation-never-resumes.md) |
| `idemio_pending_keys` | gauge | — | Informational. Context for the age gauge — one very old key reads differently from thousands. | [ADR-0006](decisions/0006-reconciliation-never-resumes.md) |
| `idemio_reconcile_stale_after_seconds` | gauge | — | Never alerted on directly. Exported so the stale-`pending` rule can reference the configured threshold instead of duplicating it. | [ADR-0006](decisions/0006-reconciliation-never-resumes.md) |
| `idemio_probe_failures_total` | counter | `resource_type` | Warn. Crash recovery for that path is degrading toward manual. Failures leave keys `pending` rather than escalating, so this rising while `idemio_oldest_pending_age_seconds` climbs is one incident, not two. | [ADR-0006](decisions/0006-reconciliation-never-resumes.md) |
| `idemio_reconciled_total` | counter | `outcome` (`done`, `failed`, `indeterminate`) | Warn on rising `indeterminate`. This is the reconciler's own account of what it escalated. | [ADR-0006](decisions/0006-reconciliation-never-resumes.md) |
| `idemio_reclaim_attempts` | histogram | — | Warn on a rising **tail**, not on the rate. One key re-claimed forty times is a flapping downstream; forty keys re-claimed once each is normal traffic. `_count` still gives the rate. | [ADR-0002](decisions/0002-key-scope-and-status-lifecycle.md) |
| `idemio_writes_total` | counter | `status` (`done`, `failed`, `indeterminate`) | The rate of `indeterminate` is the leading indicator; `idemio_indeterminate_keys` is the standing debt. | [ADR-0005](decisions/0005-downstream-outcome-taxonomy.md) |

## Contract health

| Metric | Type | Labels | Alert | Source |
|---|---|---|---|---|
| `idemio_hash_mismatches_total` | counter | `agent_id` | Warn. An agent is reusing keys across different request bodies — a client bug, never silently executed. Carries an `agent_id` label because the signal is per-agent; see *Known gaps* on cardinality. | [ADR-0003](decisions/0003-canonical-request-hashing.md) |
| `idemio_responses_total` | counter | `code` | `202` rate is informational, but a rising trend means growing contention. A rising `409` rate after enabling enforcement is the expected shape of a wrong manifest. | [API_REFERENCE.md](API_REFERENCE.md) |
| `idemio_replays_total` | counter | — | Informational. A high replay rate is the system working, not failing. | [ADR-0004](decisions/0004-concurrent-claim-resolution.md) |

## Capacity

| Metric | Type | Labels | Alert | Source |
|---|---|---|---|---|
| `idemio_claim_collisions_total` | counter | — | Warn above 1% of writes — one of the two ADR-0010 routing triggers. Counts every claim that hit an existing row, however it was then resolved. | [ADR-0010](decisions/0010-consistent-hash-routing-not-raft.md) |
| `idemio_partition_headroom_seconds` | gauge | `table` | **Page** below four weeks. A missing partition is a hard write-path outage, and `cmd/idemio` refuses to boot below two weeks. | [ADR-0009](decisions/0009-partitioning-and-retention.md) |
| `idemio_downstream_duration_seconds` | histogram | `disposition` (`done`, `failed`, `indeterminate`) | Feeds the SYSTEM_DESIGN latency budget. Splitting by disposition keeps timeouts from flattering the success percentiles. | [SYSTEM_DESIGN.md](SYSTEM_DESIGN.md) |
| `idemio_oversized_results_total` | counter | — | Informational in Phase 0. Phase 0 stores results inline regardless of size; this counts those over `limits.result_inline_bytes` so Phase 1 sets the cap from data rather than from the current guess. | [ADR-0012](decisions/0012-phase-0-implementation-stack.md) |

## Conflict detection

| Metric | Type | Labels | Alert | Source |
|---|---|---|---|---|
| `idemio_conflicts_total` | counter | `resource_type`, `resolution` (`rejected`, `serialized`, `observed`) | **Page** on `rejected` rising sharply after an enforcement change — a wrong manifest surfaces as mass rejection, which is an availability incident. `observed` is shadow mode and rejects nothing; it is the signal to read *before* enabling enforcement. | [ADR-0007](decisions/0007-operation-compatibility-matrix.md), [ADR-0013](decisions/0013-phase-1-implementation-stack.md) |
| `idemio_lock_wait_seconds` | histogram | `resource_type` | Warn when the p99 exceeds 50ms — one of the two ADR-0010 routing triggers. Measures the whole claim path, since the lock is now the transaction's first statement. | [ADR-0010](decisions/0010-consistent-hash-routing-not-raft.md), [ADR-0015](decisions/0015-conflict-check-transaction-shape.md) |
| `idemio_lock_timeouts_total` | counter | `resource_type` | Warn. Each one is a `503` on a hot resource. Nothing was written and nothing was sent downstream, so it is a throughput signal, not a correctness one. | [ADR-0015](decisions/0015-conflict-check-transaction-shape.md) |
| `idemio_serialization_waits_total` | counter | `outcome` (`resolved`, `timeout`) | Warn on rising `timeout`. A same-agent write that waited out its budget becomes a `503`; a rising ratio means one agent is writing to one resource faster than the downstream can absorb. | [ADR-0015](decisions/0015-conflict-check-transaction-shape.md) |
| `idemio_manifest_info` | gauge | `version` | Never alerted on directly. Always 1, labelled with the manifest each process is serving — the way to see a rollout half-applied across replicas. | [ADR-0013](decisions/0013-phase-1-implementation-stack.md) |
| `idemio_manifest_reload_failures_total` | counter | — | **Page** on any sustained value. The process keeps serving the last valid manifest, so nothing breaks immediately; what breaks is that manifest changes have silently stopped taking effect. | [ADR-0013](decisions/0013-phase-1-implementation-stack.md) |

## Relay, retention, and partitions

| Metric | Type | Labels | Alert | Source |
|---|---|---|---|---|
| `idemio_relay_lag_seconds` | gauge | — | Warn above one minute at steady state, per the Phase 1 exit criterion. Read from the outbox watermark, so a relay that has stopped is visible from any binary rather than only from its own. | [ADR-0001](decisions/0001-postgres-primary-intent-log.md) |
| `idemio_unpublished_intents` | gauge | — | Informational. Context for the lag gauge: a large backlog draining steadily reads differently from a small one that is stuck. | [ADR-0001](decisions/0001-postgres-primary-intent-log.md) |
| `idemio_relay_published_total` | counter | — | Informational. Publication is at-least-once, so this exceeds the number of distinct intents. | [ADR-0001](decisions/0001-postgres-primary-intent-log.md) |
| `idemio_relay_failures_total` | counter | — | Warn only. The relay is off the write path by construction, so a failing broker is a data-pipeline incident and never a write-path one. | [ADR-0001](decisions/0001-postgres-primary-intent-log.md) |
| `idemio_retention_lag_seconds` | gauge | `table` | **Page** on sustained growth. The sweep is rate-matched to ingest; if it loses, the hot table grows without bound and no other signal says so. | [ADR-0009](decisions/0009-partitioning-and-retention.md) |
| `idemio_retention_deleted_total` | counter | `table` | Informational. Zero while the lag gauge climbs means the sweep is not running at all. | [ADR-0009](decisions/0009-partitioning-and-retention.md) |
| `idemio_partitions_created_total` | counter | `table` | Informational, but the counter that explains a healthy headroom gauge. Headroom staying flat with this at zero means the maintainer has stopped and existing partitions are merely not exhausted yet. | [ADR-0016](decisions/0016-partition-maintenance-in-application-code.md) |
| `idemio_archived_partitions_total` | counter | `table` | Informational. A partition is dropped only after its export succeeds, so this and `idemio_retention_deleted_total` move together for range tables. | [ADR-0009](decisions/0009-partitioning-and-retention.md) |

## Not yet exported

These signals are specified in SYSTEM_DESIGN but have no metric, because the thing they
measure does not exist yet. They are listed so their absence is a recorded decision rather
than an oversight.

| Signal | Blocked on | Phase |
|---|---|---|
| Per-agent rate limiting and `429` | Phase 3. `429` is reserved in [API_REFERENCE.md](API_REFERENCE.md) so adding it is not a breaking change. | 3 |

Every Phase 1 signal previously listed here is now exported: the conflict rate (labelled by
`resource_type` rather than `agent_id`, see below), lock wait, relay lag, and retention lag.

## Known gaps

**Per-agent detail is not in Prometheus, by decision.** SYSTEM_DESIGN asks for rejection and
mismatch rates per agent. Both are labelled by `resource_type` instead, because an agent
identifier is caller-controlled and unbounded, and a metric an untrusted caller can expand
is a cardinality incident waiting for its first bad deploy. The per-agent question is
answered by `GET /v1/conflicts`, which is indexed on `(agent_id_a, detected_at)` and can
answer questions a counter cannot ([ADR-0013](decisions/0013-phase-1-implementation-stack.md)).
The cost is that per-agent alerting requires a query rather than a rule, and that is the
right trade at this cardinality.
