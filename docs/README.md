# Documentation index

This folder is the living specification for the idempotent transaction layer.

## How these documents relate

The original PRD is **frozen at v1.0** and kept outside version control. It records the
original intent and is never edited. Everything in this folder derives from it and
supersedes it where they disagree. This folder is the answer to what the system does
today.

The PRD contained several internal contradictions and unresolved gaps. Those were not
edited away — each was settled as a dated [Architecture Decision Record](decisions/),
so the reasoning is reviewable and the resolution has a date and a set of consequences
attached to it.

## Documents

| Document | Owns | Read it when |
|---|---|---|
| [SYSTEM_DESIGN.md](SYSTEM_DESIGN.md) | Architecture, write flow, failure modes, HA, security, observability | Onboarding, or changing how a request is processed |
| [DATA_MODEL.md](DATA_MODEL.md) | ERD, DDL, partitioning, retention | Writing a migration or a query |
| [API_REFERENCE.md](API_REFERENCE.md) | The external contract: endpoints, status codes, hashing, error semantics | Integrating an agent, or changing a response |
| [DEPLOYMENT_CHECKLIST.md](DEPLOYMENT_CHECKLIST.md) | Per-phase go-live gates, configuration, SLOs | Shipping to a new environment or write path |
| [ROADMAP.md](ROADMAP.md) | Phases, exit criteria, sequencing | Planning work, or asking "is this in scope yet" |
| [OPEN_QUESTIONS.md](OPEN_QUESTIONS.md) | Genuinely unresolved items | Before proposing a change that feels already-argued |
| [decisions/](decisions/) | Dated, immutable architecture decisions | You disagree with something above |

**One fact, one owner.** DDL lives only in `DATA_MODEL.md`. Status codes live only in
`API_REFERENCE.md`. Other documents link rather than restate. If you find the same fact
asserted in two places, that is a bug — delete one and link to the other.

## Reading order by role

- **Implementing Phase 0** → SYSTEM_DESIGN §Write flow, DATA_MODEL, ADRs 0001–0006
- **Integrating an agent** → API_REFERENCE, then ADR-0003 (hashing) and ADR-0005 (retry semantics)
- **Operating the tier** → DEPLOYMENT_CHECKLIST, SYSTEM_DESIGN §Failure modes, §Observability
- **Reviewing the design** → PRD for intent, then decisions/ in order
