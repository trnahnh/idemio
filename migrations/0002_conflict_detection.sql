ALTER TYPE conflict_resolution ADD VALUE 'observed';

ALTER TABLE write_intents ADD COLUMN voided_at TIMESTAMPTZ;

ALTER TABLE conflicts ADD COLUMN manifest_version TEXT;

CREATE TABLE manifest_activations (
    activation_id     UUID        NOT NULL DEFAULT uuidv7(),
    manifest_version  TEXT        NOT NULL,
    principal         TEXT        NOT NULL,
    resource_types    TEXT[]      NOT NULL,
    activated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (activation_id)
);

CREATE UNIQUE INDEX idx_manifest_activations_unique
    ON manifest_activations (principal, manifest_version);
CREATE INDEX idx_manifest_activations_time
    ON manifest_activations (activated_at DESC);
