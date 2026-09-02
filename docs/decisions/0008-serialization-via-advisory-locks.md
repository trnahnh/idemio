# ADR-0008: Serialize same-agent conflicts with Postgres advisory locks

- **Status:** Accepted; the lock-timeout response and the serialization mechanism are superseded by [ADR-0015](0015-conflict-check-transaction-shape.md)
- **Date:** 2026-09-01
- **Phase:** 1

## Context

PRD §10 rule 2 states that "conflicting writes from the same `agent_id` are serialized
rather than rejected outright," and PRD §7 gives the `conflicts` table a `resolution` enum
including `serialized`.

Nothing in the PRD says how serialization happens. There is no queue in the architecture
(PRD §6), no lock manager, no API status representing a queued write (PRD §8.5), and no
statement of what an agent sees while it waits or what happens when the wait is too long.
"Serialized" is named as an outcome without a mechanism.

There is also a correctness subtlety the PRD does not address: the conflict check reads
recent intents and then decides. Without mutual exclusion, two concurrent requests can both
read the window, both see no conflict, and both proceed. The check-then-act is a race
unless something serializes it. This affects *all* conflict detection, not only the
same-agent case.

## Decision

Every write path takes a **transaction-scoped Postgres advisory lock keyed on the
resource** before performing the conflict check:

    SELECT pg_advisory_xact_lock(hashtextextended(resource_type || ':' || resource_id, 0));

This runs inside the same transaction as the conflict check and the key claim
([ADR-0001](0001-postgres-primary-intent-log.md)), so:

- The check-then-act race is closed for every request, not just serialized ones.
- The lock is released automatically at commit or rollback. There is no leak path, no
  lease renewal, and no cleanup job. A crashed backend releases its locks when its
  connection dies.
- It works across all middleware replicas without adding a component, because the
  coordination point is the database that already holds the correctness guarantee.

Same-agent conflict handling is then a natural consequence rather than new machinery. The
lock holder proceeds; the waiter blocks on the lock, and when it acquires the lock it
re-runs the conflict check against a window that now includes the completed write. A
`conflicts` row is recorded with `resolution = 'serialized'`.

Waiting is bounded by `conflict.lock_timeout_ms` (default 250ms), set as the transaction's
`lock_timeout`. On timeout the request does not fail: it returns `202 Accepted` with
`Retry-After`, reusing the contract from
[ADR-0004](0004-concurrent-claim-resolution.md). This is why no new "queued" status code
is needed. From the agent's perspective, "your write is claimed and progressing, poll for
it" is the same statement in both cases.

Different-agent conflicts are unaffected: they take the same lock, then reject with `409`
per [ADR-0007](0007-operation-compatibility-matrix.md).

## Alternatives considered

**A durable queue per resource (Redis, Kafka partition, or a Postgres queue table).**
Rejected for v1. It adds a stateful component and a whole class of operational problems
(ordering, poison messages, drain-on-deploy, queue-depth alerting) to solve a case that a
250ms lock covers. Revisit only if serialized waits routinely exceed the timeout.

**Application-level locking via a `SELECT ... FOR UPDATE` on a resource row.** Rejected.
It requires a resource registry table that does not otherwise exist, and it makes lock
contention indistinguishable from row contention in monitoring.

**Optimistic detection with retry on serialization failure (`SERIALIZABLE` isolation).**
Considered seriously. Rejected as the primary mechanism because retry storms under
contention are harder to reason about and because the failure surfaces as a Postgres error
class the application must translate. Advisory locks make the contention explicit and
directly measurable.

**No lock; accept the check-then-act race.** Rejected. It silently weakens conflict
detection exactly under the concurrency it exists to catch.

## Consequences

- Lock wait time is a first-class metric and a leading indicator of the Phase 2 sharding
  decision in [ADR-0010](0010-consistent-hash-routing-not-raft.md). Sustained lock waits
  are the measured signal that key-affinity routing is needed.
- Hot resources serialize, by design. A single `resource_id` receiving heavy concurrent
  writes is throughput-limited to one write per downstream round trip. This is the correct
  behaviour and must be documented for integrators, since it is a real throughput ceiling
  per resource and does not affect the aggregate target in PRD §12.
- `hashtextextended` is a 64-bit hash, so distinct resources can theoretically collide onto
  one lock. The consequence of a collision is a spurious serialization, never a missed
  conflict, and at the described cardinality it is negligible.
- Transactions now hold a lock across the conflict check but **not** across the downstream
  call. The transaction commits the claim before execution begins. Holding a lock across a
  network call to a third party would be a serious availability hazard, and the design
  must not drift into it.
