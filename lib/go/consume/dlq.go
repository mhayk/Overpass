package consume

// Dead-lettering: the step that turns a Term from a deliberate DROP into a
// deliberate HANDOFF.
//
// #48 made terminal failure a decision — poison terminates on the first
// delivery, exhausted retries on the last, both with a log line and a metric.
// What a Term still did was lose the payload. This file publishes it first, to
// the `dlq.`-prefixed mirror of its original subject, with the header set
// `contracts/nats/topology.md` has specified since M0.
//
// THE ORDERING INVARIANT, which this package cannot enforce for you because the
// ack handle lives in your adapter (ADR-0017):
//
//	PUBLISH, THEN TERM. IF THE PUBLISH FAILS, NAK.
//
// A Term without a landed dead letter re-creates the loss with extra steps. A
// Nak retries the whole delivery — the handling and the dead-lettering — which
// is safe because both are idempotent. The residue is a crash between publish
// and Term, which produces a duplicate dead letter; the original event id rides
// along as `Nats-Msg-Id` so the DLQ stream's dedup window absorbs it, and
// beyond that window duplicates are tolerated because everything downstream of
// the DLQ is duplicate-safe by construction.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SubjectPrefix maps an original subject onto its dead-letter subject.
//
// A PREFIX, not an infix. The obvious-looking `tasking.dlq.>` is already inside
// the TASKING stream's `tasking.>` wildcard, and NATS refuses to create two
// streams whose subjects overlap (error 10065). Prefixing keeps the two subject
// spaces disjoint and keeps the mapping reversible by trimming, which is what
// makes the replay script a string operation rather than a lookup table.
const SubjectPrefix = "dlq."

// The header set from contracts/nats/topology.md. Named constants because the
// inspect and replay tooling reads exactly these strings: a typo in one place
// is a tool that silently reports nothing rather than a build failure.
const (
	HeaderReason          = "Overpass-Dlq-Reason"
	HeaderOriginalSubject = "Overpass-Dlq-Original-Subject"
	HeaderDeliveryCount   = "Overpass-Dlq-Delivery-Count"
	HeaderFailedAt        = "Overpass-Dlq-Failed-At"
	HeaderConsumer        = "Overpass-Dlq-Consumer"

	// HeaderTraceparent is carried through verbatim so the trace of a dead
	// message is still complete — the failure is inspectable in Grafana rather
	// than reconstructed from logs.
	HeaderTraceparent = "traceparent"

	// HeaderMsgID is JetStream's dedup key, set to the ORIGINAL event id.
	HeaderMsgID = "Nats-Msg-Id"
)

// Terminal error classes. A small shared vocabulary rather than free text: the
// runbook triages on `Overpass-Dlq-Reason`, and four consumers each inventing
// their own wording is a triage step that starts with reading source code.
const (
	// ReasonDecode: the payload is not a parseable envelope.
	ReasonDecode = "decode"
	// ReasonContract: it parsed and then failed the contract or a domain guard.
	ReasonContract = "contract"
	// ReasonMetadata: the broker metadata would not parse, so the delivery
	// could not even be identified.
	ReasonMetadata = "metadata"
	// ReasonExhausted: retrying was allowed to run its course and did not fix
	// it. The last delivery terminates rather than lapsing.
	ReasonExhausted = "exhausted"
)

// Publisher is the one method Deadletter needs.
//
// An interface rather than a JetStream context, for the reason stated in this
// module's go.mod: the transport stays in each service's adapter, and this
// module keeps no NATS import. The map is `nats.Header`'s underlying type, so
// an adapter converts with `nats.Header(header)` and nothing else.
type Publisher interface {
	Publish(ctx context.Context, subject string, header map[string][]string, payload []byte) error
}

// DeadLetter is one terminal failure, as it will appear to whoever inspects it.
type DeadLetter struct {
	// Subject the message arrived on. Published to SubjectPrefix + Subject.
	Subject string
	// EventID becomes Nats-Msg-Id. May be empty — see Deadletter.
	EventID string
	// Payload is republished byte-for-byte; replay depends on it being the
	// original, not a re-serialisation.
	Payload []byte
	// Traceparent as received. Empty omits the header.
	Traceparent string
	// Reason is the terminal error class — one of the Reason constants.
	Reason string
	// Delivered is how many attempts the broker made.
	Delivered uint64
	// Consumer is the durable consumer that gave up.
	Consumer string
	// FailedAt is when the terminal decision was made. Zero means now.
	//
	// The decision time, NOT the first failure: consumers are stateless across
	// deliveries, nobody knows when the first failure happened, and a header
	// cannot promise information nobody has (ADR-0017).
	FailedAt time.Time
}

// Deadletter publishes the dead letter. The caller Terms on success and Naks on
// error — never the other way round.
//
// What it refuses and what it tolerates is a deliberate split:
//
// REFUSED — subject, reason, consumer. All three are call-site facts: literals
// and a consumer name known at wiring time, never anything a bad message can
// influence. An empty one is a programming error, and it cannot be triggered in
// production by traffic.
//
// TOLERATED — a missing event id or traceparent. Both are message data, and a
// message with no id is exactly the kind that dies here. Refusing would leave
// the caller Naking forever under the ordering above, which is the loss this
// whole mechanism exists to prevent: a dead letter with no dedup key is worth
// far more than no dead letter at all.
func Deadletter(ctx context.Context, pub Publisher, d DeadLetter) error {
	switch {
	case d.Subject == "":
		return fmt.Errorf("dead letter has no original subject; there is nowhere to publish it")
	case strings.HasPrefix(d.Subject, SubjectPrefix):
		return fmt.Errorf("subject %q is already a dead letter; dead-lettering it again would invent a subject space nobody declared", d.Subject)
	case d.Reason == "":
		return fmt.Errorf("dead letter for %q has no reason; an operator would find a payload and no account of why it is there", d.Subject)
	case d.Consumer == "":
		return fmt.Errorf("dead letter for %q names no consumer; an operator would not know who gave up on it", d.Subject)
	}

	failedAt := d.FailedAt
	if failedAt.IsZero() {
		failedAt = time.Now()
	}

	header := map[string][]string{
		HeaderReason:          {d.Reason},
		HeaderOriginalSubject: {d.Subject},
		HeaderDeliveryCount:   {strconv.FormatUint(d.Delivered, 10)},
		HeaderFailedAt:        {failedAt.UTC().Format(time.RFC3339)},
		HeaderConsumer:        {d.Consumer},
	}
	// Absent, not blank. A blank traceparent parses downstream as a broken
	// trace context, where an absent one parses as "no trace" — and a blank
	// Msg-Id is a dedup key every id-less message would share.
	if d.Traceparent != "" {
		header[HeaderTraceparent] = []string{d.Traceparent}
	}
	if d.EventID != "" {
		header[HeaderMsgID] = []string{d.EventID}
	}

	if err := pub.Publish(ctx, SubjectPrefix+d.Subject, header, d.Payload); err != nil {
		return fmt.Errorf("publishing dead letter for %s on %s: %w", d.EventID, d.Subject, err)
	}
	return nil
}
