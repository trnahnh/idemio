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

**Phase 0 and Phase 1 in progress.** Every claim below is demonstrated against the fake
downstream's own execution ledger, never against idemio's logs or database. What remains in
both phases needs a running deployment, not more code. See [ROADMAP.md](docs/ROADMAP.md).

Phase 0 — the core guarantee:

| Exit criterion | State |
|---|---|
| Concurrent duplicate keys execute once downstream | demonstrated |
| Kill mid-call never re-executes; the reconciler resolves by probe | demonstrated |
| A re-serialized retry replays rather than returning `422` | demonstrated |
| A business failure replays identically | demonstrated |
| p50 under 15ms and p99 under 60ms at real volume | floor measured (p50 8.4ms, p99 10.6ms); real volume outstanding |
| `indeterminate` alerting live and fired in a drill | rules in `deploy/alerts.yml`; pager and drill outstanding |

Phase 1 — the intent log and conflict detection:

| Exit criterion | State |
|---|---|
| Incompatible writes: one rejected with `409`, a conflict recorded | demonstrated |
| Compatible writes both succeed — the matrix is not a per-resource mutex | demonstrated |
| Same-agent conflicts serialize and complete | demonstrated against ledger timestamps |
| Manifest reload without deploy, and recorded | demonstrated |
| Relay lag under a minute; broker outage with zero write-path impact | outage half demonstrated; steady-state lag outstanding |
| Archive restore drill | demonstrated end to end against object storage |
| Payload redaction and audit verified | demonstrated |

**Conflict detection ships off.** Each manifest declares `enforce`; until it is set, the
check runs in full and records what it *would* have rejected without rejecting anything. A
wrong manifest surfaces as mass rejection, so onboarding means watching real traffic first
and then flipping one field.

## Running it

Requires Go 1.25+ and Docker.

```sh
docker compose up -d

export IDEMIO_TEST_DATABASE_URL=postgres://idemio:idemio@localhost:5433/idemio
export IDEMIO_TEST_POOLED_ADDR=localhost:6433
export IDEMIO_TEST_KAFKA_BROKERS=localhost:19092
export IDEMIO_TEST_ARCHIVE_ENDPOINT=localhost:9000

go test ./...
go test -tags killtest ./internal/reconcile/ -run TestKillMidCall
go test -tags latency ./internal/api/
```

Tests **fail** rather than skip when a dependency is absent: a green run has to mean the
guarantee was actually exercised. That applies to the broker and object store too — the
outage and restore criteria are not provable against a stub.

Four binaries: `cmd/idemio` (the API), `cmd/reconciler` (resolves stale claims, maintains
partitions, sweeps retention, and links no code that can write downstream), `cmd/relay`
(publishes the outbox to Kafka, off the write path by construction), and
`cmd/fakedownstream` (a controllable downstream that keeps an independent execution
ledger — the oracle every correctness test asserts against).

Configuration is environment-only and documented in
[DEPLOYMENT_CHECKLIST](docs/DEPLOYMENT_CHECKLIST.md); the process refuses to boot on a
configuration that would turn healthy writes into unresolvable ones. Conflict semantics and
error classification live in `manifests/`, one JSON file per `resource_type`, reloaded
without a deploy.

## How it works

The guarantee rests on one mechanism: a **unique constraint on
`(agent_id, idempotency_key)` in Postgres, claimed in a transaction that commits before
any downstream call is made.** Everything else is scaffolding around that sentence.

Notably, the guarantee does **not** depend on consensus. There is no Raft group; Postgres
is the coordination point, and the middleware tier stays stateless.

Conflict detection rests on a second mechanism: a per-resource advisory lock taken as the
claim transaction's **first** statement. Taking it any later lets two conflicting writers
each observe the other and both lose, which would make contention an outage. Because the
lock is transaction-scoped, it releases at commit and is never held across the downstream
call.

## Documentation

Start at [`docs/`](docs/README.md).

| Document | What it answers |
|---|---|
| [SYSTEM_DESIGN](docs/SYSTEM_DESIGN.md) | How a write is processed, what breaks, and how it fails |
| [API_REFERENCE](docs/API_REFERENCE.md) | The contract agents integrate against |
| [DATA_MODEL](docs/DATA_MODEL.md) | Schema, partitioning, retention |
| [ROADMAP](docs/ROADMAP.md) | Phases and their exit criteria |
| [DEPLOYMENT_CHECKLIST](docs/DEPLOYMENT_CHECKLIST.md) | Configuration and go-live gates |
| [METRICS](docs/METRICS.md) | Every metric and the alert it carries |
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
