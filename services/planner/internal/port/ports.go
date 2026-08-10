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
	// Delivered is the broker's delivery counter for this message, 1 on the
	// first attempt. The poison decision reads it: retry until the LAST
	// delivery, then terminate deliberately rather than letting max_deliver
	// lapse into a silent drop.
	Delivered uint64
	// Headers carries the broker's message headers, which is where the W3C
	// traceparent lives.
	//
	// It was absent until #52, and its absence was the reason this service
	// could not continue a trace: the natsmsg adapter read the payload and
	// the metadata and dropped the headers, so by the time the projector saw
	// a message the causal chain had already been discarded one layer below
	// it. Every span this service produced would have been a root.
	Headers map[string]string
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
	// Term stops delivery of a message on purpose: poison, or the final
	// attempt of a failure retrying has not fixed. Distinct from Ack — the
	// work did NOT land — and from Nak — nobody wants it again.
	Term(ctx context.Context, m Message) error
	// Deadletter hands the message to its DLQ subject, with the terminal error
	// class as the reason. Called BEFORE Term, and a failure means Nak rather
	// than Term (ADR-0017): a Term without a landed dead letter is the silent
	// loss the DLQ exists to prevent.
	//
	// The reason is all the application supplies. Everything else on the wire —
	// the trace context, the delivery count, the consumer name — is transport
	// detail the adapter already holds, and routing it through this port would
	// put the transport back into the interface that exists to keep it out.
	Deadletter(ctx context.Context, m Message, reason string) error
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

// ErrConcurrentPlan marks a round that lost an optimistic-concurrency check on
// the plan it was replacing.
//
// Distinct from a constraint violation: nothing is malformed, the round simply
// read a plan that has since moved. It aborts and the bucket stays dirty, so
// the next round sees the newer state.
var ErrConcurrentPlan = errors.New("the plan being superseded changed under this round")

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

// JoinableCandidate is one candidate joined to its request's snapshot — the
// raw rows a round allocates from, before fairness scores them.
//
// Raw rather than ScoredCandidate, because scoring is domain work the adapter
// must not do: effective value depends on the fairness configuration and the
// round's clock, neither of which belongs in SQL.
type JoinableCandidate struct {
	domain.Candidate

	CustomerID   string
	PriorityTier string
	BidCredits   int64
	SubmittedAt  time.Time
	// Deadline is upper(request_window): the instant the acquisition must have
	// FINISHED by.
	Deadline time.Time
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

	// Joinable are the candidates a policy may allocate — the ones behind
	// CandidateRequestIDs, with their request facts attached.
	Joinable []JoinableCandidate

	// Profile is the satellite's agility and power budget, read in the same
	// transaction so the plan is explicable against the numbers it actually
	// used.
	Profile domain.SatelliteProfile

	// AgeRounds counts, per request, the rounds in this bucket that have
	// already considered it. An observability figure for the unfulfilment
	// event, NOT an input to fairness — ageing is by time, and the M2-09 commit
	// records why.
	AgeRounds        map[string]int
	DutyCycleBudgetS float64
	LivePlanID       *string
	// LivePlanRowVersion is what the live plan's row_version was when this
	// round read it, under the lock. The supersession update is guarded on it,
	// so a plan touched by anybody in between aborts the round rather than
	// being silently overwritten.
	LivePlanRowVersion int
	// NextPlanVersion is the version this round's plan would carry. Dense per
	// bucket, so a gap means a round committed and was rolled back.
	NextPlanVersion int

	// LivePlanHolders are the requests holding ACTIVE acquisitions on the live
	// plan. A holder the new plan does not include gets a SUPERSEDED
	// unfulfilment — losing a won slot silently is the worst possible customer
	// experience and the easiest bug to write, so the set is read explicitly
	// rather than inferred from the candidate ledger, which a holder may have
	// aged out of.
	LivePlanHolders []string
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

// PlanCommit is a plan ready to be written, produced under the lock.
//
// Committed in the SAME transaction that opened the round, and that is a
// correctness requirement rather than an optimisation. Allocating outside the
// lock and committing afterwards leaves a window in which another planner opens
// its own round over the same bucket, and both commit plans that were each
// correct against a state neither of them ended in.
type PlanCommit struct {
	PlanID  string
	RoundID string

	// SupersedesPlanID is the live plan this replaces, and its acquisitions are
	// demoted to SUPERSEDED in this same transaction. ADR-0012 retains them
	// rather than deleting: the SUPERSEDED reason code promises the customer an
	// account of what replaced them, and deleting the evidence in the
	// transaction that creates the need for it is not an explanation.
	SupersedesPlanID *string
	// PlanVersion is dense and unique per bucket. collection_plans_unique_version
	// is the backstop that speaks up if the advisory lock ever fails.
	PlanVersion int
	// SupersededRowVersion is the row_version the round READ from the plan it
	// is replacing. The update is guarded on it, so a plan touched by anybody
	// else since then aborts the round instead of being silently overwritten.
	SupersededRowVersion int

	Policy      string
	MetricsJSON []byte
	CommittedAt time.Time

	Acquisitions []domain.ScheduledAcquisition
	Unfulfilled  []domain.Unfulfilment

	// PlanEventID and the payload for planning.plan.committed.v1, plus one
	// event per unfulfilled request. Built by the application layer from the
	// generated contract types, so a field the contract adds is a compile error
	// rather than a message somebody's consumer terms at 3am.
	PlanEventID       string
	PlanPayload       []byte
	UnfulfilledEvents []OutboxEvent
}

// OutboxEvent is one event to enqueue alongside the plan.
type OutboxEvent struct {
	EventID   string
	EventType string
	Subject   string
	Payload   []byte
}

// RoundOutcome is what a round decided, under the lock.
//
// Plan is nil when the round opened but allocated nothing — which is what M2-01
// does on its own, and what a round with no joinable candidates does forever.
type RoundOutcome struct {
	Round        Round
	RoundPayload []byte
	Plan         *PlanCommit
}

// Rounds is the planner's round ledger and its lock.
type Rounds interface {
	// DirtyBuckets finds buckets with candidates that arrived after the most
	// recent round over them. Dirtiness is DERIVED — there is no is_dirty
	// column and no timer state on disk, so a planner restart loses nothing.
	DirtyBuckets(ctx context.Context, q BucketQuery) ([]domain.BucketState, error)

	// OpenRound takes the advisory lock for key, re-reads the candidate set
	// under it, and calls open. Everything open returns — the round row, the
	// plan, its acquisitions, the demotion of any superseded ones, and every
	// outbox row — is written in the SAME transaction that holds the lock, and
	// the lock releases at commit.
	//
	// A constraint violation from the database therefore aborts the WHOLE
	// round. That is deliberate: the exclusion constraint is a backstop, not
	// the primary mechanism, so if it fires the policy has a bug, and
	// committing the rows that happened to be legal would leave a plan that is
	// silently missing whatever the policy got wrong.
	//
	// Returns false when open returned ErrSkipRound.
	OpenRound(ctx context.Context, key domain.RoundKey, bucketEnd time.Time,
		open func(RoundInputs) (RoundOutcome, error)) (opened bool, err error)
}

// Satellites reads the per-satellite parameters allocation depends on.
//
// A port of its own rather than a method on Rounds. A profile is read once per
// satellite per round and then consulted for every candidate and every pairwise
// transition, so it belongs to whatever is doing the allocating — which is
// M2-04, not the round trigger.
type Satellites interface {
	// Profile returns one satellite's agility and power budget. ErrNotFound
	// when the satellite is unknown, which is a different fact from a satellite
	// with default parameters and must not be flattened into one.
	Profile(ctx context.Context, satelliteID string) (domain.SatelliteProfile, error)
}

// ErrNotFound is returned when a read finds nothing.
//
// Declared in port so a caller can recognise it without importing the adapter —
// an import the arch test forbids.
var ErrNotFound = errors.New("not found")
