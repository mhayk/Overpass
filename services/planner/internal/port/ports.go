// Package port declares the interfaces the application layer depends on.
package port

import (
	"context"
	"errors"
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

// ---------------------------------------------------------------------------
// Rounds — M2-01
// ---------------------------------------------------------------------------

// ErrSkipRound tells OpenRound to roll back without opening.
//
// A sentinel rather than a (Round, bool) return, because the decision is made
// INSIDE the locked transaction and the honest way to say "never mind" from in
// there is to abort it. Returning a zero Round and a false would leave the
// adapter deciding whether a zero value means anything.
var ErrSkipRound = errors.New("round not opened")

// BucketQuery bounds the search for dirty buckets.
type BucketQuery struct {
	// BucketDuration must divide 24h evenly — see domain.ValidBucketDuration.
	BucketDuration time.Duration
	// HorizonStart and HorizonEnd bound which buckets are considered. A round
	// over a bucket that has already elapsed cannot be flown, and one far in
	// the future is planning against candidates that will be superseded many
	// times before they matter.
	HorizonStart time.Time
	HorizonEnd   time.Time
	// Limit caps one sweep. The planner holds a lock per round, so an unbounded
	// sweep is an unbounded series of locks taken in one pass.
	Limit int
}

// RoundInputs is the candidate set as it stood UNDER THE LOCK.
//
// Re-read inside the locked transaction rather than carried in from the sweep.
// The sweep is unlocked and its answer is already stale by the time the lock is
// acquired: candidates arrive continuously, and a round that announced a count
// it did not actually read would break the contract's conservation property
// before allocation had even begun.
type RoundInputs struct {
	Key                       domain.RoundKey
	BucketEnd                 time.Time
	CandidateOpportunityCount int
	// CandidateRequestIDs excludes HELD candidates, per ADR-0014: a candidate
	// whose request snapshot has not landed can become neither an acquisition
	// nor an unfulfilment, so listing it would fail the contract's conservation
	// test.
	CandidateRequestIDs []string
	DutyCycleBudgetS    float64
	LivePlanID          *string
}

// Round is one opened allocation round, ready to record and announce.
type Round struct {
	RoundID       string
	EventID       string
	CorrelationID string
	CausationID   *string

	Key       domain.RoundKey
	BucketEnd time.Time

	Trigger                   string
	Policy                    string
	CandidateOpportunityCount int
	CandidateRequestIDs       []string
	DutyCycleBudgetS          float64
	SupersedesPlanID          *string
	TriggeredAt               time.Time
}

// Rounds is the planner's round ledger and its lock.
type Rounds interface {
	// DirtyBuckets finds buckets with candidates that arrived after the most
	// recent round over them. Dirtiness is DERIVED — there is no is_dirty
	// column and no timer state on disk, so a planner restart loses nothing.
	DirtyBuckets(ctx context.Context, q BucketQuery) ([]domain.BucketState, error)

	// OpenRound takes the advisory lock for key, re-reads the candidate set
	// under it, and calls open. If open returns a Round and its payload, both
	// the round row and the outbox row are written in the same transaction that
	// holds the lock; the lock releases at commit.
	//
	// Returns false when open returned ErrSkipRound.
	OpenRound(ctx context.Context, key domain.RoundKey, bucketEnd time.Time,
		open func(RoundInputs) (Round, []byte, error)) (opened bool, err error)
}

// Satellites reads the per-satellite parameters the transition model needs.
//
// A port of its own rather than a method on Rounds. Agility is read once per
// satellite per round and then consulted for every pairwise transition, so it
// belongs to whatever is doing the allocating — which is M2-04, not the round
// trigger.
type Satellites interface {
	// Agility returns one satellite's transition parameters. ErrNotFound when
	// the satellite is unknown, which is a different fact from a satellite with
	// default parameters and must not be flattened into one.
	Agility(ctx context.Context, satelliteID string) (domain.Agility, error)
}

// ErrNotFound is returned when a read finds nothing.
//
// Declared in port so a caller can recognise it without importing the adapter —
// an import the arch test forbids.
var ErrNotFound = errors.New("not found")
