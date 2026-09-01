# Architecture Decision Records

Each ADR records one decision, the context that forced it, the alternatives rejected,
and the consequences accepted. ADRs are **immutable once accepted** — to change a
decision, write a new ADR that supersedes the old one and update the old one's status
line to point at it.

## Index

### Phase 0 — required before the first line of production code

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-postgres-primary-intent-log.md) | The write-intent log is Postgres-primary; Kafka is fed asynchronously by a transactional outbox | Accepted |
| [0002](0002-key-scope-and-status-lifecycle.md) | Idempotency keys are scoped to `(agent_id, idempotency_key)`; the status enum gains `failed` and `indeterminate` | Accepted |
| [0003](0003-canonical-request-hashing.md) | `request_hash` is SHA-256 over RFC 8785 (JCS) canonical JSON of a fixed field set | Accepted |
| [0004](0004-concurrent-claim-resolution.md) | The loser of a claim race receives `202 Accepted` and polls; it never blocks on the winner | Accepted |
| [0005](0005-downstream-outcome-taxonomy.md) | Downstream outcomes split three ways: definitive, provably-not-executed, indeterminate | Accepted |
| [0006](0006-reconciliation-never-resumes.md) | Reconciliation never re-executes a stale `pending` key; it resolves via probe or escalates | Accepted |

### Phase 1 — required before conflict detection ships

| ADR | Decision | Status |
|---|---|---|
| [0007](0007-operation-compatibility-matrix.md) | Conflict detection uses a declared per-resource operation compatibility matrix, defaulting to conflict | Accepted |
| [0008](0008-serialization-via-advisory-locks.md) | Same-agent conflicts serialize on Postgres transaction-scoped advisory locks | Accepted |
| [0009](0009-partitioning-and-retention.md) | `idempotency_keys` is hash-partitioned to preserve its global unique constraint; `write_intents` is range-partitioned on time and archived by DETACH | Accepted |

### Phase 2+ and cross-cutting

| ADR | Decision | Status |
|---|---|---|
| [0010](0010-consistent-hash-routing-not-raft.md) | Key affinity uses consistent-hash routing; Raft is not adopted | Accepted |
| [0011](0011-read-api-access-control.md) | Read APIs are role-scoped and redact payloads by default | Accepted |

## Template

```markdown
# ADR-NNNN: <short imperative title>

- **Status:** Proposed | Accepted | Superseded by ADR-NNNN
- **Date:** YYYY-MM-DD
- **Phase:** 0 | 1 | 2 | cross-cutting
- **Supersedes:** —

## Context
What forces this decision. Cite the PRD section or the defect if applicable.

## Decision
What we will do, stated in the present tense as a fact about the system.

## Alternatives considered
What else was on the table and why it lost.

## Consequences
What this makes easy, what it makes hard, and what it obliges us to build.
```
