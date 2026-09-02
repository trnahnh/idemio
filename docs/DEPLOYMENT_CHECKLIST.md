# Deployment checklist

Owns configuration, go-live gates, and operational procedures. Gates are phase-scoped; a
gate is not satisfied by having the capability, only by having demonstrated it.

## Configuration reference

Every value that changes behaviour, with its default and the decision it comes from.

| Key | Default | Source | Notes |
|---|---|---|---|
| `claim.pending_wait_ms` | `0` | [ADR-0004](decisions/0004-concurrent-claim-resolution.md) | Bounded wait before returning `202`. Must never approach `downstream.timeout_ms`. |
| `conflict.window` | `5s` | [ADR-0007](decisions/0007-operation-compatibility-matrix.md) | Overridable per `resource_type` in the manifest. |
| `conflict.lock_timeout_ms` | `250` | [ADR-0008](decisions/0008-serialization-via-advisory-locks.md) | Set as the transaction `lock_timeout`. Exceeding it returns `202`, not an error. |
| `reconcile.stale_after` | `5m` | [ADR-0006](decisions/0006-reconciliation-never-resumes.md) | **Must be comfortably greater than `downstream.timeout_ms`.** Setting it below turns healthy in-flight writes into `indeterminate`. |
| `reconcile.interval` | `30s` | ADR-0006 | Reconciler sweep period. |
| `downstream.connect_timeout_ms` | `1000` | [ADR-0005](decisions/0005-downstream-outcome-taxonomy.md) | Separate from the read timeout so "not executed" is provable. |
| `downstream.timeout_ms` | `10000` | ADR-0005 | Response wait. Exceeding it yields `indeterminate`, never `failed`. |
| `limits.payload_bytes` | `262144` | [ADR-0003](decisions/0003-canonical-request-hashing.md) | Bounds canonicalization cost on the p50 path. |
| `limits.result_inline_bytes` | `65536` | [ADR-0009](decisions/0009-partitioning-and-retention.md) | Above this, results go to object storage via `result_ref`. |
| `retention.keys_days` | `90` | PRD §12 | Batched delete sweep. |
| `retention.intents_days` | `90` | PRD §12 | Detach-and-archive. |
| `retention.conflicts_days` | `365` | ADR-0009 | Answers a PRD §18 open question. |
| `partitions.keys_modulus` | `64` | ADR-0009 | **Immutable after first deploy** without a full table rewrite. |
| `outbox.enabled` | `false` | [ADR-0001](decisions/0001-postgres-primary-intent-log.md) | Phase 1+. Enabling is configuration, not migration. |

**The most dangerous misconfiguration** is `reconcile.stale_after` set at or below
`downstream.timeout_ms`. The reconciler would classify normal in-flight writes as stale,
producing a stream of false `indeterminate` records — which are terminal and require human
resolution. Validate this relation at startup and refuse to boot if it is violated.

## Pre-deploy gates: Phase 0

**Schema**

- [ ] Tables created **partitioned** — hash for `idempotency_keys`, range for
      `write_intents` — even though Phase 0 volume does not require it. Retrofitting is
      the expensive path.
- [ ] Unique constraint is exactly `(agent_id, idempotency_key)`, with no additional
      column. Verify in the live database, not in the migration file. This constraint is
      the guarantee.
- [ ] `key_status` enum contains all five values including `failed` and `indeterminate`.
- [ ] Postgres 18+ confirmed, or application-side UUIDv7 generation in place.

**Correctness**

- [ ] Concurrent-duplicate load test passes, verified against **downstream records**.
- [ ] Kill test passes: SIGKILL mid-downstream-call never produces a second execution.
- [ ] Reconciler binary has no downstream mutation client linked in. Verify structurally,
      not by reading configuration.
- [ ] Re-serialization test passes: reordered JSON keys and `4200.0` vs `4200` replay
      rather than returning `422`.
- [ ] Business-failure replay test passes.

**Operations**

- [ ] `indeterminate` alert wired to a pager and fired once in a drill. The rule is written
      in `deploy/alerts.yml`; what remains is a pager and a rehearsal.
- [ ] Stale-`pending` alert wired. The rule in `deploy/alerts.yml` compares the pending age
      against `idemio_reconcile_stale_after_seconds` rather than a copied threshold, so it
      cannot drift from the running configuration.
- [ ] Manual resolution procedure written and walked through by someone who did not write
      it (see below).
- [ ] Latency dashboard decomposed by the SYSTEM_DESIGN budget steps, so a regression can
      be attributed.
- [ ] Postgres HA configured with a synchronous replica and tested failover.

**Startup validation** — the process refuses to boot if any fails:

- [ ] `reconcile.stale_after > downstream.timeout_ms` by a stated margin.
- [ ] `claim.pending_wait_ms < downstream.timeout_ms`.
- [ ] Every `resource_type` declares an error classification and a probe path. Enforced by
      `resource.Validate()`; both binaries refuse to boot otherwise.
- [ ] The live unique constraint matches the expected column list.

## Pre-deploy gates: Phase 1

- [ ] Manifest schema validation in CI; an invalid manifest fails the build.
- [ ] Compatible-write test passes — disjoint-scope `mutate`s and two `append`s both
      succeed. Without this, the matrix is an expensive mutex.
- [ ] `409`-rate-per-agent alert live **before** conflict detection is enabled, since a
      bad manifest surfaces as mass rejection.
- [ ] Manifest changes are audited and reviewable as code.
- [ ] `pg_partman` configured with pre-creation headroom of at least 4 weeks, and the
      headroom alert pages.
- [ ] Key expiry sweep rate verified to exceed ingest rate under load.
- [ ] Archive restore drill completed — a detached partition restored and queried.
- [ ] Outbox relay lag alert live; Kafka outage drill shows zero write-path impact.
- [ ] PgBouncer in transaction mode.
- [ ] Redaction verified: `operator` cannot obtain payloads by any parameter combination.
- [ ] `payload_access_audit` rows confirmed for every `investigator` read.

## Pre-deploy gates: Phase 2

- [ ] 2,000 writes/sec sustained with latency targets met.
- [ ] Replica kill under load causes no failed writes and no double executions.
- [ ] Lock-wait and claim-collision metrics have at least one month of history, so the
      routing decision is read from data rather than argued.
- [ ] Rolling deploy verified not to strand `pending` keys — draining replicas finish or
      cleanly abandon in-flight calls.

## Operational procedures

### Resolving an `indeterminate` key

Required reading before Phase 0 go-live. This is manual by design: the system refuses to
guess ([ADR-0006](decisions/0006-reconciliation-never-resumes.md)).

1. Retrieve the intent: `GET /v1/resources/{type}/{id}/intents`, scoped to the window, with
   `include=payload` as `investigator`. This writes an audit row, which is expected.
2. Determine downstream truth by hand, using the correlation id derived from the
   idempotency key that every downstream call carries.
3. Resolve:
   - **Write landed** → set `status = 'done'` and store the retrieved result. The agent's
     next poll returns the real outcome.
   - **Write did not land** → set `status = 'failed'`. This is re-claimable, so the agent's
     retry with the same key proceeds safely.
   - **Cannot determine** → leave `indeterminate` and escalate to the resource owner. Do
     not guess. `failed` means *provably* no side effect, and a guess is not a proof.
4. Record the resolution and its evidence. A forced status change is a privileged action
   and is audited.

### Onboarding a new write path

1. Write the manifest entry: operation classes, scope selectors, conflict window override
   if needed.
2. Write the error classification — which downstream errors are definitive, which are
   provably-not-executed, which are indeterminate. Default anything ambiguous to
   indeterminate.
3. Write the probe, or record a signed-off acceptance that crash recovery for this path is
   manual. This is a decision to make during onboarding, not to discover during an
   incident.
4. Confirm the downstream call carries a correlation id derived from the idempotency key.
   Without it, no probe is possible.
5. Deploy in shadow first — the middleware records intents and claims keys but forwards to
   the existing path — then cut over.

### Rolling back a manifest change

Manifest changes are hot-reloadable and take effect without deploy, which makes them fast
to ship and fast to get wrong. A bad manifest surfaces as a `409` spike, not as an error.
Roll back by reverting the manifest commit; no deploy is required. The `409`-rate-per-agent
alert is the detection mechanism, which is why it must be live before conflict detection is
enabled.
