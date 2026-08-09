# Circuit breaker and bulkhead — design (#51, M3-04)

Approved 2026-08-09.

## What this system actually calls

The acceptance criteria say "circuit breaker on cross-service HTTP and on the
Celestrak fetch". Checked rather than assumed: **there is no cross-service HTTP**
in the backend. The services talk through JetStream, and `grep` finds no
`http.Client` in any Go service's `internal/`. The only outbound HTTP in the
whole system is the Celestrak fetch, from Python.

That is worth stating rather than quietly implementing three breakers with two
of them wrapping nothing. A breaker with no caller is dead code that looks like
resilience — the review question this repository asks about every check applies
here too.

So the scope is:

| Criterion | Where it lands |
| --- | --- |
| Breaker on the Celestrak fetch | `feasibility.resilience.breaker`, wired into `CelestrakClient` |
| Breaker on cross-service HTTP | **Nothing to wrap.** Recorded here, and the day a service calls another over HTTP the breaker exists to be used |
| Bulkhead: reads cannot starve the write path | tasking-api's submit path gets its own pool, separate from reads and health |
| Timeouts on every outbound call | Audit, and fix what is missing |
| Breaker state as a metric | A gauge on the existing metrics surface, not a log line |
| Test: a slow dependency trips the breaker | A server that never answers, driven through the real client |

## The breaker

Hand-rolled, roughly sixty lines, no new dependency.

*Rejected:* `pybreaker`. It buys a state machine this needs about a third of,
and costs a dependency in the service whose dependency list is already the
longest in the repository — plus its own failure semantics to learn, which is
exactly the kind of thing that gets configured wrongly once and believed
forever. The state machine here is three states and two counters; writing it
means the failure policy is visible in review rather than in someone else's
README.

States: CLOSED → OPEN after N consecutive failures → HALF_OPEN after a cooldown
→ CLOSED on one success, back to OPEN on one failure. Consecutive failures
rather than a rolling error rate: the caller is a startup fetch and a periodic
sweep, not a request stream, and a rate over a handful of calls per hour is
noise dressed as statistics.

**Open means fail fast, not fail silently.** The Celestrak fetch already has a
fallback — the frozen snapshot in `testdata` — and the breaker makes that
fallback prompt instead of ten seconds late per satellite. For a constellation
of tens, that is the difference between a slow start and a start that looks
hung.

## The bulkhead

tasking-api opens a second pool for the submit path, small and its own, so a
burst of read traffic or a slow health query cannot leave ingress without a
connection. ADR-0003's position is that ingress availability is the property
this architecture exists to protect; a shared pool makes that property depend on
whatever else happens to be querying.

## Out of scope

Retries with backoff around the breaker (the caller already has a fallback),
and a breaker on the database (the pool bounds it, and a breaker in front of a
connection pool is two queues where one will do).
