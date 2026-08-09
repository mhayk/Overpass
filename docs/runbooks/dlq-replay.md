# Runbook — dead letters

**Alert:** `DeadLetterQueueNotEmpty` — `max by (stream_name)
(jetstream_stream_total_messages{stream_name=~"DLQ_.*"}) > 0` for 1 minute.

**What it means:** a consumer hit a terminal failure, kept the payload, and
stopped delivery on purpose. Nothing is lost and nothing is retrying; there is
one message waiting for a human. Design of record:
[ADR-0017](../decisions/0017-dlq-publication-and-replay-semantics.md).

**Urgency:** not a page. Ingress is unaffected, the pipeline is not blocked, and
the message keeps for 720 hours. It is a bug report with the evidence attached.

---

## 1. See what is waiting

```bash
make dlq-inspect                       # depth across all three streams
make dlq-inspect STREAM=DLQ_TASKING    # one row per dead letter, newest first
```

```
SEQ      FAILED AT              REASON     TRIES  CONSUMER                 EVENT ID
11       2026-08-09T15:56:02Z   exhausted  5      feasibility-worker       c42d0eda-…
10       2026-08-09T15:42:38Z   decode     1      feasibility-worker
```

`REASON` is the first thing to read, because it says which kind of incident this
is:

| Reason | What happened | Where to look |
| --- | --- | --- |
| `decode` | The payload is not a readable envelope | The **producer**. Something is publishing malformed events. |
| `contract` | It parsed, then failed the schema or a domain guard | The producer, or a contract change that landed without its consumer. |
| `metadata` | The broker delivery itself would not parse | The broker or the client library. Rare, and interesting. |
| `exhausted` | Retried to the limit and never succeeded | The **consumer's dependencies** — the database, a downstream service. |

`TRIES` separates the first two groups from the last on its own: a `1` means the
consumer refused it immediately, a `5` means it tried until the budget ran out.

An empty `EVENT ID` is not a fault. It means the message carried no readable id,
which is usually the same defect that killed it, and it is why replay also
accepts a sequence number.

## 2. Diagnose from the trace

The `traceparent` is preserved on every dead letter, so the failure is a trace
in Grafana rather than a reconstruction from logs. Take the trace id — the
middle field of the header — and open it in Tempo:

```bash
# the full header set for one dead letter
docker run --rm --network host natsio/nats-box:0.14.3 \
  nats --server nats://127.0.0.1:4222 stream get DLQ_TASKING 11 --json \
  | jq -r '.hdrs | @base64d'
```

The span that failed is the one to read. If the reason is `exhausted`, look at
what its child spans were waiting on.

## 3. Fix the cause

A dead letter is almost always a code bug or a bad deployment, not a bad
message. Fix the cause and deploy it **before** replaying: replaying into the
same bug produces the same dead letter, with a new timestamp and a second entry.

## 4. Replay

```bash
make dlq-replay STREAM=DLQ_TASKING EVENT_ID=c42d0eda-678f-47ee-a355-70b766c0c9b6

# for a dead letter with no readable event id
make dlq-replay STREAM=DLQ_TASKING SEQ=10
```

The message is republished to its original subject with its original
`Nats-Msg-Id` and `traceparent`, and the DLQ entry is deleted only after the
tool has confirmed the message landed in the source stream.

**Replay is safe because every consumer is idempotent.** The ledger arbitrates
on event id: a message that was half-processed before it died is absorbed as a
duplicate, and one that never committed is processed as new. No drain, no
coordination, no maintenance window. This is what the idempotency tax bought.

If the tool prints `keeping the dead letter`, the republish could not be
confirmed — the original subject no longer belongs to any stream, or the broker
refused it. Nothing has been deleted. Fix the topology and run it again.

## 5. Confirm

```bash
make dlq-inspect     # depth returns to zero, the alert clears
```

Depth means outstanding operational work. If it does not return to zero, either
another dead letter arrived — check whether the fix actually shipped — or the
replay produced a new one, which means the cause is still there.

---

## Things that are not incidents

- **A duplicate dead letter for the same event.** A crash between the publish
  and the Term produces one, and the DLQ stream's 2-minute dedup window absorbs
  most of them. Replaying one of the pair and deleting the other is correct;
  replaying both is also correct, because the consumer ledger absorbs the
  second.
- **A replay that immediately shows as a duplicate downstream.** That is the
  ledger doing its job on a message that had already committed its work.

## When the queue is large

`make dlq-inspect` shows the newest 20 and says so. `DLQ_LIMIT=200 make
dlq-inspect STREAM=…` shows more, at one broker round trip each. A DLQ with
hundreds of entries is one incident, not hundreds: find the common reason, fix
it, then replay in a loop over the event ids.
