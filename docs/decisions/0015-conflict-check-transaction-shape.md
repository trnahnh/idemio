# ADR-0015: Take the resource lock first, void intents that did not happen, and serialize by waiting

- **Status:** Accepted
- **Date:** 2026-09-01
- **Phase:** 1
- **Supersedes:** the lock-timeout response clause and the serialization mechanism of
  [ADR-0008](0008-serialization-via-advisory-locks.md)

## Context

[ADR-0008](0008-serialization-via-advisory-locks.md) established that conflict detection
runs under a transaction-scoped advisory lock keyed on the resource.
[ADR-0001](0001-postgres-primary-intent-log.md) fixed the insert order inside that
transaction as: claim key, insert intent, conflict check. Three things follow from the
combination that neither ADR addresses, and each is a correctness problem rather than a
detail.

**Where the lock is acquired is not stated.** ADR-0001's ordering reads naturally as
"insert, then lock, then check." Under that reading two concurrent conflicting writes both
insert their intents, then serialize on the lock, and then each observes the other's intent
in its window. Both are rejected. There is no winner, and the resource is unwritable for
the duration of the conflict window. A conflict detector whose response to contention is to
reject everyone has failed at the thing it exists to do.

**A rejected intent stays in the window.** The conflict-check query in
[DATA_MODEL.md](../DATA_MODEL.md) filters on resource and time only. A write rejected by
conflict detection never executed and never will, but its intent row remains and rejects
the next write, whose intent then rejects the one after it. One conflict becomes a rolling
outage on that resource. The same argument applies to a key that terminates `failed`, which
[ADR-0005](0005-downstream-outcome-taxonomy.md) defines as provably never having reached
downstream business logic. It does not apply to `indeterminate`, which may have executed,
nor to `pending`, which is in flight.

**"Serialized" names an outcome without an observable meaning.** ADR-0008 describes the
waiter re-running its check "against a window that now includes the completed write." No
such window exists. The claim transaction commits *before* the downstream call — that is an
invariant, and ADR-0008's own consequences forbid holding the lock across a network call.
The lock is therefore held for roughly the duration of two statements. A same-agent waiter
acquires it while the first write's downstream call is still in flight, records
`resolution = 'serialized'`, and executes concurrently with the write it supposedly
serialized behind. The `conflicts` row would assert something the downstream never saw.

## Decision

**The advisory lock is the first statement of the claim transaction**, before the key
insert:

```sql
SELECT pg_advisory_xact_lock(hashtextextended(resource_type || ':' || resource_id, 0));
```

ADR-0001's insert ordering is unchanged — the key is still claimed before the intent
references it — but both now occur with the lock already held. Exactly one concurrent
writer proceeds; the others see a committed window and are judged against it.

**`write_intents` gains `voided_at`.** It is stamped inside the transaction when conflict
detection rejects the write, and stamped when a key completes as `failed`. The conflict
check excludes rows with a non-null `voided_at`. The rule the column encodes is stated
directly: an intent participates in the conflict window unless the write it records
provably did not happen. The row itself is never deleted, because the intent log is the
record of what an agent attempted, and a rejected attempt is exactly the kind of thing an
incident responder needs to see.

**Lock timeout returns `503`, not `202`.** `lock_timeout` is set on the transaction as
ADR-0008 specifies, defaulting to 250ms. Because the lock is now acquired before anything
is written, a timeout rolls back a transaction that wrote nothing: no key row, no intent,
no downstream call. `503` with `retryable: true` is therefore not a convention borrowed
from the claim-race path, it is a literal description of what happened. ADR-0008's `202`
would name a key that does not exist, direct the agent to poll a `GET` that returns `404`,
and describe a claim that was never made.

**Same-agent conflicts serialize on the outcome, not on the check.** When the conflict
check finds a conflicting intent belonging to the same `agent_id`, the transaction rolls
back without claiming, the lock is released, and the request waits for the conflicting key
to reach a terminal status before retrying the whole transaction from the lock. The wait is
bounded and the lock is not held during it, so the prohibition on holding locks across
downstream calls is preserved. If the wait expires, the response is `503` on the same
grounds as a lock timeout: nothing was written. A `conflicts` row with
`resolution = 'serialized'` is recorded when the retry proceeds, and it now asserts
something true — the second write reached the downstream after the first one finished.

## Alternatives considered

**Claim the key first and take the lock in a second transaction.** This preserves
ADR-0008's `202` exactly. Rejected: it splits the claim from the conflict check, which is
the one thing [ADR-0001](0001-postgres-primary-intent-log.md) exists to keep together, and
it leaves a committed `pending` key whose conflict verdict is not yet known.

**Remove `lock_timeout` and let requests queue.** Simplest, and no contract change.
Rejected: a slow resource then consumes one connection per waiter, which is the mechanism
by which a hot `resource_id` becomes a pool-exhaustion incident for every other resource.

**Delete the rejected intent instead of voiding it.** Rejected. It fixes the window by
destroying the audit record, and it leaves `conflicts.intent_id_a` referencing a row that
no longer exists.

**Derive liveness by joining `idempotency_keys` instead of storing `voided_at`.** The join
is a point lookup on a hash-partitioned key, so it is not slow. Rejected because it couples
the conflict check to a second table on the hot path and makes "is this intent live" a
derived fact recomputed on every check rather than a stored one written once.

**Accept that only the check is serialized.** Rejected. It would let
[ROADMAP.md](../ROADMAP.md) Phase 1 exit criterion 3 pass while demonstrating nothing: two
same-agent writes would reach the downstream simultaneously with a database row asserting
they had been serialized.

**Reject same-agent conflicts with `409` like any other.** Uniform and trivially correct,
and it would remove the `serialized` resolution entirely. Rejected because PRD §10 rule 2
distinguishes the two cases deliberately: an agent conflicting with itself is usually
issuing two legitimate writes in quick succession, and rejecting the second turns a
throughput characteristic into an error.

## Consequences

- Every write to a resource serializes on its lock, including replays of already-completed
  keys. On a hot `resource_id` this is a real throughput ceiling. It is measured by
  `idemio_lock_wait_seconds`, and if the ceiling binds, the escape is a pre-transaction
  read of terminal keys outside the lock — safe, because a terminal row never re-executes.
  It is not built now, because it moves a round trip onto the new-write path to help the
  replay path and there is no evidence yet about which dominates.
- Same-agent serialization makes one request's latency depend on another's downstream call.
  The bound is explicit and the failure is `503`, which is the safe-retry path.
- `voided_at` must be stamped by the same code that records a `failed` outcome. If that
  write is lost, the intent stays live in the window and causes a spurious conflict — a
  false rejection, never a missed one.
- Lock wait is a first-class signal for the [ADR-0010](0010-consistent-hash-routing-not-raft.md)
  routing trigger, and it now measures contention on the whole claim path rather than on
  the conflict check alone.
