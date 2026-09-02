# ADR-0007: Replace the blanket conflict rule with a compatibility matrix

- **Status:** Accepted; the fallback clause is superseded by [ADR-0014](0014-undeclared-operations-rejected-at-admission.md)
- **Date:** 2026-09-01
- **Phase:** 1

## Context

PRD §10 rule 1: "Two writes conflict if they target the same `resource_type` +
`resource_id` within the conflict window and at least one is a mutating operation."

Every request that reaches this system is a write. The second clause is therefore
vacuously true for every pair, and the rule reduces to: *any two writes to the same
resource within 5 seconds conflict.* For different-agent pairs, PRD §10 rule 3 makes the
default response rejection.

That is not a conflict detector; it is a per-resource 5-second mutex with an outage as its
failure mode. Two agents legitimately appending distinct line items to the same invoice,
or writing to different fields, would see one of them rejected with `409`. The only relief
the PRD offers is a manual allowlist of "safe to serialize" pairs, which requires
enumerating the safe cases one at a time while the unsafe default is already rejecting
production traffic.

## Decision

Every operation is declared, per `resource_type`, with an **operation class** and an
optional **scope selector**. Conflicts are determined by a matrix over classes, not by a
blanket rule.

**Classes:**

| Class | Meaning |
|---|---|
| `create` | Brings a resource into existence |
| `replace` | Overwrites the whole resource state |
| `mutate` | Modifies part of the resource state |
| `append` | Adds to an unordered or monotonically growing collection |
| `delete` | Removes the resource |

**Matrix** (`C` = conflict, `-` = compatible):

|  | create | replace | mutate | append | delete |
|---|---|---|---|---|---|
| **create** | C | C | C | C | C |
| **replace** | C | C | C | C | C |
| **mutate** | C | C | C* | - | C |
| **append** | C | C | - | - | C |
| **delete** | C | C | C | C | C |

`C*` — two `mutate` operations conflict **unless** both declare disjoint scope selectors.
A scope selector is a set of field paths the operation writes (for example
`["status"]` versus `["billing_address"]`). Disjoint scopes do not conflict. An operation
that declares no selector is treated as writing the whole resource and conflicts with
every other `mutate`.

**The default is conflict.** An operation with no declaration, or a `resource_type` with
no manifest, is treated as `replace` with no selector, which conflicts with everything.
Onboarding a write path therefore starts safe and is loosened deliberately, rather than
starting permissive.

Declarations live in a versioned configuration manifest per `resource_type`, hot-reloadable
without deploy, with changes recorded in the audit log. The manifest is the same artifact
that carries error classification ([ADR-0005](0005-downstream-outcome-taxonomy.md)) and
the probe ([ADR-0006](0006-reconciliation-never-resumes.md)).

The conflict **window** stays at a 5-second default but becomes overridable per
`resource_type` in the same manifest, which resolves the first PRD §18 open question. A
window is a heuristic for concurrency, and the right value depends entirely on how long
the downstream takes to make a write visible.

## Alternatives considered

**Keep the blanket rule and grow the allowlist.** Rejected. It inverts the safe default:
the system rejects legitimate traffic until each case is manually excepted, and the
pressure to add exceptions arrives during incidents.

**Detect conflicts by comparing payloads semantically.** Rejected, and explicitly out of
scope by PRD §4. Scope selectors are a deliberately shallow approximation: declared, not
inferred.

**Detect at the downstream layer with optimistic concurrency (ETag / version columns).**
Genuinely better where the downstream supports it, and not mutually exclusive with this.
Recorded in [OPEN_QUESTIONS.md](../OPEN_QUESTIONS.md) as a possible Phase 3 addition,
since it requires downstream cooperation this layer cannot assume.

## Consequences

- Conflict detection cannot ship before at least one manifest exists. This is why the
  work sits in Phase 1 alongside the first real conflict-detecting write path.
- A wrong or missing declaration fails toward rejection, which is an availability incident
  rather than a correctness incident. That is the correct direction, and it makes the
  rejection-rate-per-agent alert in PRD §15 a first-class launch gate.
- Scope selectors introduce a small path-matching evaluation on the request path. It is
  bounded by the number of intents in the window for one resource, which the
  `(resource_type, resource_id, emitted_at)` index keeps small.
- The manifest becomes the central integration artifact. It is worth generating a
  validating schema for it early.
