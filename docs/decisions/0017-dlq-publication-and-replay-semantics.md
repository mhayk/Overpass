# 0017 — Dead letters are published by the consumer, before the Term, or not at all

- **Status:** accepted
- **Date:** 2026-08-09
- **Deciders:** Mhayk Whandson

## Context and problem statement

The #48 consume hardening made every terminal failure a *decision*: poison
terminates on the first delivery, exhausted retries terminate on the last, and
each Term carries a log line and a metric instead of a silently lapsed
`max_deliver`. What it did not do is keep the message. A Term is still a drop —
chosen, logged, and unrecoverable.

`contracts/nats/topology.md` has specified the destination since M0: three DLQ
streams under a `dlq.` subject prefix, a documented header set, `traceparent`
preserved. The streams exist in `deploy/nats/init.sh` with 720h retention and
have never received a message. M3-02 (#49) is the gap between the contract and
the running system: who publishes the dead letter, when, and what happens when
that publish itself fails.

## Decision drivers

- **A dead letter must not be losable.** The entire point is that terminal
  failure becomes an operational task instead of a data loss. Any ordering
  that can Term without a landed dead letter re-creates the loss with extra
  steps.
- **One implementation.** Four consumers re-implementing publication is how
  one of them ends up wrong — the argument that extracted `lib/go/consume`
  in #168 applies unchanged.
- **The contract is already written.** topology.md says the consumer publishes
  and then acks. An implementation that contradicts the published contract is
  a contract change wearing a disguise.
- **Replay must be boring.** Consumers are idempotent (the ledger arbitrates
  on event id), which is the standing payoff this repo paid the idempotency
  tax for: replay must need no coordination, no drain, no fear.

## Considered options

1. **Consumer-side publication in the shared libraries** — on Terminate,
   publish to `dlq.<subject>` with the contract headers, then Term; if the
   publish fails, Nak instead.
2. **Broker-side shovel** — a small service subscribed to JetStream advisories
   (`MAX_DELIVERIES`, `MSG_TERMINATED`) copies the referenced messages into
   the DLQ streams.
3. **Per-service implementations** — each consumer hand-rolls publication.

## Decision

Option 1. `lib/go/consume` gains `Deadletter`, mirrored in the feasibility
messaging package, and every Terminate site calls it. The ordering invariant:

> **Publish, then Term. If the publish fails, Nak.**

A Nak retries the whole delivery — handling and dead-lettering — which is
correct because both are idempotent. The dead letter carries the original
event id as `Nats-Msg-Id`, so the crash window between publish and Term
produces a duplicate the DLQ stream's 2-minute dedup window absorbs; beyond
that window duplicates are possible and tolerated, because everything
downstream of the DLQ (inspection, replay, the consumers replay feeds) is
duplicate-safe by construction.

Option 2 is rejected on three counts: the `MAX_DELIVERIES` advisory fires
only when the budget lapses — precisely the silent path #48 exists to avoid,
and Term-on-first-delivery for poison would need the separate terminated
advisory with different payload semantics; the shovel must fetch the original
message by sequence before retention or interest removes it, a race the
consumer-side publish does not have; and it contradicts the contract as
written. Option 3 is rejected for the #168 reason.

## Amendment to the contract

`Overpass-Dlq-First-Failed-At` becomes `Overpass-Dlq-Failed-At` — the RFC 3339
time of the terminal decision. Consumers are stateless across deliveries;
nobody knows when the *first* failure happened, and a header that promises it
would be populated with a lie. The header set has never been implemented, so
the amendment precedes the first implementation and versions nothing.

## Replay semantics

Replay republishes the stored payload to `Overpass-Dlq-Original-Subject`,
preserving the original `Nats-Msg-Id`, then deletes the DLQ entry. Preserving
the id means the consumer ledgers arbitrate: a message that was half-processed
before terminating is absorbed as a duplicate, a message that never committed
is processed as new. Deleting after republish keeps DLQ depth meaning
"outstanding operational work", which is what the alert fires on. The replay
tool is deliberately a shell script over `nats-box` — the same image CI
already trusts — not a service.

## Consequences

- Terminal failure becomes: alert (DLQ depth > 0, via prometheus-nats-exporter,
  the one new dependency) → inspect → fix → replay → depth returns to zero.
  The runbook documents the loop.
- Every Terminate site pays one extra publish. The publish is to a local
  JetStream and terminal failures are rare by definition; the cost is noise.
- A crash between publish and Term can leave a duplicate dead letter outside
  the dedup window. Accepted: tolerating rare duplicates is strictly better
  than any ordering that can lose the message, and the tooling states it.
- The `deadlettered` counter joins `consume.Metrics` now; #53 exposes it.
