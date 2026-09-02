# ADR-0016: Maintain range partitions in application code instead of `pg_partman`

- **Status:** Accepted
- **Date:** 2026-09-01
- **Phase:** 1
- **Supersedes:** the `pg_partman` clause of [ADR-0009](0009-partitioning-and-retention.md)

## Context

[ADR-0009](0009-partitioning-and-retention.md) specifies that `write_intents` is
range-partitioned weekly, "managed by `pg_partman` with partitions pre-created well ahead
of need," and records as a consequence that "`pg_partman` becomes an operational dependency
for `write_intents`, with its own monitoring."

`pg_partman` is a Postgres extension. Adopting it means the deployed database is no longer
stock `postgres:18` but a custom image carrying the extension, and the CI service that
tests migrations is either that same custom image or a different database from the one that
runs in production. The second option is worse than it sounds: partition maintenance is
precisely the subsystem whose failure mode ADR-0009 describes as "a hard outage on the
write path," and it would be the subsystem least exercised by the test suite.

The work itself is also already half done. Migration `0001` creates the initial ranges for
all three range-partitioned tables directly in SQL. `internal/store` already reads
`pg_class` and `pg_inherits` to compute how far ahead the newest partition ends, exposes it
as `idemio_partition_headroom_seconds`, and refuses to boot `cmd/idemio` below two weeks of
headroom. What is missing is not a partition manager; it is a loop that calls `CREATE TABLE
... PARTITION OF` on a schedule.

## Decision

Partition maintenance is a Go component running inside `cmd/reconciler`. On each tick it
ensures that every range-partitioned table has partitions covering at least
`IDEMIO_PARTITION_AHEAD` (default 8 weeks) beyond now, creating any that are missing. It is
idempotent, so concurrent replicas racing to create the same partition is not an error.

`pg_partman` is not installed. The deployed database stays stock `postgres:18`, and the
image that CI tests against is the image that runs.

The retention half of ADR-0009 is unchanged in mechanism: `write_intents`, `conflicts` and
`payload_access_audit` are retired by `DETACH PARTITION`, export, then `DROP`, and
`idempotency_keys` by a rate-matched batched `DELETE`. Only the tool that creates the
partitions changes.

## Alternatives considered

**Install `pg_partman` as ADR-0009 specifies.** Rejected on the testability argument above.
It is a good extension; the cost is not its quality but the divergence it forces between
the tested database and the deployed one.

**Create partitions in a `cron` job or a Kubernetes `CronJob` running `psql`.** Rejected.
It puts a correctness-critical operation outside the application's own health surface,
where its failure is visible only if someone separately monitors the job, and it duplicates
the headroom logic that `internal/store` already owns.

**Create a year of partitions in the migration and revisit annually.** Rejected. It works,
right up until the year nobody remembers, and the failure is a total write-path outage with
no leading indicator other than the headroom gauge that this ADR's component exists to act
on.

## Consequences

- Partition creation is now covered by the same test suite, against the same database
  image, as everything else. The maintainer can be tested by asking it to create partitions
  for a table with none and asserting the headroom gauge afterwards.
- `cmd/reconciler` acquires a second responsibility beyond crash recovery. It remains
  structurally incapable of a downstream write, which is the invariant that matters, and
  partition maintenance is database housekeeping on the same cadence as reconciliation.
- The alerting relationship is unchanged and now closes a loop: `cmd/idemio` refuses to
  boot below two weeks of headroom, `IdemioPartitionHeadroomLow` pages below four weeks,
  and the maintainer keeps eight. A page therefore means the maintainer itself has stopped,
  which is a more specific signal than it was when nothing was creating partitions.
- We own a component we would otherwise have imported. It is roughly one query and one
  `CREATE TABLE`, and it is worth writing to keep the database stock.
