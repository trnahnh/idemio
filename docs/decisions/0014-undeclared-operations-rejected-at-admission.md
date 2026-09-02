# ADR-0014: Reject undeclared operations at admission rather than treating them as `replace`

- **Status:** Accepted
- **Date:** 2026-09-01
- **Phase:** 1
- **Supersedes:** the fallback clause of [ADR-0007](0007-operation-compatibility-matrix.md)

## Context

[ADR-0007](0007-operation-compatibility-matrix.md) states that an operation with no
declaration, or a `resource_type` with no manifest, "is treated as `replace` with no
selector, which conflicts with every other operation." The stated goal is that onboarding
a write path starts safe and is loosened deliberately.

That reasoning is sound for conflict semantics considered alone. It is wrong once the rest
of the manifest is taken into account, because the manifest carries three things and not
one. An undeclared `resource_type` has no operation class, and it also has no error
classification ([ADR-0005](0005-downstream-outcome-taxonomy.md)) and no probe
([ADR-0006](0006-reconciliation-never-resumes.md)).

The consequence is that the ADR-0007 fallback does not merely make a write conflict-prone.
It admits a write whose downstream response cannot be classified. `internal/downstream`
classifies an unclassified status as `indeterminate` by construction — the zero value of
`Disposition` — so every such write terminates as `indeterminate` regardless of what the
downstream actually did. `indeterminate` is terminal, is not re-claimable, and requires a
human ([ADR-0006](0006-reconciliation-never-resumes.md)).

The fallback therefore manufactures the one outcome the entire system treats as an
incident, in response to a missing configuration entry. `resource.Validate()` already
refuses to boot a process whose registry omits an error classification, for exactly this
reason. Admitting at request time what we refuse to admit at boot time is incoherent.

## Decision

A write whose `resource_type` has no manifest, or whose `operation` is not declared in
that manifest, is rejected before the claim transaction begins. It never reaches
`idempotency_keys`, never produces an intent, and never reaches the downstream.

The response is `422` with reason `unknown_operation`. The claim in
[API_REFERENCE.md](../API_REFERENCE.md) that an unregistered `resource_type` is "treated
as `replace` with no scope" is removed.

"Fail closed" is retained as the governing principle; this ADR relocates where it happens.
Failing closed at admission produces an immediate, specific, actionable error naming the
missing declaration. Failing closed at conflict detection produces a write that executes
against a downstream whose answer cannot be interpreted.

## Alternatives considered

**Keep the ADR-0007 fallback.** Rejected on the grounds above: it converts a configuration
omission into unresolvable write outcomes, which is a strictly worse failure than a
rejected request.

**Admit the write but force `failed` rather than `indeterminate` on any unclassified
status.** Rejected, and it is the more dangerous of the two. `failed` means *provably* not
executed and is the only re-claimable status. Asserting that about a downstream whose
response we do not understand would let the system re-execute a write that already landed.

**Use `400` rather than `422`.** Genuinely arguable. A missing manifest entry is an
operator configuration gap, not the client bug that `422` otherwise denotes in
[API_REFERENCE.md](../API_REFERENCE.md). `422` is retained because it is what the code
already returns and what any existing integrator already handles, and because changing a
status code is a breaking contract change for a case that must not occur in production.
The distinction is carried by the `reason` field instead.

## Consequences

- Onboarding a `resource_type` is strictly gated on a manifest entry. There is no partial
  mode in which a write path works but is not declared.
- The manifest becomes load-bearing for availability, not only for conflict semantics. A
  manifest that fails to load is an outage for the paths it declares, which is why
  [ADR-0013](0013-phase-1-implementation-stack.md) keeps the last known-good manifest in
  memory rather than failing open or exiting.
- ADR-0007's compatibility matrix, classes, scope selectors and per-`resource_type` window
  are unaffected. Only the fallback clause is superseded.
