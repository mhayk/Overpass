# 0008 — Idempotency in two places: a required HTTP key at ingress, and a dedup ledger in every consumer

- **Status:** accepted
- **Date:** 2026-08-08
- **Deciders:** Mhayk Whandson

## Context and problem statement

Two different things retry in this system, for two different reasons, and
conflating them produces a design that handles neither well.

**Clients retry over HTTP.** A timeout, a load balancer hiccup, a mobile network
— the client does not know whether the request arrived. Without a way to say
"this is the same submission", a retry creates a second tasking request that
competes with the first in the planner. The customer pays twice and gets one
image, and both requests look entirely legitimate.

**The broker redelivers.** JetStream is at-least-once and says so. A consumer
that crashes after doing its work and before acking will see the message again.
Recomputing a feasibility sweep is merely expensive; republishing its
opportunities creates phantom candidates the planner will happily allocate.

The question: **what makes a repeated arrival — from a client or from the broker
— produce the same outcome as a single one?**

## Decision drivers

1. **The failures are not symmetric.** A duplicate request is a billing and
   allocation problem the customer sees. A duplicate event is a correctness
   problem the customer never sees and nobody can trace.
2. **At-least-once is a given, not a choice.** [ADR-0006](0006-transactional-outbox.md)
   makes publication at-least-once deliberately, so the consumer side must
   absorb it.
3. **A client bug must surface, not be absorbed.** Two different submissions
   sent under one key is a mistake worth reporting.
4. **The mechanism must survive concurrency**, not just sequential retries. N
   clients arriving together is the interesting case and the one a naive
   implementation fails.
5. **Nothing may depend on ordering.** Consumers process concurrently and
   messages redeliver out of band.

## Considered options

1. **One mechanism for both** — deduplicate everything on a single
   application-level id
2. **Two mechanisms, chosen per boundary** — an HTTP idempotency key at ingress,
   a processed-events ledger in consumers
3. **Rely on the broker's deduplication window** for events, and nothing at
   ingress
4. **Make every operation naturally idempotent** and need no bookkeeping at all

## Decision outcome

Chosen: **Option 2 — two mechanisms, because there are two problems.**

### At ingress: a required `Idempotency-Key`

Required, not optional. **Optional means the default behaviour is unsafe under
retry, and clients discover that in production.** A header nobody sends is a
feature nobody has.

- **Scoped per customer.** `(customer_id, idempotency_key)` is the primary key.
  A global key space lets one customer's chosen key collide with another's and
  replay someone else's response — a data leak wearing an idempotency hat.
- **The claim commits with the request.** One transaction, so there is no window
  in which a key exists and its request does not. That window would swallow the
  submission forever: every retry would look like a replay of something that
  never happened.
- **The fingerprint is a digest of the CANONICALISED body**, not of the raw
  bytes. Many HTTP clients reserialise before retrying, and a digest over bytes
  would turn a genuinely identical retry into a 409. Object keys are sorted,
  whitespace dropped, arrays left in order because array order is meaningful,
  and numeric literals compared as written so `1200` and `1.2e3` stay distinct.
- **Reuse with a different body is 409, never a silent replay.** It is a client
  bug, and swallowing it discards a request the customer believes they
  submitted. They find out when the image never arrives.
- **24-hour expiry with a purge.** The primary key is customer-supplied, and an
  unbounded row count driven by client input is a slow-motion outage.

### In consumers: a `processed_events` ledger

```sql
BEGIN;
  INSERT INTO processed_events (consumer, event_id);
  -- the actual state change
COMMIT;
-- then ACK
```

- **Keyed on `(consumer, event_id)`, not `event_id` alone.** One service can run
  several durable consumers, and a redelivery to one must not look
  already-processed to another.
- **The dedup row and the result commit together.** As separate transactions, a
  crash between them either loses the work or marks it permanently done without
  it having happened. Both silent.
- **Ack strictly after commit.** Ack-then-crash loses the message;
  commit-then-crash causes a redelivery the dedup absorbs. The failure modes are
  asymmetric, so the ordering is not a matter of taste.
- **A duplicate is acked, not ignored.** The work is already done, and leaving
  it unacked would redeliver it forever.

### What the tests actually proved, and a correction

The issue behind the ingress half framed the choice as *"unique constraint over
check-then-insert"*, with a concurrency test to prove which was implemented. So
the adapter was rewritten to check-then-insert, to watch the test fail.

**It passed.** The constraint was still on the table and caught the race
regardless. What makes the test fail is dropping the primary key:

```
16 concurrent identical submissions created 16 requests
```

**The mechanism is the database constraint, not the application code path.**
That is worth stating plainly, because the tempting summary — "we chose the
right algorithm" — is not what the evidence supports. The algorithm is nearly
irrelevant; the constraint is everything. A future refactor that keeps the
constraint cannot break this, and one that drops it cannot be saved by careful
code.

### Consequences

**Good**

- A retrying client gets one request, one charge, one image, and a
  `Idempotency-Replayed: true` header telling it which it got.
- A redelivering broker produces one state change, and the expensive work is
  skipped rather than repeated: the dedup insert goes first in the transaction
  precisely so a duplicate sweep is rejected before it runs.
- Both guarantees are properties of database constraints, which no future code
  path can bypass.
- Client bugs surface as 409 rather than as silent data loss.

**Bad**

- **Two mechanisms is two things to keep straight**, and a new consumer that
  forgets the ledger is idempotent in name only. Nothing currently enforces
  that a consumer has one.
- The `idempotency_keys` table is driven by client input and needs a purge that
  must actually run. If the timer stops, the table grows quietly.
- Canonicalisation is a correctness surface of its own. A change to it silently
  invalidates every stored fingerprint, turning in-flight retries into 409s.
- The 24-hour TTL is a guess. A client retrying at hour 25 gets a second
  request, and nothing tells them why.

**Neutral**

- JetStream's own `Nats-Msg-Id` dedup window is used as well, as a second line
  of defence. It is not relied on: it expires, and ours does not.

### Confirmation

- **Concurrency:** sixteen simultaneous identical submissions produce exactly
  one request and one outbox event. Dropping the constraint makes it sixteen.
- **Replay:** an identical retry returns the original `request_id` and the
  original state, not a fresh one — a replay reporting `RECEIVED` for a request
  already `PLANNED` would tell the customer their request had been reset.
- **Reordering:** a body with its keys in a different order still replays.
- **Conflict:** the same key with a different body is 409, and the second
  request is not stored.
- **Consumers:** the same event delivered five times produces exactly one set of
  opportunities, and a handler that fails leaves no dedup row behind.
- **The decision is wrong** if a consumer is ever added without a ledger and
  nobody notices, or if the canonicalisation has to change and there is no
  migration path for stored fingerprints. The first is a gap a CI check could
  close and currently does not.

## Pros and cons of the options

### Option 1 — One mechanism for both

- Good, because one concept is easier to explain than two.
- Bad, because the two problems have different keys, different lifetimes and
  different failure responses. A client's key is chosen by the client and lives
  for a day; an event id is chosen by us and lives forever. Forcing them into
  one table means the union of both sets of constraints and the intersection of
  neither's clarity.
- Bad, because a client bug should be a 409 and a redelivery should be an ack.
  One mechanism cannot express both.

### Option 2 — Two mechanisms (chosen)

- Good, as above.
- Bad, as above: two things to keep straight, and nothing enforcing that a new
  consumer has a ledger.

### Option 3 — Rely on the broker's dedup window

- Good, because it is free and already there.
- Bad, because it is a *window*. JetStream's default is two minutes; a consumer
  that is down for ten sees duplicates it cannot detect. Correctness that
  expires is not correctness.
- Bad, because it does nothing at all for HTTP retries, which is the half the
  customer can see.

### Option 4 — Make every operation naturally idempotent

- Good, because it is the strongest answer where it applies: an operation that
  can be applied twice safely needs no bookkeeping at all.
- Good, and partially adopted — the planner's supersession model in
  [ADR-0012](0012-retain-superseded-acquisitions.md) is exactly this.
- **Bad, because submitting a tasking request is not naturally idempotent.** It
  mints an identifier, it charges, and it enters an auction. There is no
  formulation of "create a request" that can run twice and mean once without
  something remembering that it already ran.

## More information

- Ingress: `services/tasking-api/internal/domain/idempotency.go`,
  `internal/adapter/postgres/submissions.go`
- Consumers: `services/feasibility/src/feasibility/messaging/idempotency.py`
- The pattern as the contracts state it: `contracts/nats/topology.md`
- The other half of the bargain: [ADR-0006](0006-transactional-outbox.md)
- The schema that enforces both: `db/migrations/`
