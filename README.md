# idemio

An idempotent transaction layer for agent-driven writes.

AI agents retry writes after timeouts and ambiguous responses, and multiple agents write
concurrently to the same resources with no coordination. `idemio` sits between agents and
the system of record and guarantees that a given logical write executes **at most once**,
records every attempt for audit, and blocks conflicting writes before they reach the
database.

```http
POST /v1/writes
Idempotency-Key: 7c9e6679-7425-40de-944b-e07fc1f90ae7

{ "agent_id": "agent-checkout-flow", "resource_type": "invoice",
  "resource_id": "inv_8842", "operation": "create_charge",
  "payload": { "amount_cents": 4200, "currency": "usd" } }
```

Retry that request with the same key and it replays the stored result. It does not charge
twice.

## Status

**Design phase. No implementation yet.** This repository currently contains specification
documents only. Phase 0 is not started — see [ROADMAP.md](docs/ROADMAP.md).

## How it works

The guarantee rests on one mechanism: a **unique constraint on
`(agent_id, idempotency_key)` in Postgres, claimed in a transaction that commits before
any downstream call is made.** Everything else is scaffolding around that sentence.

Notably, the guarantee does **not** depend on consensus. There is no Raft group; Postgres
is the coordination point, and the middleware tier stays stateless.

## Documentation

Start at [`docs/`](docs/README.md).

| Document | What it answers |
|---|---|
| [SYSTEM_DESIGN](docs/SYSTEM_DESIGN.md) | How a write is processed, what breaks, and how it fails |
| [API_REFERENCE](docs/API_REFERENCE.md) | The contract agents integrate against |
| [DATA_MODEL](docs/DATA_MODEL.md) | Schema, partitioning, retention |
| [ROADMAP](docs/ROADMAP.md) | Phases and their exit criteria |
| [DEPLOYMENT_CHECKLIST](docs/DEPLOYMENT_CHECKLIST.md) | Configuration and go-live gates |
| [OPEN_QUESTIONS](docs/OPEN_QUESTIONS.md) | What is still undecided, and what would decide it |
| [decisions/](docs/decisions/) | Why the design is the way it is |

The original PRD is frozen and kept outside version control; `docs/` supersedes it. Review
found several contradictions in it — the intent log was specified as both a Kafka topic and
a Postgres table, reconciliation would have re-executed writes of unknown outcome, and the
schema could not satisfy its own retention target. Each was resolved as a dated ADR rather
than edited away, so the reasoning stays reviewable in
[decisions/](docs/decisions/).

## License

See [LICENSE](LICENSE).
