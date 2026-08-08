// Package port declares the interfaces the application layer depends on.
package port

import (
	"context"
	"time"

	"github.com/mhayk/overpass/services/planner/internal/domain"
)

// Consumer names, matching the durable pull consumers declared in
// deploy/nats/init.sh.
//
// They double as the dedup ledger's partition key: planning.processed_events is
// keyed (consumer, event_id), so the two streams cannot mask each other's
// redeliveries. Named here rather than in the NATS adapter because the
// application layer writes them into the ledger and must not import the adapter
// to learn them.
const (
	ConsumerOpportunities = "planner-opportunities"
	ConsumerLifecycle     = "planner-lifecycle"
)

// Message is one delivery from the broker, already unwrapped from transport.
//
// Sequence and Stream come from JetStream rather than from the payload: an
// ordering guard has to be about delivery order on a stream, not about a
// timestamp the publisher chose.
type Message struct {
	Stream   string
	Sequence uint64
	Subject  string
	EventID  string
	EventAt  time.Time
	Payload  []byte
}

// MessageSource hands the projector one batch at a time.
//
// Ack is separate and explicit. The projector acks only after its transaction
// has committed, so a crash mid-fold redelivers rather than skips — which is
// the whole reason the dedup ledger exists to absorb the repeat.
type MessageSource interface {
	Next(ctx context.Context) ([]Message, error)
	Ack(ctx context.Context, m Message) error
	Nak(ctx context.Context, m Message) error
}

// RequestReceived mirrors tasking.request.received.v1, reduced to what
// ADR-0015 chose to project.
type RequestReceived struct {
	EventID  string
	EventAt  time.Time
	Snapshot domain.Snapshot
}

// OpportunitiesComputed mirrors feasibility.opportunities.computed.v1.
//
// One event carries many candidates for one request, and they are projected in
// one transaction: a partially projected batch would let a round allocate over
// half a request's options and call the other half unfulfilled.
type OpportunitiesComputed struct {
	EventID    string
	EventAt    time.Time
	RequestID  string
	Candidates []domain.Candidate
}

// Decoder turns a wire payload into the projector's own types.
//
// A port, so the application layer never learns the wire format. This is not
// ceremony — #112 is the precedent. plan-gateway's projector called
// json.Unmarshal on its own structs and decoded every real contract payload,
// enveloped and snake_case, into an ALL-ZERO STRUCT WITHOUT ERROR. Nothing
// failed; the read model was simply always empty.
//
// One method per event rather than a generic Decode(any), because the compiler
// should know which shape it is looking at.
type Decoder interface {
	RequestReceived(payload []byte) (RequestReceived, error)
	Opportunities(payload []byte) (OpportunitiesComputed, error)
}

// Projections writes the planner's two input tables.
//
// Each method is one transaction that both records the event in
// planning.processed_events and applies the projection. They commit together or
// not at all: marking an event processed outside the transaction that applied
// it turns a crash between the two into a permanently skipped event, which is
// invisible.
//
// The bool reports whether the projection was APPLIED. False means the event
// was already in the ledger — a redelivery, absorbed. That is a normal outcome
// and not an error, but it is worth returning rather than swallowing, because
// "how often are we reprocessing?" is a question M3 will ask.
type Projections interface {
	ProjectSnapshot(ctx context.Context, consumer string, e RequestReceived) (applied bool, err error)
	ProjectCandidates(ctx context.Context, consumer string, e OpportunitiesComputed) (applied bool, err error)
}
