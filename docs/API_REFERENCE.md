# API reference

This document owns the external contract: endpoints, status codes, request hashing, and
retry semantics. It supersedes PRD §8, which defined five status codes and left several
reachable states unrepresented.

Base path: `/v1`. All requests and responses are `application/json; charset=utf-8`.

## Identity and roles

Callers are authenticated upstream (PRD §4). This layer **verifies** the resulting claims;
it does not establish identity. Three roles, per
[ADR-0011](decisions/0011-read-api-access-control.md):

| Role | Access |
|---|---|
| `agent` | `POST /v1/writes`; `GET /v1/writes/{key}` for its own `agent_id` only |
| `operator` | All read endpoints, across agents, payloads redacted |
| `investigator` | As `operator`, plus unredacted payloads (audited) |

`agent_id` in a request body must match the verified caller identity. A mismatch is `403`.
The body field is retained for readability and for logging; it is never the source of
truth for authorization.

---

## POST /v1/writes

The only endpoint agents call to perform a write.

### Request

```http
POST /v1/writes
Content-Type: application/json
Idempotency-Key: 7c9e6679-7425-40de-944b-e07fc1f90ae7

{
  "agent_id": "agent-checkout-flow",
  "resource_type": "invoice",
  "resource_id": "inv_8842",
  "operation": "create_charge",
  "payload": {
    "amount_cents": 4200,
    "currency": "usd",
    "customer_id": "cus_119"
  }
}
```

| Field | Required | Notes |
|---|---|---|
| `Idempotency-Key` header | yes | UUID. Client-generated, per PRD §9. Malformed or absent is `422`. |
| `agent_id` | yes | Must equal the verified caller identity. |
| `resource_type` | yes | Must have a registered manifest, or the write is treated as `replace` with no scope and conflicts with everything ([ADR-0007](decisions/0007-operation-compatibility-matrix.md)). |
| `resource_id` | yes | Opaque string. |
| `operation` | yes | Must be declared in the manifest for `resource_type`. |
| `payload` | yes | Arbitrary JSON object, subject to the constraints below. |

### Payload constraints

From [ADR-0003](decisions/0003-canonical-request-hashing.md):

- `NaN`, `Infinity`, and `-Infinity` are rejected with `422`. RFC 8785 cannot represent
  them, and accepting them would make the request hash ill-defined.
- Numbers outside the IEEE-754 exactly-representable integer range are rejected with `422`.
  Use integer minor units for money, as `amount_cents` does.
- Payloads above `limits.payload_bytes` (default 256 KiB) are rejected with `413`.

### Responses

**`201 Created` — new write, executed, definitive outcome.**

```json
{
  "idempotency_key": "7c9e6679-7425-40de-944b-e07fc1f90ae7",
  "status": "done",
  "result": { "charge_id": "ch_5521", "status": "succeeded" },
  "replayed": false,
  "intent_id": "0192f3a1-7c4e-7000-8b21-4f9a2c1d5e30"
}
```

A **business failure is also `201` with `status: "done"`**. The downstream answered; the
answer was "no". Per [ADR-0005](decisions/0005-downstream-outcome-taxonomy.md) this is a
definitive outcome, it is stored, and it replays identically forever.

```json
{
  "idempotency_key": "7c9e6679-7425-40de-944b-e07fc1f90ae7",
  "status": "done",
  "result": { "error": "card_declined", "decline_code": "insufficient_funds" },
  "replayed": false
}
```

**`200 OK` — replay of a completed write.** Identical body, `"replayed": true`.

**`202 Accepted` — claimed, execution in flight.** Returned to the loser of a claim race
([ADR-0004](decisions/0004-concurrent-claim-resolution.md)) and to a request whose
serialization wait exceeded `conflict.lock_timeout_ms`
([ADR-0008](decisions/0008-serialization-via-advisory-locks.md)). It is not a completion
and carries no result.

```http
HTTP/1.1 202 Accepted
Retry-After: 1
Location: /v1/writes/7c9e6679-7425-40de-944b-e07fc1f90ae7
```
```json
{
  "idempotency_key": "7c9e6679-7425-40de-944b-e07fc1f90ae7",
  "status": "pending",
  "retry_after_ms": 850
}
```

`Retry-After` is computed from recent p95 downstream latency for the `resource_type`,
clamped to `[50ms, 5s]`.

**`409 Conflict` — rejected by conflict detection.**

```json
{
  "idempotency_key": "a1b2c3d4-5e6f-4a7b-8c9d-0e1f2a3b4c5d",
  "status": "rejected",
  "reason": "conflicting_write",
  "detail": "mutate/mutate with overlapping scope on invoice:inv_8842",
  "conflicting_intent_id": "0192f3a1-7c4e-7000-8b21-4f9a2c1d5e30",
  "conflict_id": "0192f3a1-8d1f-7000-9c33-1a2b3c4d5e6f"
}
```

Terminal. Retrying the same key returns the same `409`. Note that `conflicting_intent_id`
and `conflict_id` are UUIDs; the PRD §8.3 example showed prefixed strings (`int_9981`,
`cf_442`), which the schema in [DATA_MODEL.md](DATA_MODEL.md) does not use.

**`422 Unprocessable Entity` — request hash mismatch on a reused key.**

```json
{
  "idempotency_key": "7c9e6679-7425-40de-944b-e07fc1f90ae7",
  "status": "rejected",
  "reason": "request_hash_mismatch",
  "detail": "This key was previously used with a different request body."
}
```

Per PRD §13 this is a client bug and is never silently executed. Because the penalty is
severe, hashing is canonical rather than byte-based, so a legitimate retry that
re-serializes its JSON differently will **not** trigger this. See
[ADR-0003](decisions/0003-canonical-request-hashing.md).

**`503 Service Unavailable` — provably not executed, safe to retry.**

```json
{
  "idempotency_key": "7c9e6679-7425-40de-944b-e07fc1f90ae7",
  "status": "failed",
  "reason": "downstream_unreachable",
  "retryable": true
}
```

The key is left in `failed`, which is the only re-claimable status. Retrying with the
**same** key is correct and expected.

**`502 Bad Gateway` — indeterminate outcome.**

```json
{
  "idempotency_key": "7c9e6679-7425-40de-944b-e07fc1f90ae7",
  "status": "indeterminate",
  "reason": "downstream_timeout_after_send",
  "retryable": false,
  "detail": "The write may or may not have been applied. Do not retry this key."
}
```

This code exists because `503` is auto-retried by HTTP clients, proxies, and agent
frameworks before application code sees the body. Choosing a non-auto-retried code for the
unknown-outcome case is a safety property, not a stylistic one. Resolution is by probe or
human, per [ADR-0006](decisions/0006-reconciliation-never-resumes.md).

---

## GET /v1/writes/{idempotency_key}

Polls a previously submitted write. Required by the `202` flow, not merely a convenience.

Scoped to the calling agent. A key belonging to another agent returns `404`, identical to
a key that does not exist, so the endpoint does not confirm existence to a caller not
entitled to the record.

```http
GET /v1/writes/7c9e6679-7425-40de-944b-e07fc1f90ae7
```
```json
{
  "idempotency_key": "7c9e6679-7425-40de-944b-e07fc1f90ae7",
  "status": "done",
  "result": { "charge_id": "ch_5521", "status": "succeeded" },
  "created_at": "2026-09-01T10:15:00Z",
  "completed_at": "2026-09-01T10:15:00.412Z",
  "attempt_count": 1
}
```

Returns `200` for any terminal status, with `status` carrying the outcome. Returns `202`
with `Retry-After` while the write is still `pending`.

---

## GET /v1/resources/{resource_type}/{resource_id}/intents

Write-intent history for a resource. For incident response (PRD §5).

Requires `operator` or `investigator`. **Payloads are redacted by default.**

| Parameter | Required | Default | Notes |
|---|---|---|---|
| `since` | yes | — | RFC 3339. A bounded range is mandatory. |
| `until` | no | `now()` | |
| `limit` | no | 100 | Max 1000. |
| `include` | no | — | `payload` requires the `investigator` role; otherwise `403`. |

The mandatory time bound exists for two reasons: it lets Postgres prune `write_intents`
partitions ([ADR-0009](decisions/0009-partitioning-and-retention.md)), and it limits the
blast radius of any single read. An unbounded default would do neither.

```json
{
  "resource_type": "invoice",
  "resource_id": "inv_8842",
  "intents": [
    {
      "intent_id": "0192f3a1-7c4e-7000-8b21-4f9a2c1d5e30",
      "agent_id": "agent-checkout-flow",
      "idempotency_key": "7c9e6679-7425-40de-944b-e07fc1f90ae7",
      "operation": "create_charge",
      "operation_class": "create",
      "emitted_at": "2026-09-01T10:15:00.008Z",
      "payload": null,
      "payload_redacted": true
    }
  ],
  "has_more": false
}
```

Every request with `include=payload` writes a `payload_access_audit` row.

---

## GET /v1/conflicts

Detected conflicts in a time window, for dashboards and alerting (PRD §8.4).

Requires `operator` or `investigator`. Same mandatory `since`, same redaction rules.
Additional filters: `agent_id`, `resource_type`, `resolution`.

```json
{
  "conflicts": [
    {
      "conflict_id": "0192f3a1-8d1f-7000-9c33-1a2b3c4d5e6f",
      "resource_type": "invoice",
      "resource_id": "inv_8842",
      "agent_id_a": "agent-checkout-flow",
      "agent_id_b": "agent-dunning",
      "reason": "mutate/mutate overlapping scope",
      "resolution": "rejected",
      "detected_at": "2026-09-01T10:15:00.011Z"
    }
  ],
  "has_more": false
}
```

---

## Status codes

Supersedes PRD §8.5. New codes are marked.

| Code | Meaning | Key status | Agent retries same key? |
|---|---|---|---|
| `200` | Replay of a completed write | `done` | no need |
| `201` | New write executed, definitive outcome (success **or** business failure) | `done` | no need |
| `202` **(new)** | Claimed and in flight; poll `Location` | `pending` | poll, do not resubmit |
| `400` **(new)** | Malformed JSON or missing required field | — | no, fix the request |
| `401` **(new)** | Missing or invalid caller identity | — | no |
| `403` **(new)** | Role insufficient, or `agent_id` mismatch | — | no |
| `404` **(new)** | Unknown key, or key belonging to another agent | — | no |
| `409` | Conflict detected, write rejected | `rejected` | no, terminal |
| `413` **(new)** | Payload exceeds `limits.payload_bytes` | — | no, fix the request |
| `422` | Malformed idempotency key, hash mismatch, or non-representable number | `rejected` | no, client bug |
| `429` **(reserved)** | Per-agent rate limit. Not implemented before Phase 3. | — | yes, after `Retry-After` |
| `502` **(new)** | Indeterminate outcome; may or may not have executed | `indeterminate` | **no** |
| `503` | Provably not executed; downstream unreachable | `failed` | **yes** |

## Client retry rules

The whole contract compressed into the decision an agent actually has to make. Client SDKs
should implement this so individual agent authors never reason about it.

| Response | Action |
|---|---|
| `200` / `201` | Done. Read `result`. A business failure is a result, not an error. |
| `202` | Poll `GET /v1/writes/{key}` after `Retry-After`. Never resubmit `POST`. |
| `409` / `422` | Stop. Terminal. Escalate; do not generate a new key and retry blindly. |
| `503` | Retry with the **same** key, with backoff. This is the safe-retry path. |
| `502` | **Stop.** The write may have landed. A human or a probe must resolve it. Generating a new key here is the double-write this system exists to prevent. |
| `500` | Retry with the same key. The layer itself failed; the key is either unclaimed or `pending`, and both are safe. |

## Idempotency guarantees, stated precisely

- A given `(agent_id, idempotency_key)` executes downstream **at most once**, enforced by
  a Postgres unique constraint ([DATA_MODEL.md](DATA_MODEL.md)), except in one case: a key
  in `failed` may be re-claimed, and `failed` means the request provably never reached
  downstream business logic ([ADR-0005](decisions/0005-downstream-outcome-taxonomy.md)).
- Replay returns the **stored** result byte-for-byte. It never re-executes and never
  re-reads the downstream.
- The same key with a different request body never executes. It is `422`.
- The same key from a different `agent_id` is a different key entirely; namespaces do not
  overlap ([ADR-0002](decisions/0002-key-scope-and-status-lifecycle.md)).
