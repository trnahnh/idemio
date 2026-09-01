# ADR-0006: Reconciliation never re-executes a stale pending write

- **Status:** Accepted
- **Date:** 2026-09-01
- **Phase:** 0

## Context

PRD §13 handles a middleware crash between recording intent and executing the write as
follows: "Key stays pending; a reconciliation job scans stale pending keys past a timeout
and resumes or marks them failed."

The word "resumes" breaks the system's central guarantee. A key is `pending` precisely
because the middleware does not know whether the downstream call happened. The crash could
have occurred before the call, during it, or after the downstream committed but before the
result was stored. Re-executing in that state is not a resumption; it is a second
execution of a write that may already have landed. It is the double charge in Goal §1,
introduced by the mechanism that was supposed to prevent it.

The same reasoning applies to the `indeterminate` status introduced by
[ADR-0005](0005-downstream-outcome-taxonomy.md), which a crashed `pending` key is
functionally equivalent to.

## Decision

**The reconciler never issues a downstream write.** It has no execution path. This is a
structural property of the component, not a policy flag, and it should be enforced by the
reconciler simply not having a downstream client capable of mutation.

A key found `pending` past `reconcile.stale_after` (default 5 minutes, and required to be
comfortably greater than the downstream timeout) is resolved by exactly one of:

**1. Probe, when the integration supports it.** A `resource_type` may register a
`probe(idempotency_key, intent) -> done | failed | unknown` function that performs a
**read-only** query against the downstream system to determine whether the write landed.
For a payments integration this is typically a lookup by the downstream's own idempotency
reference or by a client-supplied correlation id. If the probe returns `done`, the result
is fetched and stored and the key becomes `done`. If it returns `failed`, the key becomes
`failed` and is re-claimable. If it returns `unknown`, fall through to case 2.

To make probes possible, every downstream call carries a **correlation id derived from the
idempotency key** in whatever field the integration offers for it. Integrations that
expose no such field cannot be probed, which is a fact to establish during onboarding
rather than discover during an incident.

**2. Escalate.** With no probe available, the key transitions to `indeterminate` and is
surfaced for human resolution. It is never silently closed. `indeterminate` is terminal
and not re-claimable, so an agent retrying that key receives `502` and stops.

Whether a `resource_type` has a probe is a required field in its integration manifest.
Onboarding a write path without one is allowed, but it is an explicit, recorded acceptance
that crash recovery for that path is manual.

## Alternatives considered

**Resume, relying on downstream idempotency.** Rejected. If the downstream were reliably
idempotent, this middleware would not need to exist. Where a downstream *does* offer native
idempotency keys, we use them as defence in depth and as a probe mechanism, but we never
make correctness depend on them.

**Resume only when the intent was recorded but no downstream attempt was logged.**
Rejected. It requires a durable "about to call" marker written after commit and before the
call, which is a second write on the critical path and still leaves a window between that
marker and the call itself. It reduces the race window without closing it, at real
latency cost.

**Mark all stale pending keys `failed` (re-claimable).** Rejected. `failed` means provably
no side effect. A crashed `pending` key is precisely the case where that is not provable.
This alternative is the same double-execution bug wearing a different label.

**Leave stale keys `pending` forever.** Rejected. `pending` is non-terminal, so an agent
polling `GET /v1/writes/{key}` would receive `202` indefinitely with no signal that a human
must intervene.

## Consequences

- Crash recovery is *safe by default and complete only where a probe exists*. This is an
  honest position: the system never guesses, and the gap is visible per integration rather
  than hidden.
- `indeterminate` count and age become primary alerts. Unlike conflict rate, the correct
  target is zero, and any sustained non-zero value is an unresolved possible side effect.
- Each integration carries a probe as a first-class deliverable alongside its error
  classification from ADR-0005.
- The reconciler is cheap, stateless, and safe to run on every replica with an advisory
  lock for mutual exclusion, since it performs only reads and status transitions.
- Operators need a documented manual resolution procedure: how to inspect the intent,
  check the downstream by hand, and force a key to `done` or `failed`. That procedure and
  its audit requirements live in `DEPLOYMENT_CHECKLIST.md`.
