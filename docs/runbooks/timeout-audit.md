# Outbound timeout audit (#51)

Every call that leaves a process, and what bounds it. Written down because
"timeouts on every outbound call, no unbounded waits anywhere" is only checkable
against a list, and a list nobody wrote is an assertion.

Audited 2026-08-10, against the code rather than against memory.

| Call | Bound | Where |
| --- | --- | --- |
| Celestrak GP query | 10s per request, plus a breaker after 3 consecutive failures | `tle/celestrak.py` |
| NATS connect (Go ×3) | 2s per server, the client default | `nats.Connect` |
| NATS JetStream publish | 5s, explicit | `outbox/relay.go` in tasking-api and planner |
| NATS fetch (Go ×2) | `FETCH_WAIT`, default 5s, via a derived context | `natsmsg/source.go` |
| Postgres — tasking-api submit | 5s, `SUBMIT_TIMEOUT` | `httpapi/submit.go` |
| Postgres — tasking-api readiness | 2s, `READINESS_TIMEOUT` | `app.HealthService` |
| Postgres — plan-gateway reads | 5s, `READ_TIMEOUT` | `httpapi.Deadline` — **added by this audit** |
| Postgres — planner round | bounded by the advisory lock's own transaction | `postgres/rounds.go` |
| OTLP export (traces, metrics) | 5s per attempt | `lib/go/telemetry` |
| HTTP server read headers | 10s | every `http.Server` |

## What the audit actually found

**plan-gateway had no read timeout at all.** Every handler passed `r.Context()`
straight to the query layer, and an inbound request context carries no deadline
of its own. A query that stopped making progress held its request open
indefinitely: the service looks alive, answers nothing, and the request count
simply stops — the failure mode hardest to attribute, because every dashboard
stays green while nothing is being served.

This is the same argument `submitTimeout` already made about tasking-api's write
path, and it applies at least as strongly here. plan-gateway is the service
whose queries touch PostGIS geometry and span whole buckets, so it is the one
where a query can genuinely take minutes.

Fixed as middleware rather than eight handler edits. Per-handler would work
today and be forgotten by the ninth endpoint, and a bound covering most requests
is not a bound.

Watched it fail with the middleware removed:

```
--- FAIL: TestASlowReadIsRefusedRatherThanHeldOpen (5.00s)
    deadline_test.go:56: the request never returned; the read model is holding it open
```

**`nats.Connect` is bounded by default, at 2s.** Verified by reading
`nats.go`'s `DefaultTimeout` in the pinned version rather than assuming it —
an unoptioned call looks unbounded and is not, and adding an explicit option
would have implied a fix where none was needed.

## What is deliberately not bounded

**The consumer fetch loops.** They block on `stop` between batches and are
cancelled by context on shutdown. A "timeout" there would mean a consumer that
stops consuming while healthy.

**The planner's advisory lock wait.** It is `pg_try_advisory_lock` — it does not
wait at all. A round that cannot take the lock returns immediately, which is why
there is no timeout to write down.

## The breaker's metric, and the caller it does not have

`overpass_breaker_state` is published per dependency: 0 closed, 1 open, 2
half-open. #51 asks for state as a metric rather than a log line because of one
specific question — "why did latency drop while errors rose?" An open breaker is
the answer, and it is only answerable if the state can be overlaid on the
latency graph it explains.

**Nothing in the running services constructs a `CelestrakClient` today.**
ADR-0011 makes the seeder read the frozen snapshot in `testdata/` rather than
the network, deliberately, so the only outbound HTTP this system has is
currently unreachable from any running process.

That is stated rather than worked around. The M3-04 design already made this
argument one level up — "a breaker with no caller is dead code that looks like
resilience" — and the same applies to its metric. A gauge wired from a
composition root nobody runs would read a constant zero and imply a healthy
dependency that is simply never called.

So the gauge is registered at client CONSTRUCTION. The series appears the
moment live TLE refresh gives the client a caller, and is honestly absent until
then.
