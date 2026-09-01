# Open questions

Genuinely unresolved items. Everything the PRD left open that *has* been settled is
recorded in [decisions/](decisions/) and removed from this list — check there before
reopening an argument.

Each item names what would have to be true to decide it, so that it is resolved by
evidence rather than by whoever raises it last.

## Resolved since PRD §18

The PRD's four open questions are all answered. Kept here as a pointer so nobody
re-litigates them from the frozen document.

| PRD §18 question | Answer |
|---|---|
| Fixed or per-`resource_type` conflict window? | **Per-`resource_type`**, defaulting to 5s, declared in the manifest. [ADR-0007](decisions/0007-operation-compatibility-matrix.md) |
| Retention policy for the `conflicts` table? | **1 year**, range-partitioned, detach-and-archive. Affordable at that volume. [ADR-0009](decisions/0009-partitioning-and-retention.md), [DATA_MODEL.md](DATA_MODEL.md) |
| Should `503` carry a retry-after, and how computed? | **Yes.** p95 of recent downstream latency for the `resource_type`, clamped to `[50ms, 5s]`. Same computation serves `202`. [ADR-0004](decisions/0004-concurrent-claim-resolution.md) |
| Per-agent rate limiting in v1, or dashboard review until Phase 3? | **Dashboard review until Phase 3.** `429` is reserved in [API_REFERENCE.md](API_REFERENCE.md) so adding it later is not a breaking change. |

---

## Open

### Q1. Is `agent_id` the right isolation boundary, or is a tenant layer needed above it?

[ADR-0002](decisions/0002-key-scope-and-status-lifecycle.md) scopes keys to
`(agent_id, idempotency_key)`. That is correct isolation for the deployment the PRD
describes, where all agents belong to one organisation. It is insufficient if this layer
is ever fronted by a multi-tenant product, where two tenants could run agents with the same
`agent_id`.

**Decide when:** a second organisation's traffic is proposed for the same deployment.
**Cost of deciding late:** high. Adding `tenant_id` to the primary key of a hash-partitioned
15-billion-row table is a full rewrite. If multi-tenancy is even plausible within two
years, it is cheaper to add the column now as a constant.

### Q2. Should payloads be encrypted at rest with per-agent keys?

[ADR-0011](decisions/0011-read-api-access-control.md) protects payloads with role scoping,
redaction, and audit. It does not encrypt them, so a database-level compromise exposes
every payload and result.

**Blocked on:** a key management story that does not exist yet, and interaction with two
existing mechanisms — the Parquet archive path in
[ADR-0009](decisions/0009-partitioning-and-retention.md) (are archives encrypted with the
same keys, and how are they rotated?) and probe-based reconciliation, which may need to
read a payload during incident recovery when a key service might itself be degraded.
**Decide when:** the first genuinely regulated write path is onboarded.

### Q3. Should the conflict window be replaced by downstream optimistic concurrency?

A time window is a heuristic for concurrency. Two writes 6 seconds apart to the same
resource are "clear" and both execute, even if the second is based on state the first
invalidated. Where a downstream supports ETags or version columns, optimistic concurrency
detects real conflicts rather than temporal proximity, and has no window to tune.

**Why not now:** it requires downstream cooperation this layer cannot assume, and PRD §4
places semantic conflict detection out of scope for v1.
**Decide when:** conflict-rate data from Phase 1 shows either many false positives (the
window is too blunt) or incidents from conflicts outside the window (the window is too
short). Both are measurable with metrics that already exist.

### Q4. What is the correct behaviour when an agent legitimately needs to retry an
`indeterminate` write?

Today the answer is: a human resolves it ([ADR-0006](decisions/0006-reconciliation-never-resumes.md)).
That is safe and does not scale. If `indeterminate` volume turns out to be non-trivial
rather than exceptional, manual resolution becomes the bottleneck, and the pressure to
auto-resume will be strong and wrong.

**Decide when:** Phase 1 produces real `indeterminate` volume data.
**Likely shape of the answer:** invest in probe coverage rather than in auto-resume, and
treat a `resource_type` without a probe as a gap to close rather than a permanent state.
Auto-resume should stay off the table; the question is how to make probes universal.

### Q5. Does the `202` polling contract hold up under real agent frameworks?

[ADR-0004](decisions/0004-concurrent-claim-resolution.md) requires agents to handle `202`
and poll. This is sound but it is a real integration burden, and agent frameworks vary in
how well they express "not done yet, do not resubmit." A framework that treats any `2xx` as
terminal, or that retries `POST` on a `202`, would defeat it.

**Decide when:** the first non-trivial agent framework integrates in Phase 1.
**Alternatives if it does not hold:** raise `claim.pending_wait_ms` so the common case
resolves within the original request, or offer a long-polling variant of
`GET /v1/writes/{key}`. Both are additive and neither changes the guarantee.

### Q6. Single-region only, or is cross-region replication in scope?

PRD §12 states throughput "per region," implying more than one, but nothing in the PRD or
in these documents addresses what happens when the same key reaches two regions. The
unique constraint is per-database, so multi-region active-active would break the guarantee
outright unless keys are partitioned by region or a global coordination layer is added.

**This is the largest unexamined assumption in the design.** Everything here assumes one
Postgres primary per region and no key ever crossing regions.
**Decide when:** before any multi-region deployment is planned, not during.
**Cost of deciding late:** very high. This is the one open question that could invalidate
architecture rather than extend it.
