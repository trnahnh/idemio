# PRD: idempotent transaction layer for agent-driven writes

*Status: **frozen at v1.0** | Owner: [fill in] | Last updated: 2026-09-01*

> **This document is frozen and is no longer the specification.**
>
> It records the original intent and is preserved unedited for that purpose. The living
> specification is [`docs/`](docs/), which supersedes this document wherever they differ.
>
> Several sections below contain contradictions or gaps that were found during review and
> resolved as [Architecture Decision Records](docs/decisions/). Read the ADR before acting
> on these sections:
>
> | Section | Superseded by |
> |---|---|
> | §6 architecture (Kafka intent log, Raft group) | [ADR-0001](docs/decisions/0001-postgres-primary-intent-log.md), [ADR-0010](docs/decisions/0010-consistent-hash-routing-not-raft.md) |
> | §7 data model (global key PK, incomplete status enum, unpartitioned tables) | [ADR-0002](docs/decisions/0002-key-scope-and-status-lifecycle.md), [ADR-0009](docs/decisions/0009-partitioning-and-retention.md), [DATA_MODEL.md](docs/DATA_MODEL.md) |
> | §8 API (five status codes; several reachable states unrepresented) | [API_REFERENCE.md](docs/API_REFERENCE.md) |
> | §9 write flow (intent-before-key ordering, undefined hashing) | [ADR-0001](docs/decisions/0001-postgres-primary-intent-log.md), [ADR-0003](docs/decisions/0003-canonical-request-hashing.md) |
> | §10 conflict rules (blanket rule, unspecified serialization) | [ADR-0007](docs/decisions/0007-operation-compatibility-matrix.md), [ADR-0008](docs/decisions/0008-serialization-via-advisory-locks.md) |
> | §11 HA and consensus (Raft) | [ADR-0010](docs/decisions/0010-consistent-hash-routing-not-raft.md) |
> | §13 failure modes (reconciliation "resumes"; single 503) | [ADR-0005](docs/decisions/0005-downstream-outcome-taxonomy.md), [ADR-0006](docs/decisions/0006-reconciliation-never-resumes.md) |
> | §14 security (read endpoints unprotected; `result` unclassified) | [ADR-0011](docs/decisions/0011-read-api-access-control.md) |
> | §18 open questions | All four answered; see [OPEN_QUESTIONS.md](docs/OPEN_QUESTIONS.md) |

---

## 1. Summary

AI agents (and other automated clients) increasingly issue writes directly against production databases and APIs. Unlike normal client retries, agent retries can be triggered by ambiguous model reasoning, timeouts, or crash recovery, and agents can also issue writes that conflict with each other when running concurrently. This system is a middleware layer that sits between agents and the system of record. It guarantees that a given logical write is executed at most once, gives agents safe retry semantics, records every attempted write for audit, and detects and blocks conflicting or out-of-scope writes before they reach the database.

## 2. Problem statement

- Agents retry write calls after timeouts or ambiguous responses, causing duplicate side effects (double charges, duplicate rows, duplicate emails).
- Multiple agents (or multiple runs of the same agent) can issue conflicting writes to the same resource concurrently, with no coordination layer today.
- There is no audit trail of what an agent attempted, only what succeeded, which makes debugging and incident response slow.
- Existing idempotency patterns (e.g. Stripe-style idempotency keys) solve single-write deduplication but do not address cross-write conflict detection or agent-specific abuse patterns.

## 3. Goals

- Guarantee at-most-once execution for any write submitted with an idempotency key.
- Provide a durable, queryable log of every write attempt (intent), independent of whether it succeeded.
- Detect conflicting writes to the same resource within a configurable time window and either serialize or reject them.
- Add less than 15ms p50 latency overhead versus calling the downstream system directly.
- Support horizontal scaling of the middleware tier without weakening the at-most-once guarantee.

## 4. Non-goals

- This system does not replace the downstream database's own transaction guarantees; it is a coordination layer in front of it.
- This system does not attempt semantic understanding of what a write "means" beyond resource id and operation type; deep semantic conflict detection is out of scope for v1.
- This system does not manage agent authentication or authorization; it assumes the caller is already authenticated upstream.

## 5. Personas and use cases

| Persona | Use case |
|---|---|
| Agent framework engineer | Wraps every tool call that performs a write with an idempotency key, so agent retries are automatically safe. |
| Platform / infra engineer | Operates the middleware tier, monitors conflict rate and rejected writes, tunes conflict-window parameters. |
| Incident responder | Queries the write-intent log to reconstruct what an agent attempted during an incident, independent of what succeeded. |
| Security / trust and safety | Reviews rejected writes flagged as out-of-scope or high-risk agent behavior. |

## 6. System architecture

The middleware runs as a stateless fleet of N replicas in front of the write-intent log and the idempotency-key store. Postgres is the source of truth for key state (this is what makes at-most-once possible even with multiple middleware replicas). A Raft group tracks which middleware node currently owns coordination responsibility for a given key range, used only for the HA/failover path described in section 11; the correctness guarantee itself comes from Postgres's transactional insert, not from Raft.

Rendered version: `architecture.png` (same folder). ASCII version below for direct parsing:

```
   [AI agent A]              [AI agent B]
        \                         /
         \   write request +    /
          \   idempotency key  /
           v                  v
  +----------------------------------------+
  |  IDEMPOTENCY MIDDLEWARE (Go/Rust)       |
  |  N stateless replicas                   |
  |                                         |
  |  [Middleware node 1] <--+               |
  |  [Middleware node 2] <--+--> [Raft group]
  |                             (key-range   |
  |                              ownership,  |
  |                              HA path     |
  |                              only, not   |
  |                              on the      |
  |                              correctness |
  |                              path)       |
  +------------------+-----------+----------+
                     |           |
          1. record  |           | 2. check /
             intent  |           |    write key
                     v           v
        +---------------------+  +------------------------+
        | Write-intent log    |  | Postgres                |
        | (Kafka / NATS)      |  | idempotency-key table    |
        +---------------------+  +------------+-------------+
                                               |
                                    3. execute write
                                       (only if new key)
                                               v
                                  +----------------------------+
                                  | Downstream DB / API         |
                                  | (source of truth)           |
                                  +----------------------------+
```

**Component responsibilities:**

| Component | Responsibility |
|---|---|
| Middleware node (Go or Rust) | Terminates agent connections, computes request hash, orchestrates the write flow, exposes the API in section 8. |
| Postgres idempotency-key table | Single source of truth for "has this key been seen, and what was the result." Correctness derives from a unique constraint plus a transactional insert-before-execute pattern. |
| Write-intent log (Kafka or NATS) | Append-only record of every write attempted, before execution. Used for audit, replay, and conflict windowing. |
| Raft group | Tracks key-range ownership across middleware replicas for the HA path only; not on the critical path for correctness. |
| Downstream DB / API | The actual system of record the write is ultimately applied to. |

## 7. Data model

Rendered version: `erd.png` (same folder). ASCII version below for direct parsing:

```
+-----------------------------------------------+
| idempotency_keys                               |
+-----------------------------------------------+
| PK  idempotency_key (uuid)                      |
|     agent_id (text)                             |
|     request_hash (text)                         |
|     status (enum: pending, done, rejected)      |
|     result (jsonb)                              |
|     created_at, completed_at (timestamptz)      |
+-----------------------------------------------+
                    | 1..N
                    v
+-----------------------------------------------+
| write_intents (log)                            |
+-----------------------------------------------+
| PK  intent_id (uuid)                            |
| FK  idempotency_key (uuid)                      |
|     resource_type, resource_id (text)           |
|     operation (text)                            |
|     payload (jsonb)                             |
|     emitted_at (timestamptz)                    |
+-----------------------------------------------+
                    | 0..N
                    v
+-----------------------------------------------+
| conflicts                                       |
+-----------------------------------------------+
| PK  conflict_id (uuid)                          |
| FK  intent_id_a, intent_id_b (uuid)             |
|     reason (text)                               |
|     resolution (enum: serialized, rejected,     |
|                 manual)                         |
|     detected_at (timestamptz)                   |
+-----------------------------------------------+
```

**DDL (Postgres):**

```sql
CREATE TYPE key_status AS ENUM ('pending', 'done', 'rejected');
CREATE TYPE conflict_resolution AS ENUM ('serialized', 'rejected', 'manual');

CREATE TABLE idempotency_keys (
    idempotency_key   UUID PRIMARY KEY,
    agent_id          TEXT NOT NULL,
    request_hash      TEXT NOT NULL,
    status            key_status NOT NULL DEFAULT 'pending',
    result            JSONB,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at      TIMESTAMPTZ
);
CREATE INDEX idx_idempotency_keys_agent ON idempotency_keys (agent_id, created_at);

CREATE TABLE write_intents (
    intent_id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key   UUID NOT NULL REFERENCES idempotency_keys(idempotency_key),
    resource_type     TEXT NOT NULL,
    resource_id       TEXT NOT NULL,
    operation         TEXT NOT NULL,
    payload           JSONB NOT NULL,
    emitted_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_write_intents_resource ON write_intents (resource_type, resource_id, emitted_at);

CREATE TABLE conflicts (
    conflict_id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    intent_id_a       UUID NOT NULL REFERENCES write_intents(intent_id),
    intent_id_b       UUID NOT NULL REFERENCES write_intents(intent_id),
    reason            TEXT NOT NULL,
    resolution        conflict_resolution NOT NULL,
    detected_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

## 8. API specification

### 8.1 POST /v1/writes

Submits a write request. This is the only endpoint agents call.

Request:
```
POST /v1/writes
Content-Type: application/json
Idempotency-Key: 7c9e6679-7425-40de-944b-e07fc1f90ae7

{
  "agent_id": "agent-checkout-flow",
  "resource_type": "invoice",
  "resource_id": "inv_8842",
  "operation": "create_charge",
  "payload": {
    "amount_cents": 4200,
    "currency": "usd",
    "customer_id": "cus_119"
  }
}
```

Response (new write, executed):
```
HTTP/1.1 201 Created

{
  "idempotency_key": "7c9e6679-7425-40de-944b-e07fc1f90ae7",
  "status": "done",
  "result": { "charge_id": "ch_5521", "status": "succeeded" },
  "replayed": false
}
```

Response (duplicate key, replayed):
```
HTTP/1.1 200 OK

{
  "idempotency_key": "7c9e6679-7425-40de-944b-e07fc1f90ae7",
  "status": "done",
  "result": { "charge_id": "ch_5521", "status": "succeeded" },
  "replayed": true
}
```

Response (conflict detected):
```
HTTP/1.1 409 Conflict

{
  "idempotency_key": "a1b2c3d4-...",
  "status": "rejected",
  "reason": "conflicting_write",
  "conflicting_intent_id": "int_9981",
  "conflict_id": "cf_442"
}
```

### 8.2 GET /v1/writes/{idempotency_key}

Polls the status of a previously submitted write. Useful when the original caller lost the response.

```
GET /v1/writes/7c9e6679-7425-40de-944b-e07fc1f90ae7

HTTP/1.1 200 OK
{
  "idempotency_key": "7c9e6679-7425-40de-944b-e07fc1f90ae7",
  "status": "done",
  "result": { "charge_id": "ch_5521", "status": "succeeded" }
}
```

### 8.3 GET /v1/resources/{resource_type}/{resource_id}/intents

Returns the write-intent history for a resource. Used by incident responders and the conflict-detection engine.

### 8.4 GET /v1/conflicts?since=...

Returns detected conflicts in a time window, for dashboards and alerting.

### 8.5 Error codes

| HTTP status | Meaning |
|---|---|
| 200 | Replayed result of a previously completed write |
| 201 | New write accepted and executed |
| 409 | Conflict detected, write rejected |
| 422 | Missing/malformed idempotency key, or request_hash mismatch on a reused key |
| 503 | Downstream system unavailable; write intent recorded but not yet executed, safe to retry |

## 9. Core write flow

Rendered version: `flow.png` (same folder). ASCII version below for direct parsing:

```
+---------------------------------+
| Agent sends write               |
| (idempotency_key, payload)      |
+-----------------+-----------------+
                  v
+---------------------------------+
| Look up idempotency_key          |
| in Postgres                      |
+-----------------+-----------------+
                  v
           +---------------+
           |  Key exists?  |
           +---+-------+---+
         yes|           |no
            v           v
+------------------+  +----------------------------+
| Return stored    |  | Append write-intent to      |
| result            |  | log (Kafka / NATS)          |
| (no re-exec)      |  +--------------+---------------+
+---------+---------+                 v
          |              +----------------------------+
          |              | Conflict check              |
          |              | (same resource + window)    |
          |              +------+---------------+-------+
          |         conflict    |               | clear
          |           found     |               |
          |                     v               v
          |         +--------------------+  +------------------------+
          |         | Reject / queue for |  | Insert key row          |
          |         | manual review      |  | (status=pending)        |
          |         +----------+---------+  | in same txn             |
          |                    |            +-------------+------------+
          |                    |                          v
          |                    |             +----------------------------+
          |                    |             | Execute write against       |
          |                    |             | downstream DB / API          |
          |                    |             +-------------+----------------+
          |                    |                          v
          |                    |             +----------------------------+
          |                    |             | Store result, mark key      |
          |                    |             | status=done                 |
          |                    |             +-------------+----------------+
          |                    |                          |
          +--------------------+---------------------------+
                               v
                  +----------------------------+
                  | Return result to agent      |
                  +----------------------------+
```

**Step-by-step:**

1. Agent sends the write with a client-generated idempotency key (UUIDv4) and payload.
2. Middleware looks up the key in Postgres. If found, and the stored request_hash matches the incoming payload's hash, return the stored result immediately (replayed=true). If found but the hash does not match, return 422.
3. If the key is new, append a write-intent record to the log before doing anything else, so every attempt is recorded even if the middleware crashes before executing the write.
4. Run the conflict check against recent write-intents for the same resource_type + resource_id within the configured conflict window (default 5s). If an overlapping write touches the same resource incompatibly, reject with 409 and record the conflict.
5. If clear, insert the idempotency-key row (status=pending) in the same Postgres transaction as the conflict check, using the unique constraint to guarantee only one execution wins if two requests race.
6. Execute the write against the downstream DB/API.
7. Store the result and mark the key status=done (or rejected/failed, a terminal, replayable state).
8. Return the result to the agent.

## 10. Conflict detection rules (v1)

- Two writes conflict if they target the same resource_type + resource_id within the conflict window and at least one is a mutating operation.
- Conflicting writes from the same agent_id are serialized rather than rejected outright.
- Conflicting writes from different agent_ids are rejected by default, with an escape hatch to configure specific resource_type + operation pairs as "safe to serialize."
- Rate-based abuse detection is flagged but not auto-blocked in v1; it surfaces on the conflicts dashboard for manual review.

## 11. High availability and consensus design

The correctness guarantee (at-most-once) does not depend on Raft. It depends entirely on Postgres's unique constraint plus a single atomic transaction that both checks and claims the key. Raft is used only to make the middleware tier itself horizontally scalable and resilient to node failure, by tracking which node is the preferred handler for a given key range.

| Scenario | Recommendation |
|---|---|
| Single middleware instance, low write volume | Skip Raft. Rely on Postgres transactions alone. |
| Multiple replicas, moderate volume | Skip Raft initially. Postgres's unique constraint already prevents double-execution; occasional contention is cheap at moderate volume. |
| High volume, many replicas, Postgres contention | Add Raft-based key-range sharding to route requests for the same key consistently to the same node. |

## 12. Non-functional requirements

| Requirement | Target |
|---|---|
| Latency overhead (p50) | < 15ms versus calling downstream directly |
| Latency overhead (p99) | < 60ms |
| Availability | 99.95% for the write-accept path |
| Durability of write-intent log | No data loss on single-node failure (replication factor >= 3) |
| Throughput (initial target) | 2,000 writes/sec sustained per region |
| Idempotency-key retention | 90 days hot in Postgres, archived to cold storage after |

## 13. Failure modes and edge cases

| Failure | Behavior |
|---|---|
| Middleware crashes after recording intent, before executing write | Key stays pending; a reconciliation job scans stale pending keys past a timeout and resumes or marks them failed. |
| Downstream DB/API unavailable during execution | Return 503, intent already recorded, agent can safely retry with the same key. |
| Two requests with the same key race simultaneously | Postgres unique constraint ensures only one insert wins; the loser returns the winner's result. |
| Agent reuses a key with a different payload | 422 error; treated as a client bug, never silently executed. |
| Write-intent log temporarily unavailable | Middleware fails closed (rejects the write) rather than executing without an audit record. |

## 14. Security considerations

- Idempotency keys are client-generated; the middleware does not trust them for authorization, only for deduplication.
- The write-intent log contains full payloads and should be treated as sensitive data; access restricted to the same tier as the downstream system.
- Conflict and rejection data is used for agent behavior review; access limited to platform/security roles.

## 15. Observability

- Metrics: write accept rate, replay rate, conflict rate, rejection rate, p50/p99 latency, Postgres contention, write-intent log lag.
- Alerts: conflict rate above baseline per agent_id, pending keys older than the reconciliation timeout, log consumer lag.
- Dashboards: per-agent write volume and rejection rate, resource-level conflict hotspots.

## 16. Rollout plan

| Phase | Scope |
|---|---|
| Phase 0 | Single-node middleware, Postgres only, no Kafka/NATS, no Raft. Validate the core guarantee on one low-risk write path. |
| Phase 1 | Add write-intent log and conflict detection. Roll out to a second write path with moderate volume. |
| Phase 2 | Horizontal scale-out of middleware replicas. Add Raft-based key-range routing if Postgres contention becomes measurable. |
| Phase 3 | Onboard all agent-driven write paths. Add per-agent rate-based abuse detection. |

## 17. Tech stack decision matrix

| Layer | Choice | Why |
|---|---|---|
| Middleware language | Go (default) or Rust | Go for faster iteration and hiring pool; Rust if the team wants stronger compile-time concurrency guarantees. |
| Idempotency store | PostgreSQL | Transactional guarantees and a unique constraint are the actual mechanism behind at-most-once. |
| Write-intent log | Kafka (default) or NATS JetStream | Kafka if already running it and needing long retention/replay; NATS for a lighter footprint. |
| HA coordination | Raft via etcd or hashicorp/raft | Only introduced in Phase 2+. |
| Deployment | Kubernetes | Stateless middleware replicas scale horizontally via HPA. |

## 18. Open questions

- Should the conflict window be fixed or configurable per resource_type?
- What is the retention policy for the conflicts table, and does it need its own archival path?
- Should 503 responses include a suggested retry-after value, and how is that computed?
- Do we need per-agent rate limiting in v1, or is dashboard-based manual review sufficient until Phase 3?

## 19. Appendix: example curl calls

```bash
# Submit a write
curl -X POST https://writes.internal/v1/writes \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: 7c9e6679-7425-40de-944b-e07fc1f90ae7" \
  -d '{
    "agent_id": "agent-checkout-flow",
    "resource_type": "invoice",
    "resource_id": "inv_8842",
    "operation": "create_charge",
    "payload": { "amount_cents": 4200, "currency": "usd", "customer_id": "cus_119" }
  }'

# Poll status
curl https://writes.internal/v1/writes/7c9e6679-7425-40de-944b-e07fc1f90ae7

# List recent conflicts
curl "https://writes.internal/v1/conflicts?since=2026-09-01T00:00:00Z"
```
