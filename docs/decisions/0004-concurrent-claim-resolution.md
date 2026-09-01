# ADR-0004: Return 202 to the loser of a claim race

- **Status:** Accepted
- **Date:** 2026-09-01
- **Phase:** 0

## Context

PRD §13 states that when two requests with the same key race, the Postgres unique
constraint ensures only one insert wins and "the loser returns the winner's result."

The winner does not have a result. It has just claimed the key and set `status = pending`;
its downstream call is still in flight and may take hundreds of milliseconds. The loser is
therefore being asked to return a value that does not exist yet. PRD §8.5 defines no
response for this state. There is no code meaning "claimed, still running."

This is not an edge case. It is the most likely concurrency event in the system, because
the retry pattern that motivates the whole design (agent times out at 500ms, retries)
produces exactly this race against an in-flight original.

## Decision

The claim is `INSERT ... ON CONFLICT DO NOTHING`. If zero rows are affected, the request
lost the race and re-reads the existing row.

The response depends on the winner's status:

| Winner status | Response |
|---|---|
| `done` | `200 OK`, stored result, `replayed: true` |
| `rejected` | `409 Conflict`, stored conflict detail |
| `failed` | Re-claim the key and execute; this request becomes the new owner |
| `indeterminate` | `502 Bad Gateway`, `retryable: false` |
| `pending` | **`202 Accepted`**, with `Retry-After` and a `Location` header pointing at `GET /v1/writes/{key}` |

**The loser never blocks on the winner.** Holding the connection open until the winner
finishes would tie up a middleware worker for the full downstream latency, convert one
slow downstream call into two, and directly attack the p99 target in PRD §12. A downstream
slowdown would consume connections at the rate agents retry, which is the classic path
from a slow dependency to a saturated proxy tier.

A bounded server-side wait is available as an optimisation, configured by
`claim.pending_wait_ms` and **defaulting to 0**. When set, the loser waits up to that
budget for the winner to reach a terminal state before falling back to `202`. It is an
optimisation for the fast-downstream case and must never be set near the downstream
timeout.

`202` is added to the status table in `API_REFERENCE.md`. It carries no result and is not
a completion.

## Alternatives considered

**Block until the winner completes.** Rejected for the saturation argument above. It also
gives the loser a latency floor equal to the winner's full downstream call, which is the
opposite of the sub-15ms overhead goal.

**Return `409 Conflict` for a pending winner.** Rejected. `409` in this API means conflict
detection rejected the write (PRD §8.5), a terminal outcome. Overloading it with a
transient, retryable state would make the code ambiguous for exactly the clients that must
distinguish them.

**Return `200` with a null result and `status: "pending"`.** Rejected. `200` implies the
request was fulfilled; agent frameworks and HTTP middleware routinely treat a 2xx with a
body as success and would consume a null result as a completed write.

**Have the loser execute too, relying on downstream idempotency.** Rejected. It presumes
the downstream property this entire layer exists to supply.

## Consequences

- Agents must handle `202` and poll `GET /v1/writes/{key}`. This is the primary reason
  PRD §8.2 exists, and it is promoted from "useful when the caller lost the response" to a
  required part of the client loop. Client SDKs should implement the poll transparently.
- `Retry-After` needs a computed value rather than a constant. Initial implementation is
  the p95 of recent downstream latency for that `resource_type`, clamped to `[50ms, 5s]`.
  This also answers the PRD §18 open question about `503` retry hints.
- Middleware worker occupancy stays proportional to accepted work rather than to retry
  pressure, which is what keeps the tier stable under a slow downstream.
- The re-claim from `failed` must itself be a conditional update guarded on the observed
  status, written as `UPDATE ... WHERE status = 'failed'`, or two losers could both
  re-claim. Whoever loses that update falls back to the same re-read and 202 logic.
