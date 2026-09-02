# Roadmap

Owns phasing, sequencing, and exit criteria. Extends PRD §16, which named four phases but
gave no gate for leaving any of them.

A phase is not complete when its features exist. It is complete when its **exit criteria**
are demonstrated. Each criterion below is written so that it can be answered yes or no.

## Status

| Phase | State |
|---|---|
| Phase 0 — core guarantee | **In progress.** Criteria 1–4 demonstrated against the fake downstream's execution ledger. 5 measured at floor, not at volume. 6 has rules as code but no drill. Everything outstanding needs a deployment target, not code. |
| Phase 1 — intent log and conflict detection | **In progress.** Criteria 1–4, 6 and 7 demonstrated; 5 demonstrated for the write-path half only. Conflict enforcement ships off by default. |
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
   *Floor measured on one machine at 300 sequential writes
   (`go test -tags latency ./internal/api/`). With Phase 1's conflict detection in the
   path: p50 8.4ms, p99 10.6ms across distinct resources, and p50 14.9ms, p99 19.9ms when
   every write targets one `resource_id`. Phase 0 measured 4.1ms / 5.9ms before the resource
   lock and conflict window existed, so conflict detection roughly doubles the floor and a
   hot resource doubles it again. That is the best case — no contention between clients, no
   network between tiers. It does not satisfy the criterion, but a failure here would have
   ruled it out.*
6. `indeterminate` alerting is live and has been fired at least once in a drill.
   *Rules exist as code in `deploy/alerts.yml` and are tested to reference only metrics the
   process really exports. Neither a pager nor a drill exists.*

## Phase 1 — intent log and conflict detection

Adds the second write path, at moderate volume.

**Build**

- Conflict check under an advisory lock taken as the claim transaction's **first**
  statement, with intents voided when the write provably did not happen, and same-agent
  conflicts serialized on the outcome rather than on the check
  ([ADR-0008](decisions/0008-serialization-via-advisory-locks.md),
  [ADR-0015](decisions/0015-conflict-check-transaction-shape.md)).
- Operation manifest: JSON on disk, validated whole, polled for changes, activations
  recorded ([ADR-0007](decisions/0007-operation-compatibility-matrix.md),
  [ADR-0013](decisions/0013-phase-1-implementation-stack.md)). An undeclared operation is
  rejected at admission
  ([ADR-0014](decisions/0014-undeclared-operations-rejected-at-admission.md)).
- `GET /v1/resources/{type}/{id}/intents` and `GET /v1/conflicts`, with role scoping,
  redaction, mandatory bounded ranges and keyset paging
  ([ADR-0011](decisions/0011-read-api-access-control.md),
  [ADR-0017](decisions/0017-read-api-time-bounds-are-mandatory.md)).
- Outbox relay to Kafka in its own binary, polling the unpublished watermark, with lag
  monitoring ([ADR-0001](decisions/0001-postgres-primary-intent-log.md)).
- Partition maintenance in application code rather than `pg_partman`
  ([ADR-0016](decisions/0016-partition-maintenance-in-application-code.md)), the retention
  sweep, and the Parquet archive path.
- PgBouncer in transaction mode — a Phase 1 requirement, not a Phase 2 optimisation.
- Lock-wait, conflict and claim-collision metrics, instrumented **before** they are needed,
  so the Phase 2 decision has history to read.

**Conflict enforcement ships off.** Each manifest declares `enforce`, and until it is set
the check runs in full, records `conflicts` rows with `resolution = 'observed'` and drives
every metric, but rejects nothing. A wrong manifest surfaces as mass rejection, so
onboarding means watching what the matrix would have done to real traffic and then flipping
one field ([ADR-0013](decisions/0013-phase-1-implementation-stack.md)).

**Exit criteria**

1. Two agents writing incompatibly to one resource within the window: one is rejected with
   `409` and a `conflicts` row is written.
   *Demonstrated. The rejected write shows **zero** executions in the fake downstream's
   ledger, and the winner exactly one.*
2. Two agents writing **compatibly** (disjoint scope selectors, or two `append`s) both
   succeed. This is the criterion that proves the matrix is doing real work rather than
   acting as a per-resource mutex.
   *Demonstrated for all three compatible shapes — two appends, an append beside a mutate,
   and two mutates with disjoint scopes — each with two executions in the ledger and no
   `conflicts` row.*
3. Same-agent conflicts serialize and complete, with `resolution = 'serialized'` recorded.
   *Demonstrated against the ledger's timestamps, not against the row: the first write is
   scripted slow and the second reaches the downstream only after it finishes. A test that
   only checked for the `serialized` row would have passed on an implementation where both
   writes executed simultaneously.*
4. Manifest reload takes effect without deploy, and the change appears in the audit log.
   *Demonstrated. A changed manifest is polled up and activated with no restart, an invalid
   one is rejected whole and leaves the previous version serving, and each activation is
   recorded once per process and version in `manifest_activations`.*
5. Outbox relay lag under one minute at steady state; Kafka taken down for an hour with
   **zero** write-path impact.
   *The write-path half is demonstrated: with the broker unreachable, publishing fails and
   writes continue to be claimed, executed and recorded, with the backlog intact for the
   next cycle. Intents are also shown reaching a real broker carrying their payloads.
   Steady-state lag over an hour needs a running deployment and continuous traffic; it is
   not measured.*
6. Archive restore drill: a detached, archived partition is restored and queried
   successfully. Cold data never read back is not archived.
   *Demonstrated end to end against object storage: an expired partition is detached,
   exported to Parquet, dropped, restored into a fresh table and queried, with payloads,
   scope selectors and timestamps intact. A partition whose export fails is left in place
   rather than dropped.*
7. Payload redaction verified — `operator` cannot obtain payloads by any parameter
   combination, and `investigator` access produces audit rows.
   *Demonstrated. Redaction is enforced by issuing a different query rather than by
   filtering results, so an unentitled request never causes the column to be read; the audit
   row commits in the same transaction as the read, so an unaudited payload cannot be
   returned; and the audit table is checked to contain no payload content of its own.*

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
