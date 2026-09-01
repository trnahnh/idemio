# ADR-0009: Partition each table by what it must guarantee, not uniformly

- **Status:** Accepted
- **Date:** 2026-09-01
- **Phase:** 1

## Context

PRD §12 sets two requirements that its own schema in PRD §7 cannot satisfy together:

- Throughput: 2,000 writes/sec sustained per region.
- Retention: idempotency keys 90 days hot in Postgres, archived to cold storage after.

2,000 writes/sec is 172.8 million rows per day. Ninety days is **roughly 15.5 billion
rows** in `idempotency_keys`, and [ADR-0001](0001-postgres-primary-intent-log.md) puts an
equal volume into `write_intents` in the same database. The PRD §7 DDL declares both as
ordinary unpartitioned tables.

At that size the problems are concrete. A single B-tree over 15 billion rows will not keep
its hot pages in cache; random UUID primary keys scatter inserts across the whole index, so
each insert is likely to dirty a cold page; and archival becomes a `DELETE` of ~172M rows
per day, generating enormous WAL, bloating the heap, and leaving autovacuum permanently
behind.

The obvious fix, range-partitioning both tables on time, **cannot be applied to
`idempotency_keys`**. Postgres requires every unique constraint on a partitioned table to
include the partition key. Range-partitioning on `created_at` would make the constraint
`(agent_id, idempotency_key, created_at)`, under which two claims for the same key on
different days both succeed. That unique constraint is not an index; per PRD §11 it *is*
the at-most-once guarantee. Partitioning must not be allowed to weaken it.

## Decision

The two tables are partitioned differently, because they must guarantee different things.

**`idempotency_keys` is HASH partitioned on `(agent_id, idempotency_key)`** — 64
partitions initially. The partition key is a subset of the unique key, so the global unique
constraint on `(agent_id, idempotency_key)` is preserved exactly. Every lookup and every
claim is a point operation on that tuple, so it prunes to a single partition. The hot index
is 1/64th the size, which is what fixes the cache-residency problem.

**`write_intents` is RANGE partitioned on `emitted_at`**, weekly, managed by `pg_partman`
with partitions pre-created well ahead of need. It has no uniqueness requirement beyond its
own surrogate id, so nothing is lost. It is also the larger table by bytes, since it holds
`payload`. Conflict-check queries filter on a window of seconds and prune to one partition.

**Retention differs accordingly:**

- `write_intents`: `DETACH PARTITION`, export to Parquet in object storage, then `DROP`.
  Metadata-only until the drop, no WAL amplification, no vacuum debt. This is the clean
  path, and it applies to the table carrying the bulk of the volume.
- `idempotency_keys`: hash partitions cannot be dropped by age, so expiry is a batched
  `DELETE` driven by a per-partition index on `created_at`, run continuously at a rate
  matched to ingest rather than in a daily burst. This is the price of keeping the unique
  constraint global, and it is the right price to pay. It is made affordable by keeping the
  row narrow: `result` is capped in size by configuration, and oversized results are stored
  by reference rather than inline.

**Server-generated identifiers use UUIDv7.** `intent_id` and `conflict_id` become UUIDv7,
which is time-ordered, so inserts land at the right edge of their index. `idempotency_key`
remains client-generated UUIDv4 as PRD §9 requires; we do not dictate client key
generation, and hash partitioning makes its scatter harmless.

**The database-level foreign keys are dropped.** A foreign key targeting a partitioned
table must reference a unique constraint containing the partition key, which would drag
partition keys into every referencing row. Referential integrity is enforced in the write
path instead, which is safe because ADR-0001 places the key claim and the intent insert in
a single transaction.

## Alternatives considered

**Range-partition `idempotency_keys` on time and enforce uniqueness in the application.**
Rejected, emphatically. It replaces a database-enforced invariant with application logic at
exactly the point where PRD §11 locates the entire correctness argument.

**Range-partition on time and add a global unique index outside the partitioning.**
Not possible in Postgres; there is no global index across partitions.

**Leave `idempotency_keys` unpartitioned.** Rejected. The 15-billion-row index is the
original problem, and hash partitioning solves it at no cost to the guarantee.

**Hash-partition both tables.** Rejected for `write_intents`. It would forfeit
DETACH-based archival on the table where it works best, and conflict queries scoped to a
time window would have to touch every partition.

**Monthly or daily range partitions for `write_intents`.** Monthly reproduces the size
problem inside each partition; daily gives 90+ live partitions and inflates planning time
for queries that cannot prune. Weekly is the middle, and is revisitable per region.

## Consequences

- Two different retention mechanisms must be built and monitored, not one. The
  `idempotency_keys` expiry job is a continuous background process whose lag is an alert.
- `pg_partman` becomes an operational dependency for `write_intents`, with its own
  monitoring: partitions must exist before they are needed, and failure to pre-create is a
  hard outage on the write path.
- Changing the hash partition count later requires a full rewrite of the table, so 64 is
  chosen with headroom rather than tuned to current volume.
- The archive path needs a verified restore procedure. Cold data never read back is not
  archived, it is deleted with extra steps. A restore drill belongs in the Phase 1
  checklist.
- Dropped FKs mean application-level integrity, covered by tests rather than assumed. Any
  future change that splits ADR-0001's single transaction reopens this.
- Read endpoints must accept and apply a bounded time range with a non-infinite default,
  or they will scan every `write_intents` partition. This is enforced in
  `API_REFERENCE.md` and reinforced by [ADR-0011](0011-read-api-access-control.md).
- Partitioning is a Phase 1 concern by volume but a **Phase 0 concern by migration cost**.
  Converting populated tables later is far more disruptive than starting partitioned, so
  the Phase 0 schema is created partitioned from the first migration.
