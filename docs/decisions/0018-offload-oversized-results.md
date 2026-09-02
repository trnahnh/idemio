# ADR-0018: Offload oversized results to object storage, and degrade to inline rather than lose them

- **Status:** Accepted
- **Date:** 2026-09-01
- **Phase:** 1
- **Supersedes:** the "Phase 0 stores results inline regardless of size" position recorded in
  [METRICS.md](../METRICS.md)

## Context

[ADR-0009](0009-partitioning-and-retention.md) keeps `idempotency_keys` narrow because its
retention is a continuous batched `DELETE` rather than a partition drop, and a wide row makes
that sweep more expensive for the life of the table. `limits.result_inline_bytes` and the
`result_ref` column exist for that reason, and
[DEPLOYMENT_CHECKLIST.md](../DEPLOYMENT_CHECKLIST.md) has always said results above the cap
"go to object storage via `result_ref`".

Nothing wrote that column. `result_ref` was set to `NULL` on re-claim and never to anything
else, `idemio_oversized_results_total` counted results that *would* have been offloaded, and
METRICS.md said the opposite of the checklist. The column, its `result_inline_xor_ref`
constraint and its data classification were all real; the behaviour was not.

Phase 1 wired object storage for the Parquet archive path, so the dependency now exists.

## Decision

Results larger than `limits.result_inline_bytes` are written to object storage under a
`results/` prefix, keyed by the correlation id already derived from
`(agent_id, idempotency_key)`, and the reference is stored in `result_ref`. Both the replay
path and `GET /v1/writes/{key}` resolve the reference before answering.

Three rules govern the failure modes, and they are the substance of this decision.

**A failed offload stores inline, over the cap.** The write has already executed by the time
a result is stored. If object storage will not take it, the alternatives are to lose the
body, to leave the key `pending` for the reconciler, or to exceed a size limit. The cap is a
performance guard, not a correctness one — the `result_inline_xor_ref` constraint already
permits an oversized inline result and `idemio_oversized_results_total` already counts them —
so the system degrades to exactly the behaviour it had before offload existed. It is recorded
as `idemio_offload_fallbacks_total`.

**A failed fetch answers `500`, never `503` or `502`.** The key is terminal and the write
definitely ran; only its body is unreadable. `503` is defined in
[API_REFERENCE.md](../API_REFERENCE.md) as *provably not executed*, and asserting that about
a write that executed is the precise failure this system exists to prevent. `502` claims the
outcome is unknown, which is also false — the outcome is known and recorded. `500` already
means "the layer itself failed, retry with the same key", and retrying is unconditionally
safe here because a terminal key replays and cannot re-execute.

**The retention sweep deletes the objects.** `idempotency_keys` expires by batched `DELETE`,
so the sweep returns the `result_ref` of each row it removes and deletes those objects in the
same pass. The row is deleted first: an object left behind is reclaimable, whereas a row that
outlived its object would promise a result that is already gone. Because the two cannot be
removed atomically, a bucket lifecycle rule is required as a backstop for a crash between
them — without it, offloaded bodies, which are Confidential per
[ADR-0011](0011-read-api-access-control.md), would outlive the policy that governs them.

Offload is skipped entirely when no object storage is configured, so a deployment without a
bucket behaves exactly as it did before.

## Alternatives considered

**Leave it unimplemented and correct the documents.** This was the recommendation, and it was
overruled. The argument for it: offloading puts a second storage system on the *replay* path,
which previously had no dependency beyond Postgres, for a problem with no observed instances —
`idemio_oversized_results_total` exists precisely to measure whether it is real, and it reads
zero. The argument against, which won: the checklist and METRICS.md contradicted each other,
and a schema with a column, a constraint and a data classification for a feature that does not
exist is the kind of drift this project is supposed to be a counterexample to.

**Store the result in `result_ref` as a truncated inline copy plus a reference.** Rejected. It
doubles the storage and makes "which one is authoritative" a question that every reader has to
answer.

**Fail the outcome write and let the reconciler resolve it.** Rejected as the offload-failure
rule. The reconciler would meet the same unreachable object store when it tried to store what
the probe found, so a storage blip would churn keys until it escalated them to `indeterminate` —
manufacturing manual work out of a transient fault.

## Consequences

- **`cmd/idemio` gains a request-path dependency on object storage** that it did not have.
  This is the real cost, and it is bounded rather than hidden: it applies only to keys whose
  results were offloaded, and it degrades to `500` with an explicit reason rather than to a
  wrong answer. Configure the API binary with object storage credentials, or leave it
  unconfigured and accept inline storage at any size.
- A bucket lifecycle rule is now an operational requirement, not an optimisation. It is the
  only thing that reclaims an object orphaned by a crash mid-sweep.
- The `result_inline_xor_ref` constraint is load-bearing: it is what makes "inline or
  referenced, never both" a database guarantee rather than a convention.
- Three metrics are added, and the interesting one is `idemio_offload_fallbacks_total`. A
  sustained non-zero value means object storage is unhealthy *and* the row width protection
  that ADR-0009 depends on has silently stopped applying.
