# DLQ implementation and replay tooling — design (#49, M3-02)

Approved 2026-08-09. Decisions of record: ADR-0017.

## Goal

A message that fails terminally stops occupying a consumer slot **and is kept
somewhere inspectable and replayable**. Today every Terminate site calls
`Term()` and the payload is gone — a deliberate drop, but still a drop. The
contract (`contracts/nats/topology.md`) already specifies the destination
(`dlq.<original-subject>`), the streams (exist in `deploy/nats/init.sh`, 720h
retention), and the header set. This work implements that contract and the
operational loop around it: inspect → fix → replay → alert.

## Contract amendment (before first implementation, so free)

- `Overpass-Dlq-First-Failed-At` → **`Overpass-Dlq-Failed-At`**, RFC 3339, the
  time of the terminal decision. Stateless consumers cannot know the *first*
  failure time; the old name promised information nobody has.

## Core: dead-lettering in the shared consume libraries

`lib/go/consume/dlq.go` (and the mirror in
`services/feasibility/src/feasibility/messaging/consume.py`):

```go
type DeadLetter struct {
    Subject     string // original subject; published to "dlq." + Subject
    EventID     string // becomes Nats-Msg-Id of the dead letter
    Payload     []byte
    Traceparent string // copied through verbatim; empty omits the header
    Reason      string // terminal error class, e.g. "decode", "contract", "exhausted"
    Delivered   uint64 // Overpass-Dlq-Delivery-Count
    Consumer    string // Overpass-Dlq-Consumer
}

func Deadletter(ctx context.Context, js nats.JetStreamContext, d DeadLetter) error
```

Headers written: the four `Overpass-Dlq-*` from the contract plus
`Overpass-Dlq-Failed-At` (stamped inside `Deadletter` from the clock),
`traceparent`, and `Nats-Msg-Id = EventID`.

**The ordering invariant: publish, then Term. If the publish fails, Nak.**
A Term without a landed dead letter is the silent loss this issue exists to
prevent; a Nak retries both the handling and the dead-lettering on
redelivery. Consequence: a crash between publish and Term produces a duplicate
dead letter. Within the DLQ stream's 2-minute dedup window the broker absorbs
it (Msg-Id is preserved for exactly this); beyond it, duplicates are possible
and tolerated — replay is idempotent downstream, and the runbook says so.

A `deadlettered` counter joins `consume.Metrics` on both sides. No `/metrics`
endpoint wiring here — that is #53 (M3-06); the counter is ready for it.

## Adoption sites

- planner and plan-gateway projectors: `Decision == Terminate` →
  `Deadletter` → `Term` (or `Nak` on publish failure).
- both `natsmsg` sources' unparseable-metadata path: raw `*nats.Msg` in hand;
  dead-letter with `Reason: "metadata"` before the existing `Term`.
- feasibility worker: the two `term()` sites (poison decode, exhausted
  retries). The `FAILED_TERMINAL` business-refusal path is NOT dead-lettered —
  a published refusal is completed work, not a loss.
- tasking-api has no consumer yet; nothing to adopt.

## Tooling

The interface is already documented in topology.md's replay procedure and is
therefore the contract:

- `make dlq-inspect STREAM=<name>` → `scripts/dlq-inspect.sh` — per-stream
  depth, then per-message reason, delivery count, event id, headers.
- `make dlq-replay STREAM=<name> EVENT_ID=<uuid>` → `scripts/dlq-replay.sh` —
  republish that message's payload to `Overpass-Dlq-Original-Subject` with the
  original `Nats-Msg-Id` (consumer ledgers arbitrate duplicates), then delete
  the DLQ entry so depth means "outstanding".
- Both run through the `natsio/nats-box` image already used in CI — no new
  host dependency. `make dlq-inspect` / `make dlq-replay` wrap them.
- Runbook: `docs/runbooks/dlq-replay.md` — alert fires → inspect → diagnose
  via traceparent in Grafana → fix → replay → verify depth zero.

## Alerting

`prometheus-nats-exporter` joins the compose infrastructure (approved new
dependency: buys a true JetStream stream-depth gauge and the consumer metrics
M3-06 wants; costs one small container). Alert rules: DLQ depth > 0 sustained,
per stream.

## Test (the acceptance loop)

Integration test in `tests/integration`: a poison message published to
`TASKING` is terminated by the real consumer and lands in `DLQ_TASKING` with
the full header set and intact `traceparent`; replaying a corrected message
through the replay path processes it exactly once. Tool behaviour (nats-box
CLI flags, exporter metric names) is verified against the running stack before
being relied on — never asserted from memory.

## Delivery

1. PR 1 — contract amendment + `lib/go/consume/dlq.go` + Python mirror +
   ADR-0017 + this spec (`crosses-tracks`).
2. PR 2 — adoption in planner, plan-gateway, feasibility (`crosses-tracks`).
3. PR 3 — exporter + alert rules, inspect/replay scripts + Make targets,
   runbook, integration test. Closes #49.

## Out of scope

`/metrics` endpoints (#53), chaos scenarios that *produce* poison (#50),
DLQ browsing UI. YAGNI.
