# ADR-0012: Phase 0 implementation stack

- **Status:** Accepted
- **Date:** 2026-09-01
- **Phase:** 0
- **Supersedes:** —

## Context

ADRs 0001–0011 settled what the system does. None of them says what it is built from.
Phase 0 cannot start without choosing a driver, a router, a migration mechanism, a
configuration source, and a test strategy — and three of those choices turn out to bear
directly on the at-most-once guarantee rather than on convenience.

Individually none of these warrants an ADR. Collectively they are binding on every line of
Phase 0, and two of them close hazards that are invisible in the design documents because
they live in the standard library and in the test harness rather than in the architecture.

## Decision

### Runtime and dependencies

Go 1.25.5. Four direct dependencies, each pinned:

| Dependency | Version | Why |
|---|---|---|
| `github.com/jackc/pgx/v5` | v5.10.0 | Native, with `pgxpool`. Not `database/sql` — we need `ON CONFLICT … RETURNING`, `pg_advisory_xact_lock`, real `JSONB`/`TIMESTAMPTZ`, later `LISTEN`/`COPY`. Portability is not a value we hold; the guarantee is a Postgres unique constraint and is not portable either. |
| `github.com/gowebpki/jcs` | v1.0.1 | RFC 8785 canonicalization for [ADR-0003](0003-canonical-request-hashing.md). The official RFC test vectors are committed as our own conformance test — the library is small and low-traffic, so the vectors are the check, and they survive an implementation swap. |
| `github.com/prometheus/client_golang` | v1.24.1 | `/metrics`. What DEPLOYMENT_CHECKLIST's pager wiring assumes. |

HTTP is stdlib `net/http` with Go 1.22+ method-and-path patterns. Logging is `log/slog`
with a JSON handler. No router, no ORM, no configuration library.

### Repository structure

```
cmd/idemio           API server
cmd/reconciler       reconciler
cmd/fakedownstream   fake downstream, its own process
internal/downstream  mutating client   — imported ONLY by cmd/idemio
internal/probe       read-only probe   — imported by cmd/reconciler
internal/{api,claim,store,canonical,config,reconcile,resource}
migrations/
```

The `downstream`/`probe` split exists so that the CLAUDE.md invariant "the reconciler has no
downstream write path — structurally, not by configuration" is *checkable*. A test walks the
import graph of `cmd/reconciler` and fails if `internal/downstream` is reachable from it.
A single package holding both clients would demote the invariant to convention.

### The downstream call

The client returns a typed `Outcome` and **no `error`**. The type's zero value is
`Indeterminate`, so an unhandled branch, an early return, or a future error nobody mapped
fails into the safe state automatically. An `error` return invites `if err != nil { mark
failed }`, which is precisely the bug that turns an unknown outcome into a re-claimable one
and executes the write twice.

The correlation header is **`X-Idemio-Correlation-Id`**. It must never be named
`Idempotency-Key` or `X-Idempotency-Key`: `net/http` retries a request on a reused
connection when `Request.isReplayable()` is true, and that returns true for a `POST` when
`GetBody` is set — which `http.NewRequest` does automatically — **and** one of those two
header names is present. Under the obvious naming, the standard library is licensed to
replay our write on `errServerClosedIdle` or a mid-read connection failure while idemio
counts one call. `GetBody` is additionally set to `nil` on the downstream request, and a
regression test forces the path: the fake closes idle connections mid-flight and the
assertion is that its ledger shows exactly one execution.

`connect_timeout_ms` is configured separately from `timeout_ms` on the dialer, so
"connection refused" remains provably not-executed per
[ADR-0005](0005-downstream-outcome-taxonomy.md).

### Configuration

Environment variables only, `IDEMIO_` prefix, DEPLOYMENT_CHECKLIST's dotted keys mapping
mechanically (`reconcile.stale_after` → `IDEMIO_RECONCILE_STALE_AFTER`). One struct, one
file, validation beside it. The process refuses to boot unless:

- `stale_after >= 3 × (connect_timeout_ms + timeout_ms)`. Connect time is in the budget
  because worst-case call duration is connect plus read; `3×` absorbs jitter and a sweep
  landing at the wrong moment. DEPLOYMENT_CHECKLIST required "a stated margin" and never
  stated one.
- `stale_after > 2 × reconcile.interval`, or a key can be swept as stale before one sweep
  period has passed since it was claimed.
- `claim.pending_wait_ms < downstream.timeout_ms`.
- The **live** unique constraint on `idempotency_keys` is exactly
  `(agent_id, idempotency_key)`, read from the catalog rather than from the migration file.
- `IDEMIO_AUTH_MODE=trusted_header` is set explicitly. Caller identity arrives as a
  gateway-set header per API_REFERENCE §1; making the only Phase 0 mode opt-in means running
  without real verification is always deliberate. The `403`-on-mismatch check and role
  scoping ship in full, because per-agent key isolation depends on them.

### Schema and migrations

Numbered `.sql` under `migrations/`, embedded with `go:embed`, applied by an in-repo runner
with a `schema_migrations` ledger, one transaction per file. The same path serves the binary
and the tests, so the schema under test cannot drift from the schema in production. The
runner is deliberately featureless: no down-migrations, no dirty-state recovery, fail loudly
and stop.

A `DO $$` loop creates the 64 hash partitions. RANGE-partitioned tables are pre-created 12
weeks out, with **no DEFAULT partition** — a default silently absorbs rows for unprovisioned
ranges and then blocks attaching a real partition for that range, converting an outage
DEPLOYMENT_CHECKLIST wants to page on into quiet corruption. Startup exposes remaining
headroom as a metric and refuses to boot below two weeks.

### The fake downstream

The fake **is** the Phase 0 integration. Phase 0's job is to prove a correctness property
under adversarial conditions, which requires controllability, not realism: no real API can
be scripted to hang mid-response or fail on the fifth call of a test run.

Its execution ledger is an append-only JSONL file in its own process — never a table in
idemio's Postgres. Shared infrastructure means a bug in that infrastructure can make both
sides agree while both are wrong. Appending to the ledger **is** the execution, flushed
before the fake responds or hangs, so a hung call still leaves a record idemio never
observed. The `GET` probe is the **only** way the test suite may assert how many times a
write actually executed.

Behaviour is installed by `POST /control` as an ordered script keyed by `resource_id`; the
fake pops the next behaviour per call. Encoding the mode in the payload is the obvious
alternative and it silently kills the tests that matter most — proving a `failed` key is
re-claimable needs attempt 1 to be connection-refused and attempt 2 to succeed with a
byte-identical body, and the payload is hashed.

| Mode | What it proves |
|---|---|
| `succeed` | Replay returns the stored result instead of re-executing |
| `business-failure` | A valid downstream rejection is terminal and replays, is not retried |
| `connection-refused` | The `503` path, and that the intent was durable before the call |
| `hang-past-timeout` | Giving up waiting never executes twice; the reconciler resolves the key |
| `GET` probe | The oracle for every exit criterion |

### Testing

Postgres 18 runs under `docker compose`. Tests read `IDEMIO_TEST_DATABASE_URL` and **fail**
when it is unset; skipping is how a suite silently stops testing the only thing that matters.
Migrations run once into `idemio_template`; each test package gets `CREATE DATABASE …
TEMPLATE` — a file-level copy, so 64 partitions cost nothing per package — and truncates
between tests.

Transaction-rollback-per-test is impossible here: the claim transaction commits before the
downstream call, so a test wrapped in a rollback tests a system that does not exist.

The kill test builds and spawns `cmd/idemio`, sends a write with the fake hanging, waits
until the fake's ledger shows the call arrived, then calls `cmd.Process.Kill()` — which on
Windows is `TerminateProcess`, as uncatchable as `SIGKILL`, so the test is faithful on both
platforms with no OS-specific code. It sits behind a build tag. The reconciler is driven by
importing `internal/reconcile` and running one deterministic sweep with `stale_after` in
milliseconds; a flaky correctness test is worse than none, because it teaches you to re-run
until green.

CI is one GitHub Actions workflow: `postgres:18` service, `go vet`, `go test ./...`, kill
test enabled. CI is not the kind of product capability phase discipline exists to block —
it is the mechanism by which the exit criteria stay true, and it gives the correctness suite
a Linux run.

### Scope boundaries held for Phase 0

- Per-`resource_type` knowledge (operation → `operation_class`, error classification, probe
  endpoint) lives in a Go registry with one entry. The Phase 1 manifest
  ([ADR-0007](0007-operation-compatibility-matrix.md)) replaces it behind the same
  interface. An operation absent from the registry is `422`.
- No result size cap. Results store inline regardless of size, `result_ref` stays `NULL`,
  and a metric counts results over `limits.result_inline_bytes` so Phase 1 sets the cap from
  data. Truncation was rejected outright: it breaks replay fidelity, which is an exit
  criterion.
- `GET /v1/writes/{key}` is agent-scoped. The owning agent gets the full `result` — that is
  replay. A key belonging to another agent returns **`404`, not `403`**, because `403` would
  confirm the key exists in another agent's namespace. This does not contradict
  API_REFERENCE, whose `403` governs an `agent_id` mismatch in a `POST` body. Operator,
  investigator, and `payload_access_audit` land in Phase 1 with the intents API.
- Bounded drain on `SIGTERM`, pulled forward from its Phase 2 gate: without it every restart
  manufactures `pending` keys and the signal that should mean "something went wrong" becomes
  routine noise.
- Payloads and results are Confidential per [DATA_MODEL.md](../DATA_MODEL.md) and are never
  logged. `agent_id`, key, resource, status, and request hash only.

## Alternatives considered

**`database/sql` with the pgx shim** — buys portability to a database we will never use.

**`goose` or `golang-migrate`** — a dependency for one migration file. `goose` remains the
named fallback if the in-repo runner ever grows features; the moment it needs down-migrations
or dirty-state recovery, it should be replaced rather than extended.

**A router (`chi`) and a framework (`echo`, `gin`)** — two routes in Phase 0, six by Phase 1.
Frameworks own the request lifecycle, and this service's value is precise control between
receiving a request and committing a transaction.

**A config file or viper** — two sources of truth for twelve scalars, with precedence bugs
that only appear in production. A file becomes right in Phase 1 for the operation manifest,
which is a different problem: it needs review as code.

**`testcontainers-go`** — hides the database behind a Go API when DEPLOYMENT_CHECKLIST
requires opening a shell and checking the live unique constraint by hand.

**`DisableKeepAlives` on the downstream client** — closes the `net/http` retry hazard by
brute force, at a handshake per write against a 15ms p50 budget on the hottest path.

**Integrating a real downstream, or building a generic manifest-driven forwarder** — the
first cannot be scripted to misbehave and so leaves the fake in place anyway; the second is
Phase 3's shape. Since the middleware talks to the downstream through one interface, swapping
the fake later is contained, not a rewrite.

## Consequences

**Makes easy.** The invariants become mechanically checkable rather than aspirational: the
import graph proves the reconciler cannot write downstream, the zero value proves an
unclassified outcome is `indeterminate`, and the catalog check proves the live constraint
was never widened. Dependencies are few enough to audit in an afternoon.

**Makes hard.** We own a migration runner and a canonicalization conformance suite. The
`X-Idemio-Correlation-Id` naming is load-bearing and looks arbitrary — anyone renaming it to
`Idempotency-Key` for tidiness reintroduces a double-execution path, which is why the
regression test exists as the durable layer rather than the naming convention.

**Obliges us to build.** A featureless migration runner; a fake with a durable ledger, a
scripted control plane, and a probe; a template-database test harness; an import-graph test;
RFC 8785 vector tests; a `net/http` retry regression test; startup validation covering five
relations; and one CI workflow.
