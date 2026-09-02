# ADR-0017: Require an explicit time range on read endpoints, and cap its span

- **Status:** Accepted
- **Date:** 2026-09-01
- **Phase:** 1
- **Supersedes:** the default-window clause of [ADR-0011](0011-read-api-access-control.md)

## Context

[ADR-0011](0011-read-api-access-control.md) states that the list endpoints "require a
bounded range **and** apply a default window when none is given." Those are two different
rules. A parameter that is required cannot also have a default; whichever is implemented,
the other is false. [API_REFERENCE.md](../API_REFERENCE.md) resolves it one way, marking
`since` as required, but the ADR is the document that gets read when the endpoint is built.

The underlying requirement is not in dispute. Both endpoints read range-partitioned tables,
and an unbounded query scans every retained partition — 90 days of `write_intents` at
target volume. ADR-0009 makes the bound a performance requirement and ADR-0011 makes it a
blast-radius limit. Neither says what happens when the caller omits it.

## Decision

`since` is **required**. A request without it is `400`, not a request over a default
window.

The range is additionally capped: `until - since` may not exceed `IDEMIO_READ_MAX_SPAN`,
default 31 days. A request exceeding the cap is `400` naming the limit. `until` continues
to default to `now()`, which is a default on a bound the caller has already established
rather than a substitute for one they never gave.

A silently applied default window is worse here than an error. The caller believes they
searched a period they did not search, and on an endpoint whose stated purpose is incident
response, a quietly truncated result set reads as evidence of absence. An operator asking
"did this agent write to this resource" and receiving an empty list has been actively
misled. An explicit `400` costs one round trip and cannot mislead anyone.

## Alternatives considered

**Apply a 24-hour default, per ADR-0011's second clause.** The endpoint is then usable with
no parameters and partitions still prune. Rejected for the reason above; it optimises for
convenience on an endpoint used a few times a month, at the cost of an answer that can be
wrong without appearing wrong.

**Require `since` but impose no maximum span.** Simpler, and an investigator chasing a
slow-burn issue is never blocked by a cap. Rejected because a single request could then
scan every retained partition, which is precisely what the bound exists to prevent — the
requirement would be satisfied in form and defeated in substance. The cap is configuration,
so a genuine need for a wider window is a deliberate change with a name attached, not an
accident.

## Consequences

- [API_REFERENCE.md](../API_REFERENCE.md) is now consistent with the ADR that governs it,
  and gains the maximum-span parameter and its `400`.
- Paging a wide range is the caller's job, and keyset cursors make it cheap: an
  investigation spanning more than the cap is a sequence of bounded requests, each of which
  prunes.
- The cap applies to both list endpoints even though `conflicts` is far smaller than
  `write_intents`. One rule that holds everywhere is easier to rely on than a rule with an
  exception, and the `conflicts` retention window is longer, not shorter.
