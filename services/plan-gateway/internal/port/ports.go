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
	TargetWKT   string
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
	FootprintWKT         string
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
	FootprintWKT          string
	SlewTimeFromPreviousS *float64
	GapFromPreviousS      *float64
	AwardedValueCredits   int64
}

// RequestUnfulfilled mirrors planning.request.unfulfilled.v1.
type RequestUnfulfilled struct {
	EventAt    time.Time
	RequestID  string
	ReasonJSON []byte
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
}

// MessageSource hands the projector one message at a time.
//
// Ack is separate and explicit. The projector acks only after the fold has
// committed, so a crash mid-fold redelivers rather than skips.
type MessageSource interface {
	Next(ctx context.Context) ([]Message, error)
	Ack(ctx context.Context, m Message) error
	Nak(ctx context.Context, m Message) error
}

// Reads serves the REST endpoints.
type Reads interface {
	Plans(ctx context.Context, q PlanQuery) ([]PlanView, Cursor, error)
	Plan(ctx context.Context, satelliteID string, bucketStart time.Time, version *int) (PlanView, error)
	Acquisitions(ctx context.Context, q AcquisitionQuery) ([]AcquisitionView, Cursor, error)
	Request(ctx context.Context, requestID string) (RequestView, error)
	RequestOpportunities(ctx context.Context, requestID string) ([]OpportunityView, Cursor, error)
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
	LastEventAt      time.Time
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
