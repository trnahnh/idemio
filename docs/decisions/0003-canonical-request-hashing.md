# ADR-0003: Define request hashing over RFC 8785 canonical JSON

- **Status:** Accepted
- **Date:** 2026-09-01
- **Phase:** 0

## Context

PRD §9 step 2 returns `422` when a reused key arrives with a `request_hash` that does not
match the stored one, and PRD §13 classifies this as a client bug that is never silently
executed. The PRD never defines how the hash is computed.

This is dangerous precisely because the penalty is severe. An agent retrying after a
timeout will re-serialize its request from an in-memory object. If the hash is taken over
raw bytes, then a different map iteration order, a float rendered as `4200.0` instead of
`4200`, a changed key order, or added whitespace all produce a different hash. The agent
then receives `422`, the response that means "you have a bug, we will never execute this,"
for a correct retry. The core promise of the system fails in its single most important
scenario.

## Decision

`request_hash` is SHA-256, hex-encoded lowercase, computed over the **RFC 8785 JSON
Canonicalization Scheme (JCS)** serialization of exactly this object:

    {
      "agent_id":      <string>,
      "operation":     <string>,
      "payload":       <the payload value, canonicalized recursively>,
      "resource_id":   <string>,
      "resource_type": <string>
    }

JCS fixes every degree of freedom that caused the problem: object keys are sorted by
UTF-16 code unit, there is no insignificant whitespace, strings use minimal escaping, and
numbers serialize by the ECMAScript number-to-string algorithm, so `4200`, `4200.0`, and
`4.2e3` all canonicalize identically.

Rules:

- The `Idempotency-Key` header is **not** part of the hash. It identifies the record; it
  does not describe the request.
- No other header, no transport metadata, and no server-assigned timestamp is included.
- `NaN`, `Infinity`, and `-Infinity` are rejected with `422` at parse time. JCS cannot
  represent them, and accepting them would make the hash ill-defined.
- Integers outside the IEEE-754 exactly-representable range are rejected with `422`.
  Monetary values must be integer minor units, as the PRD §8.1 `amount_cents` field
  already models.
- The canonical form is hashed, not stored. Only the digest is persisted.

The algorithm is versioned. The stored value is `sha256-jcs-v1:<hex>`, so a future change
to the scheme can be detected rather than silently mismatching every existing key.

## Alternatives considered

**Hash the raw request bytes.** Simplest and fastest, and rejected for the reason above:
it makes correct retries fail.

**Have the client send its own hash.** Rejected. It shifts a correctness-critical
computation to the least trusted party and makes every client SDK reimplement it.

**Skip the hash and trust the key alone.** Rejected. It removes the only defence against
an agent reusing a key across genuinely different writes, which PRD §13 explicitly wants
to catch.

**Canonicalize by sorting keys ad hoc rather than adopting JCS.** Rejected. Number
formatting is the subtle half of the problem and the half a hand-rolled implementation
gets wrong. JCS is specified, has published test vectors, and has implementations in both
candidate languages.

## Consequences

- Client SDKs do not need to canonicalize; the middleware does it on receipt. Agents may
  send any valid JSON encoding of the same logical request.
- Canonicalization cost lands on the p50 path. It is bounded by payload size and is
  microseconds for the payloads described in PRD §8.1. Large payloads must be size-capped
  in configuration.
- Rejecting non-finite numbers is a real, documented behaviour change from "accept any
  JSON" and must appear in `API_REFERENCE.md`.
- The `sha256-jcs-v1:` prefix must be written from the first migration. Retrofitting a
  prefix onto stored bare digests later requires a backfill.
