export const repo = "https://github.com/trnahnh/idemio";

export const guarantee =
  "For a given (agent_id, idempotency_key), the downstream write executes at most once, and every attempt is durably recorded before it is executed.";

export const headline = {
  eyebrow: "Idempotent transaction layer · Go · PostgreSQL 18",
  title: ["Send it twice.", "Charge them once."],
  sub: "AI agents retry after timeouts they cannot interpret, and several of them write to the same resource with no coordination. idemio sits in front of the system of record and makes a duplicate write physically impossible — enforced by a unique constraint, not by application logic.",
};

export const stats = [
  { value: "at most once", label: "per (agent, key)", note: "a Postgres unique constraint, not a code path" },
  { value: "166", label: "tests in the default suite", note: "plus kill, latency, load and alert drills" },
  { value: "~600/s", label: "within the latency budget", note: "one machine, p50 13.2ms · p99 33.3ms" },
  { value: "18", label: "architecture decision records", note: "four of them supersede earlier ones" },
];

export const latency = [
  { rate: 150, p50: 9.8, p95: 11.2, p99: 13.8 },
  { rate: 400, p50: 11.3, p95: 13.4, p99: 16.4 },
  { rate: 600, p50: 13.2, p95: 18.3, p99: 33.3 },
  { rate: 800, p50: 22.4, p95: 86.7, p99: 109.3 },
];

export const budget = { p50: 15, p99: 60 };

export const flow = [
  {
    step: "01",
    title: "Verify and hash",
    body: "The caller's identity is verified, then the request is canonicalised to RFC 8785 and hashed. A retry that re-serialises its JSON differently hashes the same, so it replays instead of being rejected.",
  },
  {
    step: "02",
    title: "Lock the resource",
    body: "A transaction-scoped advisory lock on the resource, taken as the transaction's first statement. Taking it any later lets two conflicting writers each see the other and both lose.",
    accent: true,
  },
  {
    step: "03",
    title: "Claim the key",
    body: "INSERT … ON CONFLICT DO NOTHING against a unique constraint on (agent_id, idempotency_key). Zero rows back means someone else owns this write; the loser replays rather than blocking.",
    accent: true,
  },
  {
    step: "04",
    title: "Record the intent, check for conflicts",
    body: "The intent row and the claim commit together, and the conflict check reads the window under the same lock. Intents for writes that provably did not happen are voided so one rejection cannot cascade.",
  },
  {
    step: "05",
    title: "Commit — then call downstream",
    body: "The transaction commits before the downstream call and is never held across it. A crash here leaves a pending key, which the reconciler resolves by probe. It never re-executes.",
    boundary: true,
  },
  {
    step: "06",
    title: "Classify, store, replay forever",
    body: "Definitive, provably-not-executed, or indeterminate. Only the middle one is re-claimable. The stored result replays byte for byte and never re-reads the downstream.",
  },
];

export const decisions = [
  {
    id: "ADR-0014",
    claimed: "An undeclared operation is safe to admit — treat it as a full replace so it conflicts with everything.",
    found:
      "It also has no error classification, so every outcome falls to indeterminate — the one result the system treats as an incident. A missing config entry would have manufactured unresolvable writes.",
    now: "Rejected at admission, before the claim.",
  },
  {
    id: "ADR-0015",
    claimed: "Serialize the conflict check with an advisory lock; the loser gets a 202 and polls.",
    found:
      "Nothing said where the lock is taken. Locked after the intent insert, two conflicting writers each observe the other and both are rejected — contention becomes an outage. And a lock timeout writes nothing, so a 202 would name a key that does not exist.",
    now: "Lock first. Timeout answers 503.",
  },
  {
    id: "ADR-0015",
    claimed: "Same-agent conflicts serialize rather than being rejected.",
    found:
      "The lock is held for two statements while the downstream call happens outside it. Two same-agent writes would have landed simultaneously, with a database row asserting they had been serialized.",
    now: "The second waits for the first to finish.",
  },
  {
    id: "ADR-0016",
    claimed: "pg_partman manages the range partitions.",
    found:
      "It needs an extension, so CI would test a different database from the one that runs — making partition maintenance the least-tested subsystem with the largest blast radius. A missing partition is a hard write outage.",
    now: "Maintained in application code, on stock Postgres.",
  },
];

export const stack = [
  { name: "Go", role: "Four binaries, stdlib net/http", why: "No framework on the correctness path." },
  { name: "PostgreSQL 18", role: "Source of truth and coordination", why: "The unique constraint is the guarantee." },
  { name: "pgx/v5", role: "Driver and pool", why: "Native protocol, no ORM between us and the claim." },
  { name: "PgBouncer", role: "Transaction-mode pooling", why: "Every lock on the request path is transaction-scoped." },
  { name: "Redpanda", role: "Outbox destination", why: "Off the write path; an outage costs analytics, not writes." },
  { name: "MinIO / S3", role: "Parquet archives, oversized results", why: "Retention that is a metadata drop, not a mass delete." },
  { name: "Prometheus", role: "Metrics and alert rules", why: "Rules are code, and tested against what is exported." },
  { name: "Grafana", role: "Latency budget dashboard", why: "Decomposed so a regression can be attributed." },
];

export const principles = [
  {
    title: "The oracle is never our own logs",
    body: "Every correctness claim is verified against a separate downstream process that keeps its own append-only execution ledger, fsynced before it answers. A test proving idemio believes it executed once has not tested the guarantee.",
  },
  {
    title: "Unknown is not failed",
    body: "Any outcome that is not provably never-executed is classified indeterminate, and indeterminate is terminal. The system refuses to guess, because a wrong guess is the double write it exists to prevent.",
  },
  {
    title: "Documents that cannot drift",
    body: "The metric names, the alert rules and the exported registry are checked against each other on every run. Renaming a metric without touching the docs fails the build — an alert on a metric that does not exist looks exactly like nothing being wrong.",
  },
  {
    title: "Break it to trust it",
    body: "Every conformance check was verified by deliberately breaking what it guards. Two of them passed while proving nothing, and were only caught that way.",
  },
];
