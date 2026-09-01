# Roadmap

Owns phasing, sequencing, and exit criteria. Extends PRD §16, which named four phases but
gave no gate for leaving any of them.

A phase is not complete when its features exist. It is complete when its **exit criteria**
are demonstrated. Each criterion below is written so that it can be answered yes or no.

## Status

| Phase | State |
|---|---|
| Phase 0 — core guarantee | **Not started.** Repository contains documentation only. |
| Phase 1 — intent log and conflict detection | Not started |
| Phase 2 — horizontal scale-out | Not started |
| Phase 3 — full onboarding and abuse detection | Not started |

## Phase 0 — prove the core guarantee on one low-risk write path

Single-region, single write path. No broker, no routing, no conflict detection.

**Build**

- Schema from [DATA_MODEL.md](DATA_MODEL.md), **created partitioned from the first
  migration**. Phase 0 volume does not need partitioning; converting a populated table
  later is far more disruptive than starting partitioned
  ([ADR-0009](decisions/0009-partitioning-and-retention.md)).
- `POST /v1/writes` and `GET /v1/writes/{key}`.
- Canonical request hashing ([ADR-0003](decisions/0003-canonical-request-hashing.md)).
- Claim transaction with `ON CONFLICT DO NOTHING`; `202` for the race loser
  ([ADR-0004](decisions/0004-concurrent-claim-resolution.md)).
- Outcome classification for one integration
  ([ADR-0005](decisions/0005-downstream-outcome-taxonomy.md)).
- Reconciler with no downstream write path, plus a probe for the chosen integration
  ([ADR-0006](decisions/0006-reconciliation-never-resumes.md)).
- Correctness and contract-health metrics from SYSTEM_DESIGN §Observability.

**Explicitly deferred:** conflict detection, `write_intents` reads, Kafka, the outbox
relay, consistent-hash routing, rate limiting.

Intents are still **written** in Phase 0 — the table and the transaction shape exist from
day one — they are simply not yet read by a conflict check. This keeps Phase 1 from
requiring a change to the write transaction.

**Exit criteria**

1. A load test issuing duplicate keys concurrently produces exactly one downstream
   execution per key, verified against downstream records, not against this system's logs.
2. A kill test — SIGKILL the replica mid-downstream-call — leaves the key `pending`, and
   the reconciler resolves it by probe or escalates to `indeterminate`. It never
   re-executes. Verified downstream.
3. A retry with a re-serialized body (reordered keys, `4200.0` for `4200`) replays rather
   than returning `422`.
4. A business failure from downstream replays identically on retry.
5. p50 overhead under 15ms and p99 under 60ms at the Phase 0 write path's real volume.
6. `indeterminate` alerting is live and has been fired at least once in a drill.

## Phase 1 — intent log and conflict detection

Adds the second write path, at moderate volume.

**Build**

- Conflict check under advisory lock
  ([ADR-0008](decisions/0008-serialization-via-advisory-locks.md)).
- Operation manifest, schema-validated, hot-reloadable
  ([ADR-0007](decisions/0007-operation-compatibility-matrix.md)).
- `GET /v1/resources/{type}/{id}/intents` and `GET /v1/conflicts`, with role scoping and
  redaction ([ADR-0011](decisions/0011-read-api-access-control.md)).
- Outbox relay to Kafka, with lag monitoring
  ([ADR-0001](decisions/0001-postgres-primary-intent-log.md)).
- `pg_partman`, the retention sweep, and the archive path.
- PgBouncer in transaction mode — a Phase 1 requirement, not a Phase 2 optimisation.
- Lock-wait and claim-collision metrics, instrumented **before** they are needed, so the
  Phase 2 decision has history to read.

**Exit criteria**

1. Two agents writing incompatibly to one resource within the window: one is rejected with
   `409` and a `conflicts` row is written.
2. Two agents writing **compatibly** (disjoint scope selectors, or two `append`s) both
   succeed. This is the criterion that proves the matrix is doing real work rather than
   acting as a per-resource mutex.
3. Same-agent conflicts serialize and complete, with `resolution = 'serialized'` recorded.
4. Manifest reload takes effect without deploy, and the change appears in the audit log.
5. Outbox relay lag under one minute at steady state; Kafka taken down for an hour with
   **zero** write-path impact.
6. Archive restore drill: a detached, archived partition is restored and queried
   successfully. Cold data never read back is not archived.
7. Payload redaction verified — `operator` cannot obtain payloads by any parameter
   combination, and `investigator` access produces audit rows.

## Phase 2 — horizontal scale-out

**Build**

- Replica scale-out under HPA, with load tests at the PRD §12 target of 2,000 writes/sec.
- Read replicas for `GET` endpoints.
- Consistent-hash key affinity **only if** the ADR-0010 trigger fires: sustained p99 lock
  wait above 50ms, or claim collisions above 1% of writes, at or above 70% of target
  throughput.

Raft is not built. See [ADR-0010](decisions/0010-consistent-hash-routing-not-raft.md).

**Exit criteria**

1. 2,000 writes/sec sustained per region with the latency targets still met.
2. Killing a replica under load causes no failed writes and no double executions.
3. Either the routing trigger has not fired (and the metrics prove it), or routing is
   deployed and the trigger metric has returned below threshold.
4. Partition pre-creation headroom alerting has been drilled.

## Phase 3 — full onboarding and abuse detection

**Build**

- Remaining agent-driven write paths, each with a manifest, an error classification, and a
  probe — or a recorded, signed-off acceptance that its crash recovery is manual
  ([ADR-0006](decisions/0006-reconciliation-never-resumes.md)).
- Per-agent rate limiting (`429`, currently reserved in
  [API_REFERENCE.md](API_REFERENCE.md)).
- Rate-based abuse detection, promoted from PRD §10's dashboard-only treatment to
  enforcement.

**Exit criteria**

1. Every onboarded `resource_type` has a manifest and either a probe or a recorded
   acceptance of manual recovery.
2. Rate limiting demonstrably contains a runaway agent in a game day, without affecting
   other agents.
3. No write path bypasses the layer — verified from the downstream side, by confirming
   that writes arrive only from this middleware.

## Sequencing notes

**What moved earlier than PRD §16.** Partitioning, the outbox column, and intent writes all
land in Phase 0 despite being Phase 1+ concerns by volume, because they are cheap to build
first and expensive to retrofit. The rule applied throughout: anything that changes the
shape of the claim transaction or the schema goes in Phase 0 even if it is not yet used.

**What moved later.** Kafka moves from Phase 1's critical path to a Phase 1 asynchronous
consumer. Raft moves out of the roadmap entirely.

**The gate that matters most** is Phase 0 exit criterion 2, the kill test verified against
downstream records. It is the only criterion that directly tests the property the entire
system exists to provide, and it is the one most easily satisfied on paper without being
satisfied in fact.
