CREATE TYPE key_status AS ENUM (
    'pending',
    'done',
    'failed',
    'indeterminate',
    'rejected'
);

CREATE TYPE conflict_resolution AS ENUM ('serialized', 'rejected', 'manual');

CREATE TYPE operation_class AS ENUM ('create', 'replace', 'mutate', 'append', 'delete');

CREATE TABLE idempotency_keys (
    agent_id          TEXT        NOT NULL,
    idempotency_key   UUID        NOT NULL,
    request_hash      TEXT        NOT NULL,
    status            key_status  NOT NULL DEFAULT 'pending',

    resource_type     TEXT        NOT NULL,
    resource_id       TEXT        NOT NULL,
    operation         TEXT        NOT NULL,

    result            JSONB,
    result_ref        TEXT,
    outcome_detail    TEXT,

    attempt_count     INT         NOT NULL DEFAULT 1,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at      TIMESTAMPTZ,

    PRIMARY KEY (agent_id, idempotency_key),
    CONSTRAINT result_inline_xor_ref CHECK (result IS NULL OR result_ref IS NULL),
    CONSTRAINT terminal_has_completed_at CHECK (
        status = 'pending' OR completed_at IS NOT NULL
    )
) PARTITION BY HASH (agent_id, idempotency_key);

CREATE INDEX idx_keys_stale_pending ON idempotency_keys (claimed_at)
    WHERE status = 'pending';
CREATE INDEX idx_keys_created ON idempotency_keys (created_at);
CREATE INDEX idx_keys_agent ON idempotency_keys (agent_id, created_at DESC);
CREATE INDEX idx_keys_indeterminate ON idempotency_keys (completed_at)
    WHERE status = 'indeterminate';

CREATE TABLE write_intents (
    intent_id         UUID            NOT NULL DEFAULT uuidv7(),
    agent_id          TEXT            NOT NULL,
    idempotency_key   UUID            NOT NULL,

    resource_type     TEXT            NOT NULL,
    resource_id       TEXT            NOT NULL,
    operation         TEXT            NOT NULL,
    operation_class   operation_class NOT NULL,
    scope_selector    TEXT[],

    payload           JSONB           NOT NULL,
    emitted_at        TIMESTAMPTZ     NOT NULL DEFAULT now(),
    published_at      TIMESTAMPTZ,

    PRIMARY KEY (intent_id, emitted_at)
) PARTITION BY RANGE (emitted_at);

CREATE INDEX idx_intents_resource
    ON write_intents (resource_type, resource_id, emitted_at DESC);
CREATE INDEX idx_intents_key ON write_intents (agent_id, idempotency_key);
CREATE INDEX idx_intents_unpublished ON write_intents (emitted_at)
    WHERE published_at IS NULL;

CREATE TABLE conflicts (
    conflict_id       UUID                NOT NULL DEFAULT uuidv7(),
    intent_id_a       UUID                NOT NULL,
    intent_id_b       UUID                NOT NULL,
    agent_id_a        TEXT                NOT NULL,
    agent_id_b        TEXT                NOT NULL,
    resource_type     TEXT                NOT NULL,
    resource_id       TEXT                NOT NULL,
    reason            TEXT                NOT NULL,
    resolution        conflict_resolution NOT NULL,
    detected_at       TIMESTAMPTZ         NOT NULL DEFAULT now(),

    PRIMARY KEY (conflict_id, detected_at)
) PARTITION BY RANGE (detected_at);

CREATE INDEX idx_conflicts_detected ON conflicts (detected_at DESC);
CREATE INDEX idx_conflicts_agent    ON conflicts (agent_id_a, detected_at DESC);
CREATE INDEX idx_conflicts_resource ON conflicts (resource_type, resource_id, detected_at DESC);

CREATE TABLE payload_access_audit (
    audit_id          UUID        NOT NULL DEFAULT uuidv7(),
    principal         TEXT        NOT NULL,
    caller_role       TEXT        NOT NULL,
    endpoint          TEXT        NOT NULL,
    query_params      JSONB       NOT NULL,
    record_count      INT         NOT NULL,
    intent_ids        UUID[],
    stated_reason     TEXT,
    accessed_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (audit_id, accessed_at)
) PARTITION BY RANGE (accessed_at);

DO $$
BEGIN
    FOR i IN 0..63 LOOP
        EXECUTE format(
            'CREATE TABLE idempotency_keys_p%s PARTITION OF idempotency_keys '
            'FOR VALUES WITH (MODULUS 64, REMAINDER %s)',
            lpad(i::text, 2, '0'), i
        );
    END LOOP;
END $$;

DO $$
DECLARE
    week_start TIMESTAMPTZ := date_trunc('week', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';
    month_start TIMESTAMPTZ := date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';
    lo TIMESTAMPTZ;
    hi TIMESTAMPTZ;
BEGIN
    FOR i IN 0..11 LOOP
        lo := week_start + (i * INTERVAL '7 days');
        hi := lo + INTERVAL '7 days';
        EXECUTE format(
            'CREATE TABLE write_intents_%s PARTITION OF write_intents FOR VALUES FROM (%L) TO (%L)',
            to_char(lo AT TIME ZONE 'UTC', 'YYYYMMDD'), lo, hi
        );
        EXECUTE format(
            'CREATE TABLE conflicts_%s PARTITION OF conflicts FOR VALUES FROM (%L) TO (%L)',
            to_char(lo AT TIME ZONE 'UTC', 'YYYYMMDD'), lo, hi
        );
    END LOOP;

    FOR i IN 0..2 LOOP
        lo := month_start + (i * INTERVAL '1 month');
        hi := lo + INTERVAL '1 month';
        EXECUTE format(
            'CREATE TABLE payload_access_audit_%s PARTITION OF payload_access_audit '
            'FOR VALUES FROM (%L) TO (%L)',
            to_char(lo AT TIME ZONE 'UTC', 'YYYYMM'), lo, hi
        );
    END LOOP;
END $$;
