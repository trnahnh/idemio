# ADR-0010: Use consistent-hash routing for key affinity; do not adopt Raft

- **Status:** Accepted
- **Date:** 2026-09-01
- **Phase:** 2

## Context

PRD §6 and §11 introduce a Raft group that "tracks which middleware node currently owns
coordination responsibility for a given key range," used for the HA and failover path. The
PRD is careful and correct that Raft is **not** on the correctness path; correctness comes
from the Postgres unique constraint. PRD §11 recommends skipping Raft entirely at low and
moderate volume and adding "Raft-based key-range sharding" only when Postgres contention
becomes measurable.

The unexamined question is whether Raft is the right tool for the job it is left with.
Raft provides strongly consistent, linearizable agreement on ownership with a correct
handoff protocol. That is valuable when two nodes believing they own the same range would
be a correctness failure.

Here it is not. Postgres is the backstop: if routing sends two requests for the same key to
two different replicas, the unique constraint and the advisory lock in
[ADR-0008](0008-serialization-via-advisory-locks.md) still produce a correct outcome. The
only cost of imperfect routing is contention, which is the very thing routing was
introduced to reduce. **Routing is an optimisation whose worst case is the status quo.**
Paying for consensus to optimise something that is already safe is the wrong trade: it adds
a distributed state machine, leader elections, quorum loss modes, and a persistent state
directory to a tier PRD §6 describes as stateless.

## Decision

**Raft is not adopted.** The Raft group is removed from the target architecture.

When measurement shows contention worth addressing, key affinity is achieved by
**consistent hashing over the current replica set**, with membership taken from the
existing service discovery mechanism (Kubernetes endpoints). A request arriving at any
replica hashes `(agent_id, idempotency_key)` onto the ring and either handles it locally or
forwards it once to the owning replica. Disagreement during membership change costs a
brief period of reduced affinity, not incorrectness, and the hop is bounded to one.

Contention is addressed in this order, and each step must be shown insufficient before the
next is taken:

1. **Connection pooling** (PgBouncer in transaction mode). Most apparent Postgres
   contention at this scale is connection churn, not lock contention.
2. **Partitioning**, per [ADR-0009](0009-partitioning-and-retention.md), which shrinks the
   hot index and is required for other reasons anyway.
3. **Read replicas** for `GET` endpoints, removing read load from the write primary.
4. **Consistent-hash routing** as described above.
5. **Postgres sharding by `agent_id`**, if a single primary is genuinely saturated.

**Trigger for step 4**, so the decision is measured rather than argued: sustained p99
advisory-lock wait above 50ms, or `ON CONFLICT DO NOTHING` claim collisions above 1% of
writes, at or above 70% of the PRD §12 throughput target. Both are already emitted as
metrics per PRD §15.

## Alternatives considered

**Adopt Raft as specified in the PRD.** Rejected. It is a heavyweight solution to a
problem whose failure mode is benign, and it makes a stateless tier stateful. The
operational cost (quorum management, membership changes during rolling deploys, disk state
per replica) is paid continuously; the benefit is a marginal reduction in a contention
metric that cheaper steps address first.

**Use etcd leases for ownership without full Raft integration.** Rejected for the same
reason at lower magnitude. It still introduces an external coordination dependency on the
write path, and it still buys strong consistency the failure mode does not require.

**Never route; scale Postgres only.** A defensible position, and it is what steps 1-3 and
5 amount to. Consistent-hash routing is retained as an option because it is cheap and
requires no new infrastructure, unlike Postgres sharding.

## Consequences

- The middleware tier stays genuinely stateless, matching PRD §6, and horizontal scaling
  stays a plain HPA concern.
- The architecture diagram loses a component. `SYSTEM_DESIGN.md` reflects this; the PRD
  §6 diagram is superseded.
- "Raft via etcd or hashicorp/raft" is struck from the PRD §17 tech stack matrix.
- We commit to instrumenting lock wait and claim collision rate in Phase 1, before they
  are needed, so that the Phase 2 decision is made on data rather than on intuition.
- If a future requirement makes routing correctness-critical rather than an optimisation,
  this ADR must be revisited rather than worked around, since its entire argument rests on
  that not being the case.
