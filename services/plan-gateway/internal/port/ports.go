// Package port declares the interfaces the application layer depends on.
package port

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when a read finds nothing.
//
// Declared HERE rather than in the postgres adapter, so the HTTP handler can
// recognise it without importing the adapter — an import the arch test forbids.
// The first version had a second sentinel in httpapi and compared against that
// one; nothing ever matched, and every miss rendered as 503 instead of 404.
//
// Distinct from an empty list. "There is no plan for this bucket" and "I cannot
// tell you whether there is one" are different answers and a client must not
// conflate them.
var ErrNotFound = errors.New("not found")

// ErrMalformed marks a payload that cannot be understood — and therefore never
// will be: a payload that does not decode will not decode on redelivery
// either. Declared HERE rather than in the wire adapter for the same reason
// ErrNotFound is: the projector classifies failures as permanent or transient,
// and it may not import the adapter to learn the sentinel.
var ErrMalformed = errors.New("malformed payload")

// Projection folds events into read models.
//
// One method per event shape rather than a generic Apply(any), because the
// projector's whole job is knowing what each event means. A generic interface
// would push that knowledge into a type switch somewhere else and lose the
// compiler's help.
type Projection interface {
	ProjectRequestReceived(ctx context.Context, e RequestReceived) error
	ProjectOpportunities(ctx context.Context, e OpportunitiesComputed) error
	ProjectPlanCommitted(ctx context.Context, e PlanCommitted) error
	ProjectUnfulfilled(ctx context.Context, e RequestUnfulfilled) error
	// ProjectFeasibilityFailed records a DEFINITIVE refusal. Only ever called
	// with a non-retryable failure; the projector filters the rest.
	ProjectFeasibilityFailed(ctx context.Context, e FeasibilityFailed) error
	ProjectEphemeris(ctx context.Context, e EphemerisComputed) error

	// Cursor and Advance track how far each stream has been folded.
	Cursor(ctx context.Context, stream string) (Cursor, error)
	Advance(ctx context.Context, stream string, sequence uint64, eventAt time.Time) error

	// Reset clears every projection and every cursor.
	//
	// The operation the replay test exercises, and the reason a rebuild is
	// routine rather than an incident.
	Reset(ctx context.Context) error
}

// Cursor is how far one stream has been folded.
type Cursor struct {
	Sequence    uint64
	LastEventAt time.Time
}

// RequestReceived mirrors tasking.request.received.v1.
type RequestReceived struct {
	EventAt     time.Time
	RequestID   string
	CustomerID  string
	TargetName  string
	WindowStart time.Time
	WindowEnd   time.Time

	// GeoJSON as it arrived on the wire, handed to PostGIS unchanged.
	//
	// Not WKT. The contracts publish GeoJSON, so converting here would mean
	// hand-rolling a serialiser for a format PostGIS already parses —
	// ST_GeomFromGeoJSON — and hand-rolled WKT is exactly where ring closure
	// and coordinate order go quietly wrong.
	TargetGeoJSON []byte
}

// OpportunitiesComputed mirrors feasibility.opportunities.computed.v1.
type OpportunitiesComputed struct {
	EventAt       time.Time
	RequestID     string
	Opportunities []Opportunity
}

// Opportunity is one candidate.
type Opportunity struct {
	OpportunityID        string
	SatelliteID          string
	Mode                 string
	AccessStart          time.Time
	AccessEnd            time.Time
	AcquisitionDurationS float64
	OrbitNumber          *int
	QualityScore         float64
	FootprintGeoJSON     []byte
}

// PlanCommitted mirrors planning.plan.committed.v1.
type PlanCommitted struct {
	EventAt          time.Time
	PlanID           string
	SatelliteID      string
	BucketStart      time.Time
	BucketEnd        time.Time
	PlanVersion      int
	SupersedesPlanID *string
	Policy           string
	MetricsJSON      []byte
	CommittedAt      time.Time
	Acquisitions     []Acquisition
}

// Acquisition is one scheduled collection.
type Acquisition struct {
	AcquisitionID         string
	RequestID             string
	OpportunityID         *string
	CustomerID            string
	Mode                  string
	WindowStart           time.Time
	WindowEnd             time.Time
	FootprintGeoJSON      []byte
	SlewTimeFromPreviousS *float64
	GapFromPreviousS      *float64
	AwardedValueCredits   int64
}

// EphemerisComputed mirrors feasibility.ephemeris.computed.v1.
//
// One satellite over one bucket. The samples arrive as offsets from an epoch
// because that is cheaper on the wire; they are resolved to absolute instants
// here, at the boundary, so nothing downstream has to carry the epoch around to
// know what a sample means.
type EphemerisComputed struct {
	EventAt     time.Time
	SatelliteID string
	// The element set the track was propagated from. Carried through to the
	// projection, where a fresher one is what lets a newer track win over an
	// older one for the same instants.
	TleEpoch time.Time
	Samples  []EphemerisSample
}

// EphemerisSample is one position, at one instant.
type EphemerisSample struct {
	At           time.Time
	LongitudeDeg float64
	LatitudeDeg  float64
	// Height above the WGS84 ellipsoid, in metres. Not above terrain.
	AltitudeM float64
}

// RequestUnfulfilled mirrors planning.request.unfulfilled.v1.
type RequestUnfulfilled struct {
	EventAt    time.Time
	RequestID  string
	ReasonJSON []byte
}

// FeasibilityFailed mirrors feasibility.failed.v1.
//
// Retryable is kept out of ReasonJSON's shadow deliberately: the projector has
// to branch on it, and reaching into a []byte to make a control-flow decision
// is how a check ends up being made in two places that disagree.
type FeasibilityFailed struct {
	EventAt   time.Time
	RequestID string
	// Retryable says the failure is about OUR ability to answer rather than
	// about the physics. Those go back to the stream with backoff and must
	// NEVER reach a customer as a verdict — TLE_UNAVAILABLE is our problem,
	// and recording it as their answer makes a transient outage permanent.
	Retryable  bool
	ReasonJSON []byte
}

// Decoder turns a wire payload into the projector's own event types.
//
// A port, so the application layer never learns the wire format. The first
// version had the projector call json.Unmarshal on its own structs directly,
// which decoded every real contract payload — snake_case, and wrapped in an
// envelope — into an all-zero struct WITHOUT ERROR. See #112.
//
// One method per event rather than a generic Decode(any), for the same reason
// Projection has one method per event: the compiler should know which shape it
// is looking at.
type Decoder interface {
	RequestReceived(payload []byte) (RequestReceived, error)
	Opportunities(payload []byte) (OpportunitiesComputed, error)
	PlanCommitted(payload []byte) (PlanCommitted, error)
	Unfulfilled(payload []byte) (RequestUnfulfilled, error)
	Ephemeris(payload []byte) (EphemerisComputed, error)
	FeasibilityFailed(payload []byte) (FeasibilityFailed, error)
}

// Message is one delivery from the broker, already unwrapped from transport.
//
// Sequence and Stream come from JetStream rather than the payload. The
// projector's ordering guard has to be about delivery order on a stream, not
// about a timestamp the publisher chose.
type Message struct {
	Stream   string
	Sequence uint64
	Subject  string
	EventID  string
	EventAt  time.Time
	Payload  []byte
	// Delivered is the broker's delivery counter, 1 on the first attempt. The
	// poison decision reads it.
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

// MessageSource hands the projector one message at a time.
//
// Ack is separate and explicit. The projector acks only after the fold has
// committed, so a crash mid-fold redelivers rather than skips.
type MessageSource interface {
	Next(ctx context.Context) ([]Message, error)
	Ack(ctx context.Context, m Message) error
	Nak(ctx context.Context, m Message) error
	// Term stops delivery on purpose: poison, or the final attempt of a
	// failure retrying has not fixed.
	Term(ctx context.Context, m Message) error
	// Deadletter hands the message to its DLQ subject, with the terminal error
	// class as the reason. Called BEFORE Term, and a failure means Nak rather
	// than Term (ADR-0017): a Term without a landed dead letter is the silent
	// loss the DLQ exists to prevent — and this service is a pure projector, so
	// a message it drops exists nowhere else.
	//
	// The reason is all the application supplies. The trace context, the
	// delivery count and the consumer name are transport detail the adapter
	// already holds, and routing them through this port would put the transport
	// back into the interface that exists to keep it out.
	Deadletter(ctx context.Context, m Message, reason string) error
}

// Reads serves the REST endpoints.
type Reads interface {
	Plans(ctx context.Context, q PlanQuery) ([]PlanView, Cursor, error)
	Plan(ctx context.Context, satelliteID string, bucketStart time.Time, version *int) (PlanView, error)
	Acquisitions(ctx context.Context, q AcquisitionQuery) ([]AcquisitionView, Cursor, error)
	Request(ctx context.Context, requestID string) (RequestView, error)
	RequestOpportunities(ctx context.Context, requestID string) ([]OpportunityView, Cursor, error)

	// Ephemeris returns one satellite's samples over `[from, to)`, in time
	// order. An empty result is not an error: the sweep may not have reached
	// this horizon.
	// Targets returns request targets whose own window overlaps `[start, end)`.
	//
	// The DEMAND side of the map. Deliberately separate from Acquisitions,
	// which returns what was scheduled: a region dense with targets and empty
	// of acquisitions is the interesting case, and one query cannot show both
	// halves without an outer join that lies about one of them.
	Targets(ctx context.Context, q TargetQuery) ([]TargetView, Cursor, error)

	// OpportunityFootprints returns candidate footprints over a window, won
	// and lost.
	//
	// Distinct from RequestOpportunities, which answers "what could this ONE
	// request have had". This answers "what did the constellation consider
	// over this ground", across every request, which is the question a
	// contention view asks.
	OpportunityFootprints(ctx context.Context, q OpportunityFootprintQuery) ([]OpportunityFootprintView, Cursor, error)

	Ephemeris(ctx context.Context, satelliteID string, from, to time.Time) ([]EphemerisSample, error)

	// Constellation returns every satellite's samples over `[from, to)`, keyed
	// by satellite id. `satelliteID` narrows it to one; empty means all.
	//
	// Separate from Ephemeris rather than a loop over it: the globe asks for
	// the whole constellation at once, and one query beats one per satellite
	// against a table with a hundred thousand rows a day.
	Constellation(ctx context.Context, satelliteID string, from, to time.Time) (map[string][]EphemerisSample, Cursor, error)
}

// PlanQuery filters a plan list.
type PlanQuery struct {
	SatelliteID       string
	BucketStartAfter  *time.Time
	BucketStartBefore *time.Time
	IncludeSuperseded bool
	Limit             int
}

// AcquisitionQuery filters an acquisition list.
type AcquisitionQuery struct {
	SatelliteID string
	WindowStart time.Time
	WindowEnd   time.Time
	RequestID   string
	Statuses    []string
	Limit       int
}

// TargetQuery filters the target-density layer.
type TargetQuery struct {
	WindowStart time.Time
	WindowEnd   time.Time
	// State narrows to one request state. Empty means every state, which is
	// what a density view wants: an INFEASIBLE request is still demand, and
	// hiding it would make the map describe supply.
	State string
	Limit int
}

// TargetView is one request's target, with enough of the request to colour it.
type TargetView struct {
	RequestID  string
	CustomerID string
	TargetName string
	State      string
	// No priority tier and no bid: readmodel.request_views carries neither.
	// They live on the write side and were never projected, and declaring
	// them here would put a field in two generated languages that is served
	// as null forever — the mistake #192 corrected in the contract.
	WindowStart time.Time
	WindowEnd   time.Time
	// OpportunityCount distinguishes "nobody wanted this ground" from "nobody
	// could image it" — zero against a settled state is a feasibility answer,
	// not an absence of demand.
	OpportunityCount int
	// GeoJSON is the target geometry, already serialised by PostGIS.
	//
	// Carried as bytes rather than a decoded struct because it is a Point OR a
	// Polygon: the read model stores both in one geometry column, and decoding
	// to a Go type here would mean a union that every layer above has to
	// re-discriminate. The renderer embeds it verbatim.
	GeoJSON []byte
}

// OpportunityFootprintQuery filters the contention layer.
type OpportunityFootprintQuery struct {
	SatelliteID string
	WindowStart time.Time
	WindowEnd   time.Time
	// Awarded narrows to winners or losers. Nil means both, which is what the
	// conflict view needs — the ratio is the interesting quantity and it
	// cannot be computed from one half.
	Awarded *bool
	Limit   int
}

// OpportunityFootprintView is one candidate's footprint, won or lost.
type OpportunityFootprintView struct {
	OpportunityID string
	RequestID     string
	SatelliteID   string
	Mode          string
	WindowStart   time.Time
	WindowEnd     time.Time
	QualityScore  float64
	// Awarded is `won` from the read model. False means this candidate lost,
	// NOT that its request went unserved — the same request may have won on a
	// different pass.
	Awarded bool
	GeoJSON []byte
}

// PlanView is a projected plan.
type PlanView struct {
	PlanID           string
	SatelliteID      string
	BucketStart      time.Time
	BucketEnd        time.Time
	PlanVersion      int
	SupersedesPlanID *string
	Superseded       bool
	Policy           string
	MetricsJSON      []byte
	CommittedAt      time.Time
	Acquisitions     []AcquisitionView

	// The satellite's sampled position across this bucket, in time order.
	//
	// EMPTY IS A LEGITIMATE STATE and the renderer must treat it as one. The
	// ephemeris sweep runs on its own timer and may not have reached a bucket
	// yet, so a plan can exist before its track does. An absent orbit layer is
	// the correct rendering of that; a path interpolated through the footprint
	// centroids that ARE present would look like an orbit and be a fiction.
	//
	// Populated by Plan() and deliberately NOT by Plans(). A list of twenty
	// buckets would carry twenty thousand samples to render a table.
	Track []EphemerisSample
}

// AcquisitionView is a projected acquisition.
type AcquisitionView struct {
	AcquisitionID         string
	PlanID                string
	RequestID             string
	CustomerID            string
	SatelliteID           string
	Mode                  string
	WindowStart           time.Time
	WindowEnd             time.Time
	Status                string
	FootprintGeoJSON      []byte
	SlewTimeFromPreviousS *float64
	GapFromPreviousS      *float64
	AwardedValueCredits   int64
}

// RequestView is a projected request.
type RequestView struct {
	RequestID        string
	CustomerID       string
	TargetName       string
	State            string
	WindowStart      time.Time
	WindowEnd        time.Time
	OpportunityCount int
	UnfulfilmentJSON []byte
	// InfeasibilityJSON is set only for a DEFINITIVE refusal — no pass at a
	// valid geometry, or constraints that eliminated every one. Distinct from
	// UnfulfilmentJSON, which means the request competed and lost and will
	// compete again.
	InfeasibilityJSON []byte
	LastEventAt       time.Time
}

// OpportunityView is a projected candidate.
type OpportunityView struct {
	OpportunityID        string
	SatelliteID          string
	Mode                 string
	AccessStart          time.Time
	AccessEnd            time.Time
	AcquisitionDurationS float64
	OrbitNumber          *int
	QualityScore         float64
	FootprintGeoJSON     []byte
	Won                  bool
}
