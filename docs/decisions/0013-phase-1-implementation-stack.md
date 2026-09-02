# ADR-0013: Phase 1 implementation stack

- **Status:** Accepted
- **Date:** 2026-09-01
- **Phase:** 1

## Context

[ADR-0012](0012-phase-0-implementation-stack.md) recorded the choices Phase 0 needed before
its first line of code. Phase 1 adds conflict detection, the operation manifest, the two
read endpoints, the outbox relay, retention and archival, and connection pooling. Several
of those choices constrain each other, and several of them are cheap now and expensive to
revisit once data exists. This ADR records them in one place.

The decisions that turned out to be corrections rather than choices are recorded separately,
because they supersede accepted ADRs and must be findable as such:
[ADR-0014](0014-undeclared-operations-rejected-at-admission.md),
[ADR-0015](0015-conflict-check-transaction-shape.md),
[ADR-0016](0016-partition-maintenance-in-application-code.md), and
[ADR-0017](0017-read-api-time-bounds-are-mandatory.md).

## Decision

### The manifest is JSON on disk, validated by hand-written Go

One file per `resource_type` in `IDEMIO_MANIFEST_DIR`. It replaces the hard-coded registry
in `internal/resource` and carries everything [ADR-0007](0007-operation-compatibility-matrix.md)
requires of it: operation classes, scope selectors, the conflict window, the error
classification from [ADR-0005](0005-downstream-outcome-taxonomy.md), and the probe path
from [ADR-0006](0006-reconciliation-never-resumes.md).

Validation is a hand-written validator in the style of `config.Validate` — every problem
collected, never the first one only. [ROADMAP.md](../ROADMAP.md) says "schema-validated";
this satisfies the requirement without a JSON Schema dependency. The manifest is small, is
reviewed as code, and hand-written checks produce better messages than a schema library
does for the errors that actually occur, which are semantic rather than structural: a class
that is not in the enum, a probe path that is not a path, a window of zero.

**Reload is a poll of the directory's content hash**, every
`IDEMIO_MANIFEST_RELOAD_INTERVAL` (default 30s). A mounted configuration file changes
underneath a running process and signals nothing, so `SIGHUP` would make "without deploy"
mean "with an operator holding exec access to every replica." `fsnotify` would be
event-driven, but a ConfigMap update lands as an atomic symlink swap of the directory
rather than as writes to the files, so the case it most needs to catch is the one it most
often misses. The hash is needed for versioning regardless, so polling it costs one
directory read per interval.

**A failed reload is all-or-nothing and keeps the last known-good manifest.** If any file
fails validation the whole reload is rejected, so the live ruleset is always one artifact
that was valid together rather than a mixture of two that were never reviewed together.
The process continues serving the previous manifest, increments
`idemio_manifest_reload_failures_total`, and logs every problem.

Boot and runtime differ deliberately. `cmd/idemio` refuses to start on an invalid manifest,
exactly as `resource.Validate()` already refuses to start on an incomplete registry. A
running process degrades instead. A bad manifest reaching production must not be able to
stop the fleet, and "conflict detection is running slightly stale rules" is a far smaller
incident than "the write path is down" — particularly now that
[ADR-0014](0014-undeclared-operations-rejected-at-admission.md) makes the manifest
load-bearing for admission.

### Manifest activations are recorded in the database

Git owns what the manifest said; it is reviewed as code. What nothing currently records is
which version a given process was actually running at a given moment, and that is the fact
which cannot be reconstructed afterwards. `manifest_activations` records the content hash,
the activation time, the process identity and the `resource_type`s covered, once per
process per version.

`conflicts` gains `manifest_version`. A conflict verdict is otherwise unexplainable after
the rules change: the class assignments, scope selectors and window that produced it are
all gone. `write_intents` is not stamped, because it already denormalises `operation_class`
and `scope_selector` onto every row and is therefore self-describing. Correlating a verdict
by activation timestamp instead would work everywhere except mid-rollout, when replicas
genuinely disagree — which is exactly when a surprising `409` needs explaining.

### Conflict enforcement is off by default, per `resource_type`

Each manifest declares `enforce`. When false, the conflict check runs in full, records
`conflicts` rows with `resolution = 'observed'`, and drives every metric, but never rejects
a write. Enabling is a single field in a hot-reloadable, audited, per-type artifact.

[METRICS.md](../METRICS.md) already requires the `409` rate to be live *before* conflict
detection is enabled, because a wrong manifest surfaces as mass rejection. Shadow mode is
what makes that requirement satisfiable: onboarding a write path means watching what the
matrix would have done to real traffic, then flipping one field. `conflict_resolution`
gains `observed` so a shadow row can never be mistaken for a real rejection — adding an
enum value is cheap now and awkward once a year of data exists.

### The outbox relay polls, and lives in its own binary

`cmd/relay` selects unpublished intents using the partial index that
[DATA_MODEL.md](../DATA_MODEL.md) already declares, publishes them, and stamps
`published_at`. Logical decoding would be lower-latency, but a replication slot that stops
advancing retains WAL until the disk fills — a total database outage caused by the one
component [ADR-0001](0001-postgres-primary-intent-log.md) deliberately moved *off* the
correctness path. Polling structurally cannot take down the write path; the slot can.

It is a separate binary rather than another job inside `cmd/reconciler` because it is the
only one of the three background jobs that talks to an external system, and therefore the
only one that can be down, backed up, or restarted for reasons unrelated to the database.
Coupling the Kafka client's availability to crash recovery would be exactly backwards.
Partition maintenance ([ADR-0016](0016-partition-maintenance-in-application-code.md)) and
the retention sweep stay in `cmd/reconciler`, where they belong: pure database housekeeping
on the same cadence as reconciliation.

Publication is at-least-once, as ADR-0001 requires. Redpanda runs in `compose.yaml` so that
the Phase 1 exit criterion — Kafka down for an hour with zero write-path impact — is
demonstrated rather than asserted.

### Archives are Parquet in object storage

As [ADR-0009](0009-partitioning-and-retention.md) specifies, with MinIO in `compose.yaml`
standing in for object storage so the restore drill exercises the whole path including
credentials and multipart upload, not just the `DETACH` and the `DROP`.

Parquet's columnar advantage is unused today; nothing queries the archive yet. It is
adopted now anyway, on the same reasoning that put partitioning in Phase 0: the archive
format is the thing that cannot be changed cheaply after data exists, because changing it
means either rewriting every archive or maintaining two readers indefinitely.

### PgBouncer is deployed in transaction mode, and the request path is tested through it

Transaction-mode pooling forbids session-scoped state. ADR-0008's `pg_advisory_xact_lock`
is transaction-scoped and therefore safe, but `store.Migrate` holds a *session* advisory
lock and `internal/testdb` creates databases, and both require a direct connection.

`internal/pooled` runs its whole suite through the pooler: the write path, replay, the
conflict check, concurrent claims, the read endpoints, and an audited read. That is where
the risk actually is, because those are the paths that run continuously under load. The
remaining packages stay direct, for speed and because some of them genuinely need a session.

**Migrations must use a direct connection, and this is enforced by configuration rather
than by code.** The intended design was a test asserting that migrations through the pooler
fail loudly. That test cannot be written honestly. PgBouncer does not reject session-scoped
statements in transaction mode — `SET`, `LISTEN` and `pg_advisory_lock` are all accepted —
and with an idle pool the same server connection is handed back each time, so session state
survives and everything appears to work. The failure only emerges under concurrency, when
the lock and the unlock land on different backends. A client-side check has the same blind
spot for the same reason: it would pass on a quiet pool and prove nothing.

So the rule is operational, and stated where operators will meet it: the migration URL
points at Postgres directly, never at PgBouncer, and
[DEPLOYMENT_CHECKLIST.md](../DEPLOYMENT_CHECKLIST.md) carries it as a gate. Recording the
gap here is deliberate — a test that passes on a quiet pool would have been worse than no
test, because it would have retired the question.

### Metrics are labelled by `resource_type`, never by `agent_id`

[METRICS.md](../METRICS.md) asks for the `409` rate per `agent_id` and `resource_type`, and
lists `idemio_hash_mismatches_total{agent_id}` under *Known gaps* precisely because that
cardinality is unbounded and caller-influenced. Phase 1 is where that becomes a pattern or
stops being one.

Conflict metrics are labelled by `resource_type` and `resolution`, and
`idemio_hash_mismatches_total` is relabelled to match. Per-agent detail is not lost:
`GET /v1/conflicts` owns that fact, is indexed on `(agent_id_a, detected_at)`, and answers
questions a counter cannot. Prometheus is the wrong store for unbounded identity and the
right store was already being built.

### The Phase 1 fixture carries two resource types

`invoice` gains `add_line_item` (`append`), `update_status` (`mutate`, scope `["status"]`)
and `update_billing_address` (`mutate`, scope `["billing_address"]`). Those three make
exit criteria 1–3 testable: `append`/`append` is compatible, two disjoint `mutate`s are
compatible, and `mutate`/`replace` conflicts.

A second type, `subscription`, is added with a deliberately different conflict window and a
different error classification. With one type in the registry, every per-`resource_type`
code path is trivially correct because there is nothing for it to be wrong about.

## Alternatives considered

**YAML for the manifest.** Better to review and it supports comments. Rejected for one
dependency on a path where the manifest now gates admission.

**A JSON Schema library.** Satisfies "schema-validated" literally and yields a
machine-readable artifact for integrators. Rejected: a heavier dependency producing worse
messages for the errors that occur in practice. If integrators later need a published
schema, it can be generated from the validator rather than replacing it.

**Enforcing conflict detection from the first request.** No shadow path and no second code
path that could drift from the enforcing one. Rejected because the first bad manifest would
then be discovered as mass `409`s in production, which METRICS.md already identifies as the
expected failure of this feature.

**A global enforcement kill switch instead of a per-type flag.** Simpler, and no enum
change. Rejected: it is all-or-nothing across every write path, and turning it off is a
deploy rather than a manifest change.

**Gzipped JSONL archives.** No Parquet dependency and restore is a plain `COPY`. Rejected
on the format-permanence argument above.

## Consequences

- `internal/resource` is deleted. Everything that read the registry now reads a manifest
  snapshot, and every component that needs one must handle it changing under them.
- The manifest is a deployment artifact with its own review, validation, rollout and audit
  story. That is the cost of [ADR-0007](0007-operation-compatibility-matrix.md)'s central
  claim that declarations replace inference, and it is now real work rather than a table in
  a document.
- Four new components run in production: the relay, the partition maintainer, the retention
  sweep, and the archive exporter. Each needs its own lag metric and its own alert, and
  three of them are new failure modes that did not exist in Phase 0.
- `compose.yaml` grows Redpanda, MinIO and PgBouncer. Local development and CI now start
  four containers instead of one.
- Two schema changes land in migration `0002` that are cheap now and would be expensive
  later: `write_intents.voided_at`, and the `observed` value on `conflict_resolution`.
