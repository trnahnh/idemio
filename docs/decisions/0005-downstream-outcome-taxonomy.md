# ADR-0005: Classify downstream outcomes three ways

- **Status:** Accepted
- **Date:** 2026-09-01
- **Phase:** 0

## Context

PRD §8.5 defines exactly one failure response for the execution step: `503`, meaning
"downstream system unavailable; write intent recorded but not yet executed, safe to
retry." PRD §13 repeats this.

That covers one of three genuinely different situations, and conflates the other two:

1. The downstream **answered**, but the answer was a business failure. A card was declined,
   a validation rule rejected the payload, the invoice was already closed. This is a
   definitive result. It is not an availability problem.
2. The downstream was **provably not reached**. DNS failure, connection refused, TLS
   handshake failure, or a load balancer 503 returned before the request body was
   forwarded. No side effect is possible.
3. The outcome is **unknown**. The connection was established and the request sent, then
   the call timed out, the socket reset, or a gateway returned 502/504. The write may have
   landed, may have partially landed, or may not have landed at all.

Case 3 is the hardest and the most common in practice, and it is the one agents hit most
often, since it is exactly what an agent-side timeout looks like from the middleware. The
PRD's single `503` tells the agent it is "safe to retry" in all three cases. In case 3
that statement is false, and acting on it produces the double charge this system exists
to prevent.

## Decision

Every downstream call is classified into one of three buckets, which map onto the statuses
defined in [ADR-0002](0002-key-scope-and-status-lifecycle.md).

| Bucket | Trigger | Key status | Response to agent | Agent may retry same key |
|---|---|---|---|---|
| **Definitive** | Any complete, well-formed downstream response, including business failures | `done` | `201` on first execution, `200` on replay; the downstream outcome is carried in the `result` body | no, and it does not need to |
| **Not executed** | DNS, connection refused, TLS failure, request never transmitted, downstream returned an availability error before accepting the body | `failed` | `503`, `retryable: true`, with `Retry-After` | **yes** |
| **Indeterminate** | Timeout after transmission, connection reset mid-flight, gateway `502`/`504`, any response we cannot parse | `indeterminate` | `502`, `retryable: false` | **no** |

Two consequences of this table deserve to be stated as rules in their own right:

**A business failure is a success of the idempotency layer.** A declined charge is stored
in `result` with `status = done` and replays identically forever. Retrying the same key
returns the same decline. An agent that wants a different outcome must issue a new logical
write with a new key. This is the only behaviour that preserves replay semantics; treating
a decline as retryable would let an agent grind a decline into an eventual success.

**Only "not executed" is retryable, and it must be proven, not assumed.** The default
classification for anything ambiguous is `indeterminate`. Implementations must not widen
the "not executed" bucket for convenience. A useful test: a call belongs in `failed` only
if we can point to the specific mechanism that guarantees the request bytes never reached
downstream business logic.

To make bucket 2 provable rather than inferred, the HTTP client is configured so that
connection establishment and request transmission are distinguishable from response
waiting, and per-attempt timeouts are set separately for each. Requests are not
transparently retried by the HTTP client library; retry is a decision this layer makes,
not the transport.

## Alternatives considered

**Keep a single `503` for everything.** Rejected. It tells agents that indeterminate
writes are safe to retry, which is the primary failure mode the system was built to
eliminate.

**Map indeterminate to `503` with a flag in the body.** Rejected. `503` carries a strong,
widely implemented "retry me" convention in HTTP clients, proxies, and service meshes.
Many agent frameworks retry `503` automatically before any application code sees the body.
Choosing a code that is not auto-retried is a safety property, not a stylistic one.

**Probe the downstream synchronously on timeout to resolve the outcome.** Rejected for the
request path. It adds unbounded latency to the worst case. It is however exactly the right
mechanism off the request path, which is [ADR-0006](0006-reconciliation-never-resumes.md).

## Consequences

- `502` joins the status code table in `API_REFERENCE.md` with a meaning specific to this
  system: the write may or may not have happened, and this layer will not guess.
- Agents need three retry behaviours rather than one. Client SDKs should encode this
  directly so that individual agent authors never reason about it.
- `indeterminate` writes accumulate and require resolution. That is deliberate: they are
  the cases a human or a probe must settle, and the alert threshold for them is non-zero.
- Downstream integrations become non-trivial to onboard. Each `resource_type` must declare
  how its errors classify, which is a per-integration piece of work tracked in the
  Phase 1 checklist.
