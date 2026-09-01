# ADR-0011: Scope read APIs by role and redact payloads by default

- **Status:** Accepted
- **Date:** 2026-09-01
- **Phase:** cross-cutting

## Context

PRD §4 places authentication and authorization out of scope, assuming the caller is
authenticated upstream. PRD §14 states that the write-intent log "contains full payloads
and should be treated as sensitive data; access restricted to the same tier as the
downstream system," and that conflict data access is "limited to platform/security roles."

Those two statements are in tension with the API surface. PRD §8.3
(`GET /v1/resources/{type}/{id}/intents`) returns intent history including the `payload`
column, and PRD §8.4 (`GET /v1/conflicts`) returns conflict records. Neither endpoint has
any stated access control. Declaring authorization out of scope is reasonable for the
*write* path, where the upstream caller is already authenticated and the key carries no
authority. It is not reasonable for endpoints whose entire purpose is to return other
parties' request bodies.

PRD §14 also omits `idempotency_keys.result`, which stores full downstream response
bodies and is exactly as sensitive as `write_intents.payload`.

## Decision

**Data classification.** `write_intents.payload` and `idempotency_keys.result` are
Confidential, at the same classification as the downstream system's own data. This is
stated in `DATA_MODEL.md` per column, so it is visible where schema changes happen.

**Three caller roles**, asserted by the upstream authenticator and carried to this layer
as verified claims. This layer authenticates the claims; it does not establish identity.

| Role | May call |
|---|---|
| `agent` | `POST /v1/writes`, and `GET /v1/writes/{key}` **only for its own `agent_id`** |
| `operator` | Read endpoints across agents, with payloads redacted |
| `investigator` | Everything `operator` can, plus unredacted payloads |

**Agent scoping.** `GET /v1/writes/{key}` resolves against `(agent_id, idempotency_key)`
from [ADR-0002](0002-key-scope-and-status-lifecycle.md), taking `agent_id` from the
verified caller identity and never from a parameter. A key belonging to another agent is
indistinguishable from a key that does not exist: both return `404`. This avoids
confirming existence to a caller not entitled to the record.

**Redaction by default.** `GET /v1/resources/{type}/{id}/intents` and `GET /v1/conflicts`
omit `payload` and `result` unless the request carries `?include=payload` **and** the
caller holds `investigator`. Metadata (resource, operation, timing, status, conflict
reason) is returned to `operator` without restriction, because that is what the PRD §5
incident-responder and dashboard use cases actually need. Reconstructing what an agent
attempted rarely requires the request bodies, and defaulting to metadata means the routine
path never touches Confidential data.

**Payload access is itself audited.** Every unredacted read writes an audit record
(caller, resources returned, time, stated reason). The `investigator` role is expected to
be rarely and deliberately used.

**Time bounds are mandatory.** Both list endpoints require a bounded range and apply a
default window when none is given, per the partition-pruning requirement in
[ADR-0009](0009-partitioning-and-retention.md). This is a performance requirement that
doubles as a blast-radius limit on any single read.

## Alternatives considered

**Leave read endpoints unauthenticated, per PRD §4.** Rejected. It contradicts PRD §14 in
the same document, and it makes payload disclosure a matter of network reachability.

**Return payloads to `operator` by default and rely on the audit log.** Rejected. It makes
the routine dashboard path a Confidential-data path, so the audit log fills with normal
activity and stops being a signal.

**Encrypt payloads at rest with per-agent keys.** Attractive and not rejected on merit,
but deferred: it interacts with the archive path in ADR-0009 and with probe-based
reconciliation, and it needs a key management story that does not exist yet. Recorded in
[OPEN_QUESTIONS.md](../OPEN_QUESTIONS.md).

## Consequences

- This layer must parse and verify caller identity even though it does not establish it,
  which is a narrowing of PRD §4 rather than a contradiction of it. `SYSTEM_DESIGN.md`
  states the trust boundary explicitly.
- The `404`-for-unauthorized behaviour on `GET /v1/writes/{key}` must be documented so
  agent authors do not read it as data loss.
- Redaction has to be enforced at the query layer, not by filtering after retrieval, so
  Confidential columns are never loaded for a request not entitled to them.
- An audit log is now a component with its own retention and access policy, which is
  itself Confidential. It must not become an unaudited copy of the data it protects, so it
  records which records were accessed, never their contents.
