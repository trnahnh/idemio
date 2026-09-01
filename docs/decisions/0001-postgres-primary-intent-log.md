# ADR-0001: Make the write-intent log Postgres-primary

- **Status:** Accepted
- **Date:** 2026-09-01
- **Phase:** 0

## Context

PRD §6 and §17 specify the write-intent log as Kafka (default) or NATS JetStream.
PRD §7 simultaneously models `write_intents` as a Postgres table with a foreign key to
`idempotency_keys` and a `(resource_type, resource_id, emitted_at)` index. These are not
compatible, and at least four requirements depend on which one is true:

1. PRD §9 step 5 requires the conflict check and the key claim to occur "in the same
   Postgres transaction." A conflict check that reads Kafka cannot participate in a
   Postgres transaction.
2. PRD §9 step 3 appends the intent *before* the key row exists, which violates the
   declared foreign key.
3. The `conflicts` table declares foreign keys to `write_intents.intent_id`. Kafka
   records have no such identity.
4. PRD §12 targets <15ms p50 overhead while PRD §13 requires failing closed when the log
   is unavailable, meaning a synchronous, durably-acknowledged append on the critical
   path. A Kafka acks=all round trip alone commonly consumes 5-15ms.

## Decision

`write_intents` is a **Postgres table and the source of truth** for the write-intent log.
It is written in the same transaction that claims the idempotency key, and it is the only
thing the conflict check reads.

Kafka (or NATS) is retained but moved **off the request path**. It is fed asynchronously
by a **transactional outbox**: the intent row is the outbox record, and a relay process
tails it (by logical decoding, or by polling an unpublished watermark) and publishes to
the topic at-least-once. Kafka serves long-horizon retention, replay, and downstream
analytics. Nothing on the write path ever reads from it.

The intent row and the key claim commit together. The PRD §9 motivation of recording
intent before doing anything else is preserved rather than lost: if the process crashes
before commit, neither the intent nor the claim exists, and critically neither does any
downstream side effect, so there is nothing to audit. The audit-relevant window is between
commit and downstream execution, and the committed `pending` row covers exactly that
window. Ordering *within* the transaction is therefore not meaningful; atomicity is what
the requirement actually needed.

Because the key row must exist before the intent row can reference it, the insert order
inside the transaction is: claim key, insert intent, conflict check under lock, execute.

## Alternatives considered

**Kafka-primary with a Postgres materialization of the recent window.** Rejected. It
introduces a replication lag window precisely where correctness lives: a conflict check
reading a materialization that is 200ms behind will pass two conflicting writes. It also
keeps a synchronous Kafka append on the p50 path.

**Kafka-primary, drop the transactional conflict check.** Rejected. This abandons PRD
§9 step 5 and reduces conflict detection to best-effort, which does not meet Goal §3.

**Drop Kafka entirely.** Tempting, and correct for Phases 0-1, where it is in fact not
deployed. Rejected as a permanent decision only because the PRD §12 90-day retention plus
replay requirement is real and Postgres should not carry it alone at target volume.

## Consequences

- PRD §9 step 5 becomes implementable exactly as written: one transaction performs the
  conflict check and the claim.
- **Availability stops being a product of two systems.** The PRD §13 fail-closed rule now
  means "Postgres must be up," which was already a hard dependency. The 99.95% target in
  PRD §12 no longer has to be divided across Kafka.
- The p50 budget loses its largest single line item. No synchronous broker acknowledgement
  is on the request path.
- We must build and monitor an outbox relay, including its lag. Publication is
  at-least-once, so downstream Kafka consumers must tolerate duplicates. Acceptable, since
  consumers are analytics and replay, not correctness.
- Postgres now absorbs the full intent write volume. This is what forces
  [ADR-0009](0009-partitioning-and-retention.md).
- Phase 0 and Phase 1 ship with no broker at all. The outbox watermark column exists from
  day one so that enabling the relay in Phase 1 is configuration, not migration.
