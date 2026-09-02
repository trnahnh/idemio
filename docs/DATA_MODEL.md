# Data model

This document owns the schema. DDL appears here and nowhere else; other documents link to
it. It supersedes PRD §7, which is retained as a record of the original design.

**Target:** PostgreSQL 18+. Version 18 provides the built-in `uuidv7()` function required
by [ADR-0009](decisions/0009-partitioning-and-retention.md). On 16 or 17, generate UUIDv7
in the application and drop the column defaults.

## Entity overview

```
idempotency_keys              (agent_id, idempotency_key)   HASH x64
  |  1..N   logical reference, no database FK  (ADR-0009)
  v
write_intents                 (intent_id, emitted_at)       RANGE weekly
  |  0..N   logical reference, no database FK
  v
conflicts                     (conflict_id, detected_at)    RANGE weekly

payload_access_audit          (audit_id, accessed_at)       RANGE monthly
```

The foreign keys declared in PRD §7 are intentionally absent. Postgres requires a foreign
key to reference a unique constraint containing the partition key, which would drag
partition columns into every referencing row. Integrity is enforced in the write path,
which is safe because the key claim and the intent insert commit in one transaction
([ADR-0001](decisions/0001-postgres-primary-intent-log.md)).

## Types

```sql
CREATE TYPE key_status AS ENUM (
    'pending',        -- claimed; downstream call in flight
    'done',           -- downstream gave a definitive outcome (success OR business failure)
    'failed',         -- downstream provably not reached; re-claimable
    'indeterminate',  -- outcome unknown; terminal, NOT re-claimable
    'rejected'        -- blocked by conflict detection or policy
);

CREATE TYPE conflict_resolution AS ENUM ('serialized', 'rejected', 'manual', 'observed');

CREATE TYPE operation_class AS ENUM ('create', 'replace', 'mutate', 'append', 'delete');
```

`key_status` extends PRD §7 with `failed` and `indeterminate`; see
[ADR-0002](decisions/0002-key-scope-and-status-lifecycle.md) and
[ADR-0005](decisions/0005-downstream-outcome-taxonomy.md). Permitted transitions:

```
(new) -> pending -> done | failed | indeterminate
(new) -> rejected
failed -> pending        (re-claim; the only transition out of a terminal state)
```

## idempotency_keys

The unique constraint on `(agent_id, idempotency_key)` is the at-most-once guarantee. It
is not an optimisation and must never be widened.

```sql
CREATE TABLE idempotency_keys (
    agent_id          TEXT        NOT NULL,
    idempotency_key   UUID        NOT NULL,
    request_hash      TEXT        NOT NULL,   -- sha256-jcs-v1 prefixed  (ADR-0003)
    status            key_status  NOT NULL DEFAULT 'pending',

    -- denormalised from the intent so replay and reconciliation need no join
    resource_type     TEXT        NOT NULL,
    resource_id       TEXT        NOT NULL,
    operation         TEXT        NOT NULL,

    result            JSONB,                  -- inline result, size-capped (ADR-0009)
    result_ref        TEXT,                   -- object-storage ref when over the cap
    outcome_detail    TEXT,                   -- classification note for failed/indeterminate

    attempt_count     INT         NOT NULL DEFAULT 1,   -- increments on failed -> pending
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at      TIMESTAMPTZ,

    PRIMARY KEY (agent_id, idempotency_key),
    CONSTRAINT result_inline_xor_ref CHECK (result IS NULL OR result_ref IS NULL),
    CONSTRAINT terminal_has_completed_at CHECK (
        status = 'pending' OR completed_at IS NOT NULL
    )
) PARTITION BY HASH (agent_id, idempotency_key);
```

Create 64 partitions. The count cannot be changed without rewriting the table, so it is
chosen with headroom rather than tuned to current volume.

```sql
-- repeat for remainder 0..63
CREATE TABLE idempotency_keys_p00 PARTITION OF idempotency_keys
    FOR VALUES WITH (MODULUS 64, REMAINDER 0);
```

```sql
-- Reconciler scan: stale pending keys. Partial, so it stays tiny regardless of table size.
CREATE INDEX idx_keys_stale_pending ON idempotency_keys (claimed_at)
    WHERE status = 'pending';

-- Retention sweep (ADR-0009: batched DELETE; hash partitions cannot be dropped by age).
CREATE INDEX idx_keys_created ON idempotency_keys (created_at);

-- Per-agent operational queries; carried over from PRD §7.
CREATE INDEX idx_keys_agent ON idempotency_keys (agent_id, created_at DESC);

-- Operator triage of unresolved outcomes.
CREATE INDEX idx_keys_indeterminate ON idempotency_keys (completed_at)
    WHERE status = 'indeterminate';
```

**Claim statement.** The single most important query in the system.

```sql
INSERT INTO idempotency_keys
    (agent_id, idempotency_key, request_hash, resource_type, resource_id, operation)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (agent_id, idempotency_key) DO NOTHING
RETURNING status;
```

Zero rows returned means the claim was lost; re-read the row and respond per
[ADR-0004](decisions/0004-concurrent-claim-resolution.md).

**Re-claim after `failed`,** guarded so two losers cannot both re-claim:

```sql
UPDATE idempotency_keys
   SET status = 'pending', claimed_at = now(), completed_at = NULL,
       attempt_count = attempt_count + 1, result = NULL, result_ref = NULL
 WHERE agent_id = $1 AND idempotency_key = $2 AND status = 'failed'
RETURNING attempt_count;
```

## write_intents

Source of truth for the intent log and the only table the conflict check reads
([ADR-0001](decisions/0001-postgres-primary-intent-log.md)). `published_at` is the outbox
watermark: it exists from the first migration so that enabling Kafka relay in Phase 1 is
configuration, not migration.

```sql
CREATE TABLE write_intents (
    intent_id         UUID            NOT NULL DEFAULT uuidv7(),
    agent_id          TEXT            NOT NULL,
    idempotency_key   UUID            NOT NULL,

    resource_type     TEXT            NOT NULL,
    resource_id       TEXT            NOT NULL,
    operation         TEXT            NOT NULL,
    operation_class   operation_class NOT NULL,   -- from the manifest (ADR-0007)
    scope_selector    TEXT[],                     -- NULL = whole resource

    payload           JSONB           NOT NULL,
    emitted_at        TIMESTAMPTZ     NOT NULL DEFAULT now(),
    published_at      TIMESTAMPTZ,                -- outbox watermark; NULL = unpublished
    voided_at         TIMESTAMPTZ,                -- the write provably did not happen (ADR-0015)

    PRIMARY KEY (intent_id, emitted_at)
) PARTITION BY RANGE (emitted_at);
```

```sql
-- The conflict-check query. Prunes to one partition for any window of seconds.
CREATE INDEX idx_intents_resource
    ON write_intents (resource_type, resource_id, emitted_at DESC);

-- Intent history for a key (replay debugging, reconciliation).
CREATE INDEX idx_intents_key ON write_intents (agent_id, idempotency_key);

-- Outbox relay cursor. Partial, so it holds only the unpublished backlog.
CREATE INDEX idx_intents_unpublished ON write_intents (emitted_at)
    WHERE published_at IS NULL;
```

`voided_at` is set when conflict detection rejects the write, and when a key completes as
`failed`. An intent participates in the conflict window unless the write it records
provably did not happen; without it one rejection cascades into the next
([ADR-0015](decisions/0015-conflict-check-transaction-shape.md)). `indeterminate` and
`pending` intents are never voided, because they may have executed.

**Conflict-check query,** run under the advisory lock from
[ADR-0008](decisions/0008-serialization-via-advisory-locks.md) — which
[ADR-0015](decisions/0015-conflict-check-transaction-shape.md) makes the transaction's
first statement — and evaluated against the matrix in
[ADR-0007](decisions/0007-operation-compatibility-matrix.md):

```sql
SELECT intent_id, agent_id, idempotency_key, operation, operation_class, scope_selector
  FROM write_intents
 WHERE resource_type = $1
   AND resource_id   = $2
   AND emitted_at > now() - $3::interval   -- per-resource_type window, default 5s
   AND voided_at IS NULL                   -- writes that did not happen do not conflict
   AND intent_id <> $4;                    -- exclude our own just-inserted intent
```

## conflicts

Lower volume, but partitioned for retention symmetry. Retention is one year rather than 90
days, which is affordable at this volume and answers the second PRD §18 open question.

```sql
CREATE TABLE conflicts (
    conflict_id       UUID                NOT NULL DEFAULT uuidv7(),
    intent_id_a       UUID                NOT NULL,   -- the incoming intent
    intent_id_b       UUID                NOT NULL,   -- the intent it conflicted with
    agent_id_a        TEXT                NOT NULL,
    agent_id_b        TEXT                NOT NULL,
    resource_type     TEXT                NOT NULL,
    resource_id       TEXT                NOT NULL,
    reason            TEXT                NOT NULL,   -- e.g. mutate/mutate overlapping scope
    resolution        conflict_resolution NOT NULL,
    manifest_version  TEXT,                           -- the ruleset that judged it (ADR-0013)
    detected_at       TIMESTAMPTZ         NOT NULL DEFAULT now(),

    PRIMARY KEY (conflict_id, detected_at)
) PARTITION BY RANGE (detected_at);

CREATE INDEX idx_conflicts_detected ON conflicts (detected_at DESC);
CREATE INDEX idx_conflicts_agent    ON conflicts (agent_id_a, detected_at DESC);
CREATE INDEX idx_conflicts_resource ON conflicts (resource_type, resource_id, detected_at DESC);
```

## manifest_activations

Records which manifest version a process was serving when it judged a write. Git owns what
the manifest said; nothing else records where and when it was live
([ADR-0013](decisions/0013-phase-1-implementation-stack.md)). Low volume and unpartitioned:
one row per process per version.

```sql
CREATE TABLE manifest_activations (
    activation_id     UUID        NOT NULL DEFAULT uuidv7(),
    manifest_version  TEXT        NOT NULL,   -- content hash of the manifest directory
    principal         TEXT        NOT NULL,   -- host and pid of the serving process
    resource_types    TEXT[]      NOT NULL,
    activated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (activation_id)
);

CREATE UNIQUE INDEX idx_manifest_activations_unique
    ON manifest_activations (principal, manifest_version);
CREATE INDEX idx_manifest_activations_time
    ON manifest_activations (activated_at DESC);
```

## payload_access_audit

Required by [ADR-0011](decisions/0011-read-api-access-control.md). It records **which**
records were accessed, never their contents, so it does not become an unaudited copy of
the data it protects.

```sql
CREATE TABLE payload_access_audit (
    audit_id          UUID        NOT NULL DEFAULT uuidv7(),
    principal         TEXT        NOT NULL,   -- verified caller identity
    caller_role       TEXT        NOT NULL,   -- operator | investigator
    endpoint          TEXT        NOT NULL,
    query_params      JSONB       NOT NULL,
    record_count      INT         NOT NULL,
    intent_ids        UUID[],                 -- identifiers only, never payloads
    stated_reason     TEXT,
    accessed_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (audit_id, accessed_at)
) PARTITION BY RANGE (accessed_at);
```

## Data classification

Per [ADR-0011](decisions/0011-read-api-access-control.md). Restated here because this is
where schema changes happen and where the classification must be visible.

| Column | Class | Notes |
|---|---|---|
| `write_intents.payload` | **Confidential** | Same tier as the downstream system's own data. Redacted by default on all read APIs. |
| `idempotency_keys.result` / `result_ref` | **Confidential** | Full downstream response bodies. PRD §14 omitted this; it is as sensitive as `payload`. |
| `idempotency_keys.request_hash` | Internal | A digest, but confirms request equality. Not returned by any API. |
| `conflicts.*` | Internal | Metadata only. Available to `operator`. |
| `payload_access_audit.*` | **Confidential** | Reveals investigative activity. Access limited to security roles. |
| `idempotency_keys.result` when `status = 'rejected'` | Internal | A conflict rejection body, not a downstream response. Redaction treats the column uniformly as Confidential, which over-classifies in this case — the safe direction. |
| `manifest_activations.*` | Internal | Which ruleset was live where. |
| all other columns | Internal | Resource identifiers, operations, timestamps, statuses. |

## Retention

| Table | Mechanism | Hot window |
|---|---|---|
| `idempotency_keys` | Continuous batched `DELETE` on `created_at`, rate-matched to ingest | 90 days |
| `write_intents` | `DETACH PARTITION`, export to Parquet, `DROP` | 90 days |
| `conflicts` | `DETACH PARTITION`, export, `DROP` | 1 year |
| `payload_access_audit` | `DETACH PARTITION`, export, `DROP` | 1 year |
| `manifest_activations` | None; retained indefinitely | — |

The mechanisms differ for the reason given in
[ADR-0009](decisions/0009-partitioning-and-retention.md): hash partitions cannot be dropped
by age, and preserving the global unique constraint on `idempotency_keys` takes precedence
over cheap archival. Archive restore must be drilled, not assumed; see
[DEPLOYMENT_CHECKLIST.md](DEPLOYMENT_CHECKLIST.md).

## Not in the database

The **operation manifest** (per `resource_type`: operation classes, scope selectors,
conflict window, enforcement, error classification, probe path) is versioned configuration,
not a table. It is hot-reloadable, reviewed as code, and its activations are recorded in
`manifest_activations`. Keeping it out of the database makes a manifest change a reviewed
deploy artifact rather than a production `UPDATE`. Its format and lifecycle are owned by
[ADR-0013](decisions/0013-phase-1-implementation-stack.md).
