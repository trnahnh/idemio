# Metrics

This document owns the mapping from exported metric to the alert it serves. Metric names
appear here and nowhere else in `docs/`; [SYSTEM_DESIGN.md](SYSTEM_DESIGN.md) explains why
each signal exists and links here for what it is called.

**This file is verified, not maintained by hand.** `TestDocumentedMetricsMatchTheRegistry`
compares the `idemio_*` names in the table below against the names the process actually
registers, in both directions. Adding a metric without documenting it fails the build, and
so does documenting one that does not exist. A row in *Not yet exported* has no metric name,
so it is invisible to that check by construction.

Both binaries expose `/metrics`: `cmd/idemio` on `IDEMIO_LISTEN_ADDR`, `cmd/reconciler` on
`IDEMIO_METRICS_ADDR`. The gauges are read live from the database on each scrape rather than
incremented, so a restart cannot lose them and two replicas cannot double-count.

## Correctness and safety

| Metric | Type | Labels | Alert | Source |
|---|---|---|---|---|
| `idemio_indeterminate_keys` | gauge | — | **Page on any sustained non-zero value.** The correct value is zero; each key is a possible unresolved side effect requiring a human. | [ADR-0005](decisions/0005-downstream-outcome-taxonomy.md), [ADR-0006](decisions/0006-reconciliation-never-resumes.md) |
| `idemio_oldest_pending_age_seconds` | gauge | — | **Page** above `reconcile.stale_after`. The reconciler is not keeping up. The threshold must be kept in step with the configured value; see the gap noted below. | [ADR-0006](decisions/0006-reconciliation-never-resumes.md) |
| `idemio_pending_keys` | gauge | — | Informational. Context for the age gauge — one very old key reads differently from thousands. | [ADR-0006](decisions/0006-reconciliation-never-resumes.md) |
| `idemio_probe_failures_total` | counter | `resource_type` | Warn. Crash recovery for that path is degrading toward manual. Failures leave keys `pending` rather than escalating, so this rising while `idemio_oldest_pending_age_seconds` climbs is one incident, not two. | [ADR-0006](decisions/0006-reconciliation-never-resumes.md) |
| `idemio_reconciled_total` | counter | `outcome` (`done`, `failed`, `indeterminate`) | Warn on rising `indeterminate`. This is the reconciler's own account of what it escalated. | [ADR-0006](decisions/0006-reconciliation-never-resumes.md) |
| `idemio_reclaims_total` | counter | — | Warn on a rising rate; suggests a flapping downstream. **Does not fully serve its signal** — see *Known gaps*. | [ADR-0002](decisions/0002-key-scope-and-status-lifecycle.md) |
| `idemio_writes_total` | counter | `status` (`done`, `failed`, `indeterminate`) | The rate of `indeterminate` is the leading indicator; `idemio_indeterminate_keys` is the standing debt. | [ADR-0005](decisions/0005-downstream-outcome-taxonomy.md) |

## Contract health

| Metric | Type | Labels | Alert | Source |
|---|---|---|---|---|
| `idemio_hash_mismatches_total` | counter | `agent_id` | Warn. An agent is reusing keys across different request bodies — a client bug, never silently executed. Carries an `agent_id` label because the signal is per-agent; see *Known gaps* on cardinality. | [ADR-0003](decisions/0003-canonical-request-hashing.md) |
| `idemio_responses_total` | counter | `code` | `202` rate is informational, but a rising trend means growing contention. `409` becomes meaningful in Phase 1. | [API_REFERENCE.md](API_REFERENCE.md) |
| `idemio_replays_total` | counter | — | Informational. A high replay rate is the system working, not failing. | [ADR-0004](decisions/0004-concurrent-claim-resolution.md) |

## Capacity

| Metric | Type | Labels | Alert | Source |
|---|---|---|---|---|
| `idemio_claim_collisions_total` | counter | — | Warn above 1% of writes — one of the two ADR-0010 routing triggers. Counts every claim that hit an existing row, however it was then resolved. | [ADR-0010](decisions/0010-consistent-hash-routing-not-raft.md) |
| `idemio_partition_headroom_seconds` | gauge | `table` | **Page** below four weeks. A missing partition is a hard write-path outage, and `cmd/idemio` refuses to boot below two weeks. | [ADR-0009](decisions/0009-partitioning-and-retention.md) |
| `idemio_downstream_duration_seconds` | histogram | `disposition` (`done`, `failed`, `indeterminate`) | Feeds the SYSTEM_DESIGN latency budget. Splitting by disposition keeps timeouts from flattering the success percentiles. | [SYSTEM_DESIGN.md](SYSTEM_DESIGN.md) |
| `idemio_oversized_results_total` | counter | — | Informational in Phase 0. Phase 0 stores results inline regardless of size; this counts those over `limits.result_inline_bytes` so Phase 1 sets the cap from data rather than from the current guess. | [ADR-0012](decisions/0012-phase-0-implementation-stack.md) |

## Not yet exported

These signals are specified in SYSTEM_DESIGN but have no metric, because the thing they
measure does not exist yet. They are listed so their absence is a recorded decision rather
than an oversight.

| Signal | Blocked on | Phase |
|---|---|---|
| `409` rate per `agent_id` and `resource_type` | Conflict detection. Must be live **before** conflict detection is enabled, since a bad manifest surfaces as mass rejection. | 1 |
| p99 advisory lock wait | Serialization under advisory locks. The other ADR-0010 routing trigger, at 50ms. | 1 |
| Outbox relay lag | The Kafka relay. | 1 |
| Key expiry sweep lag vs. ingest | The retention sweep. If it falls behind ingest the hot table grows without bound. | 1 |

## Known gaps

**`idemio_reclaims_total` does not answer its own question.** SYSTEM_DESIGN asks for re-claims
per key so a *high tail* can be alerted on — one key retried forty times is a flapping
downstream, forty keys retried once each is normal. The counter is global and has no
distribution, so the tail is invisible. Fixing it means a histogram over `attempt_count` at
re-claim time. Recorded rather than quietly left as though the signal were covered.

**`idemio_oldest_pending_age_seconds` duplicates a threshold.** The alert compares the gauge
against `reconcile.stale_after`, which lives in the process configuration. Nothing checks the
two agree, so raising the configured value without editing the alert rule produces a page on
every healthy in-flight write. Exporting the configured value as its own gauge would let the
rule reference it instead of restating it.

**`idemio_hash_mismatches_total` is labelled by `agent_id`.** SYSTEM_DESIGN asks for the
`422` rate per agent and this is the honest reading of that. It is also unbounded cardinality
in a metric an untrusted caller influences. Phase 0 has one agent path; this must be revisited
before Phase 3 onboards the rest.
