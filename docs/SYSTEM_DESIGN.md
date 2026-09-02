# System design

Owns the architecture, the write flow, failure handling, security posture, and
observability. Supersedes PRD §6, §9, §11, §13, §14, and §15.

Schema lives in [DATA_MODEL.md](DATA_MODEL.md). The external contract lives in
[API_REFERENCE.md](API_REFERENCE.md). Neither is restated here.

## What the system guarantees

> For a given `(agent_id, idempotency_key)`, the downstream write executes **at most
> once**, and every attempt is durably recorded before it is executed.

That guarantee rests on exactly one mechanism: a **unique constraint on
`(agent_id, idempotency_key)` in Postgres, claimed in a transaction that commits before
any downstream call is made.** Everything else in this document is scaffolding around that
sentence. If a proposed change weakens it, the change is wrong regardless of what it
improves.

Two boundaries on the guarantee, stated up front because they are the honest limits:

1. A key in `failed` may be re-claimed. `failed` means the request provably never reached
   downstream business logic ([ADR-0005](decisions/0005-downstream-outcome-taxonomy.md)).
2. A key in `indeterminate` is a write we cannot classify. The system does not guess and
   does not retry; it escalates ([ADR-0006](decisions/0006-reconciliation-never-resumes.md)).

## Architecture

The Raft group from PRD §6 and §11 has been removed. Its only job was key-range ownership
for the HA path, which the PRD itself placed off the correctness path. Since Postgres is
the backstop, imperfect routing costs contention, not correctness, and paying for consensus
to optimise something already safe is the wrong trade. See
[ADR-0010](decisions/0010-consistent-hash-routing-not-raft.md).

```
        [AI agent A]                    [AI agent B]
             \                              /
              \   POST /v1/writes          /
               \  + Idempotency-Key       /
                v                        v
        +--------------------------------------------+
        |  IDEMPOTENCY MIDDLEWARE (Go or Rust)        |
        |  N stateless replicas, HPA-scaled           |
        |                                             |
        |  - verify caller identity + role            |
        |  - canonicalize + hash request  (ADR-0003)  |
        |  - orchestrate the write flow               |
        +---------------------+-----------------------+
                              |
                              | single transaction:
                              |   advisory lock -> claim key
                              |   -> insert intent -> conflict check
                              v
        +--------------------------------------------+
        |  POSTGRES  (source of truth)                |
        |                                             |
        |   idempotency_keys   HASH x64               |
        |   write_intents      RANGE weekly           |
        |   conflicts          RANGE weekly           |
        +------+--------------------------+-----------+
               |                          |
               | 2. execute (only         | outbox relay
               |    after commit)         |    (async, Phase 1+)
               v                          v
   +------------------------+   +----------------------------+
   | Downstream DB / API    |   | Kafka / NATS               |
   | (system of record)     |   | retention, replay,         |
   +------------------------+   | analytics. NEVER read on   |
                                | the request path.          |
                                +----------------------------+
```

### Components

| Component | Responsibility |
|---|---|
| Middleware replica | Terminates agent connections, verifies identity, canonicalizes and hashes the request, runs the write flow, exposes the API. Holds no durable state. |
| Postgres | Source of truth for key state **and** the intent log. Supplies the unique constraint and the advisory locks that make correctness work. |
| Outbox relay | Tails `write_intents.published_at IS NULL` and publishes to Kafka at-least-once. Off the request path. Phase 1+. |
| Kafka / NATS | Long-horizon retention, replay, downstream analytics. Not a dependency of any request. |
| Reconciler | Resolves stale `pending` keys. Has **no** downstream write path, structurally. |
| Downstream DB / API | The system of record the write is applied to. |

The most consequential change from PRD §6: the intent log moved into Postgres
([ADR-0001](decisions/0001-postgres-primary-intent-log.md)). The PRD modelled it as both a
Kafka topic and a Postgres table, and four separate requirements depended on which one was
true — including PRD §9 step 5, which requires the conflict check and the key claim to share
a transaction. That is only possible if the intents are in Postgres.

## Write flow

```
  Agent: POST /v1/writes (Idempotency-Key, body)
                  |
                  v
  [1] Verify identity; agent_id must match caller        -> 401 / 403
                  |
                  v
  [2] Parse + validate; canonicalize (JCS); hash          -> 400 / 413 / 422
                  |
                  v
  [3] Resolve the operation in the manifest      (ADR-0013)
        |
   undeclared --> 422                             (ADR-0014)
        |
        v
  [4] BEGIN                                       (ADR-0015)
        |  advisory lock on resource, FIRST       (ADR-0008)
        |     timed out --> ROLLBACK -> 503  (nothing was written)
        |  INSERT key ON CONFLICT DO NOTHING
        |     claim lost --> re-read in-tx:
        |                      hash differs   -> 422
        |                      done           -> 200 replay
        |                      pending        -> 202 poll
        |                      rejected       -> 409 (stored body, replayed verbatim)
        |                      indeterminate  -> 502
        |                      failed         -> re-claim, continue
        |  INSERT intent
        |  conflict check over live window        (ADR-0007)
        |     other agent   --> record conflicts; void intent;
        |                       status=rejected; COMMIT -> 409
        |     same agent,
        |       still pending --> ROLLBACK, wait for it, retry [4]
        |     same agent,
        |       terminal      --> record 'serialized'; continue
        |     not enforcing   --> record 'observed'; continue
        |     clear           --> continue
        v
      COMMIT  (key is now pending)
        |
        v
  [5] Execute downstream
      (NO transaction, NO lock held)
        |
        v
  [6] Classify outcome                            (ADR-0005)
      definitive    -> done          -> 201
      not executed  -> failed        -> 503  (and void the intent)
      indeterminate -> indeterminate -> 502
        |
        v
  [7] Store result, set completed_at
```

### The transaction boundary is the design

Step 4 commits **before** step 5 begins. This ordering is what makes the guarantee work,
and both halves matter:

- **Claim before execute.** If the claim committed after execution, a crash in between
  would leave an executed write with no record, and the next retry would execute again.
- **Commit before execute.** The transaction must not stay open across the downstream call.
  Holding a Postgres transaction — and an advisory lock — across a network call to a third
  party would tie database resources to an external system's latency, and a downstream
  slowdown would become a database incident.

The cost of this ordering is the `pending` window: between commit and result storage, the
system has claimed a key whose outcome it does not yet know. That window is exactly what
`indeterminate` and the reconciler exist to handle. It is an accepted, bounded cost, not an
oversight.

### On PRD §9 step 3

The PRD appends the intent to the log *before* anything else, so that attempts are recorded
even if the middleware crashes before executing. That motivation is preserved by
atomicity rather than by ordering: if the transaction does not commit, there is no intent
**and** no side effect, so there is nothing to audit. The audit-relevant window opens at
commit, and the committed `pending` row covers it.

## Failure modes

Supersedes PRD §13.

| Failure | Behaviour |
|---|---|
| Crash before commit | Nothing recorded, nothing executed. The retry is a clean first attempt. |
| Crash after commit, before downstream call | Key stays `pending`. Reconciler probes if the integration supports it, else escalates to `indeterminate`. **Never re-executes.** ([ADR-0006](decisions/0006-reconciliation-never-resumes.md)) |
| Crash during downstream call | Identical handling. The system cannot distinguish this from the row above, which is precisely why neither is auto-resumed. |
| Crash after downstream call, before storing result | Identical handling. A probe resolves it to `done` with the real result. |
| Downstream provably unreachable | `failed`, `503`, agent retries the same key safely. |
| Downstream timeout after send | `indeterminate`, `502`, agent stops. |
| Downstream returns a business failure | `done`. Stored and replayed. Not an error of this layer. |
| Two requests race on the same key | Unique constraint picks a winner. Loser re-reads and responds per [ADR-0004](decisions/0004-concurrent-claim-resolution.md); it never blocks. |
| Key reused with a different body | `422`. Never executed. |
| Postgres unavailable | Total outage of the write path. Fail closed: no write proceeds without a committed claim. |
| Kafka unavailable | **No effect on the write path.** The outbox backlog grows and relay lag alerts. This is the direct benefit of ADR-0001. |
| Conflict manifest missing for a `resource_type` | Fails toward rejection: treated as `replace` with no scope, conflicting with everything. Availability incident, not a correctness one. |

PRD §13's "middleware fails closed rather than executing without an audit record" is now
structural rather than a policy: the audit record and the claim are the same transaction,
so executing without an audit record is not a reachable state.

## Availability

PRD §12 targets 99.95% on the write-accept path. Under the PRD's original design that
target had to be divided across Postgres **and** Kafka, because failing closed on log
unavailability put Kafka serially on the path. After ADR-0001 the only hard dependency is
Postgres, which was already required for correctness.

Practical implications:

- Postgres HA (synchronous replica, managed failover) is the single most important
  availability investment. Nothing else moves the number as much.
- Read endpoints can be served from a replica; the write path cannot.
- PgBouncer in transaction mode is a Phase 1 requirement, not a Phase 2 optimisation. At
  target throughput, connection churn shows up as contention long before lock contention
  does ([ADR-0010](decisions/0010-consistent-hash-routing-not-raft.md)).

## Scaling

The tier is stateless and scales on HPA. Contention is addressed in a fixed order, each
step justified by measurement before the next is taken: connection pooling, then
partitioning, then read replicas, then consistent-hash key affinity, then Postgres
sharding by `agent_id`. The trigger for introducing routing is stated numerically in
ADR-0010 so the decision is made on data.

Note one deliberate throughput ceiling: writes to a **single** `resource_id` serialize
behind the advisory lock, so per-resource throughput is bounded by one write per downstream
round trip. This does not affect the aggregate 2,000/sec target, but integrators must know
it ([ADR-0008](decisions/0008-serialization-via-advisory-locks.md)).

The ceiling is now measured rather than asserted. On one machine at 300 sequential writes,
overhead is **p50 8.4ms** across distinct resources and **p50 14.9ms** when every write
targets the same `resource_id` — the difference being lock contention plus a conflict window
that is never empty. A hot resource costs roughly twice an ordinary one, which is the price
of the guarantee rather than a defect, and it is why conflict recording is capped per write:
pairing each incoming intent against every live one in the window is quadratic in the
per-resource write rate.

## Latency budget

PRD §12: under 15ms p50 overhead, under 60ms p99. Indicative p50 allocation:

| Step | Budget |
|---|---|
| Identity verification | 0.5ms |
| Canonicalization + SHA-256 | 0.5ms |
| Claim transaction (lock, insert key, insert intent, conflict check, commit) | 6ms |
| Result store (update after execution) | 3ms |
| Middleware overhead, serialization, network | 2ms |
| **Total** | **~12ms** |

The claim transaction is four round trips — lock, claim, intent, conflict check — which is
why the timeout is set inside the lock statement rather than before it. Measured against
this allocation on one machine, the whole path costs 8.4ms p50 with Postgres on localhost,
so the budget has roughly 6ms of headroom for real network between the tiers. That headroom
is the entire margin, and it is why a fifth round trip on this path needs an argument.

This budget only closes because no synchronous broker acknowledgement is on the path. A
Kafka `acks=all` append alone commonly costs 5–15ms and would consume the entire budget by
itself, which was the fourth argument in ADR-0001.

## Security

Supersedes PRD §14, which is extended rather than contradicted.

**Trust boundary.** Callers are authenticated upstream (PRD §4). This layer verifies the
resulting claims and enforces role-based access. Declaring authorization out of scope is
reasonable for the write path, where the idempotency key carries no authority. It is not
reasonable for read endpoints whose purpose is returning other parties' request bodies,
which is why [ADR-0011](decisions/0011-read-api-access-control.md) narrows PRD §4.

- Idempotency keys are client-generated and never trusted for authorization — but replay
  **is** a read, so keys are scoped to `agent_id` and a cross-agent key returns `404`.
- `write_intents.payload` **and** `idempotency_keys.result` are Confidential. PRD §14
  named only the former; downstream response bodies are equally sensitive.
- Read endpoints redact payloads by default. Unredacted access requires the `investigator`
  role and writes an audit record naming the records accessed, never their contents.
- Every read endpoint requires a bounded time range, which limits blast radius as well as
  enabling partition pruning.

## Observability

Supersedes PRD §15. Metrics are grouped by the decision each one informs, because a metric
nobody acts on is not observability. [METRICS.md](METRICS.md) owns the metric names, their
labels, and the alert each one carries. This section owns why the groups exist and which of
them is allowed to wake someone.

**Correctness and safety.** Whether the guarantee is holding, and whether any write has an
outcome nobody knows. An `indeterminate` key is terminal and needs a human, so the correct
standing value is zero and any sustained non-zero pages. Stale `pending` keys and probe
failures are the leading indicators of the same failure, and they arrive together: a probe
that cannot be reached leaves keys pending rather than escalating them.

**Contract health.** Whether agents are using the contract correctly. These warn rather than
page, because they describe someone else's bug: a `422` spike means an agent is reusing keys
across different bodies, and a `409` spike after a manifest change usually means the manifest
rather than the agents.

**Capacity.** Whether the tier is running out of something. Two of these are the ADR-0010
routing triggers, instrumented well before the decision they inform so that it can be read
from data rather than argued. Partition headroom is the one capacity signal that pages, since
a missing partition is a hard write-path outage rather than a degradation.

**Dashboards.** Per-agent write volume, rejection rate, and replay rate; resource-level
conflict hotspots; latency decomposed by the budget table above so a regression can be
attributed to a step rather than to the system as a whole.
