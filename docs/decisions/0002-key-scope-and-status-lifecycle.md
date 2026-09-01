# ADR-0002: Scope keys to the agent and extend the status lifecycle

- **Status:** Accepted
- **Date:** 2026-09-01
- **Phase:** 0

## Context

Two defects in PRD §7 and §8.

**Scope.** `idempotency_key` is declared a bare global UUID primary key. The key arrives
in the `Idempotency-Key` header while `agent_id` arrives in the body, and nothing binds
them. Any caller presenting a known key receives the stored `result`, the full downstream
response body, as a replay. PRD §14 states that keys are not trusted for authorization,
only deduplication, but replay *is* a read, and a global key namespace makes it a
cross-agent read. It also means two agents that independently generate the same key, or
one agent that copies another's, collide in a namespace they do not control.

**Lifecycle.** `key_status` is declared as `('pending','done','rejected')`. PRD §9 step 7
refers to a terminal `failed` state and PRD §13 says reconciliation "marks them failed."
Neither value exists. Separately, the enum has no way to express the most important state
in the system: a write whose outcome is genuinely unknown.

## Decision

**The primary key is `(agent_id, idempotency_key)`.** A replay is served only when the
presented `agent_id` matches the stored one. A key presented with a different `agent_id`
is treated as a distinct key, not as a conflict. Agents live in separate namespaces and
cannot observe each other through this surface.

**The status enum is:**

| Status | Meaning | Terminal | Re-claimable |
|---|---|---|---|
| `pending` | Claimed; downstream call in flight | no | no |
| `done` | Downstream returned a **definitive** outcome, success *or* business failure | yes | no |
| `failed` | Downstream was **provably not reached**; no side effect is possible | yes | **yes** |
| `indeterminate` | Outcome unknown: timeout, or a reset after the request was sent | yes | no |
| `rejected` | Blocked by conflict detection or policy; never sent downstream | yes | no |

The classification rules that assign these are in
[ADR-0005](0005-downstream-outcome-taxonomy.md).

`failed` is the only status that permits re-claiming the same key. The transition
`failed -> pending` is allowed because `failed` is defined as *provably no side effect*,
which is precisely the condition under which a retry is safe. This is what makes the
Goal §3 promise of safe retry semantics concrete rather than aspirational.

`indeterminate` is deliberately **not** re-claimable. Re-executing an unknown-outcome
write is the double-charge this system exists to prevent. Resolution is covered by
[ADR-0006](0006-reconciliation-never-resumes.md).

## Alternatives considered

**Keep the global key, verify `agent_id` on replay and return 403 on mismatch.** Rejected.
It still leaks existence, since a mismatched key proves another agent used it, and it
makes an agent's key generation collide with a namespace it cannot see.

**Add a separate `tenant_id` above `agent_id`.** Deferred, not rejected. Nothing in the
PRD establishes a tenancy model, and `agent_id` is sufficient isolation for the described
deployment. Recorded in [OPEN_QUESTIONS.md](../OPEN_QUESTIONS.md).

**Collapse `indeterminate` into `failed`.** Rejected outright. This is the exact merge
that causes double execution, since `failed` is re-claimable.

**Collapse business failures into `failed`.** Rejected. A declined charge is a definitive
answer from the downstream system. Marking it re-claimable would let an agent retry a
decline into an eventual success, which changes program semantics and defeats replay.

## Consequences

- The composite key widens the primary index and every foreign key that references it.
  Combined with the partitioning in [ADR-0009](0009-partitioning-and-retention.md), this
  is the reason we accept dropping the database-level FK on `write_intents`.
- `GET /v1/writes/{key}` requires an authenticated agent identity to scope the lookup. It
  cannot be an unauthenticated endpoint. See [ADR-0011](0011-read-api-access-control.md).
- Five statuses means five response mappings in `API_REFERENCE.md`, including status codes
  the PRD did not define (`202`, `502`).
- Operators gain a directly alertable signal: any non-zero `indeterminate` count is a
  human-attention event, not a metric to average.
