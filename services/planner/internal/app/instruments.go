package app

import (
	"context"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/mhayk/overpass/gen/go/events"
)

// The instrument names, as constants because the dashboards query them.
//
// Every one declares its unit IN THE NAME and leaves the OTel unit field
// empty. Measured against the running collector: overpass.allocation.duration_ms
// with an empty unit exports as overpass_allocation_duration_ms_bucket, which
// is what deploy/prometheus/rules/alerts.yml has committed to since it was
// written. Declaring unit "ms" instead exports
// overpass_allocation_duration_milliseconds_bucket and silently orphans the
// PlannerRoundsSlow alert. The more correct-looking declaration is the wrong
// one, which is why this was measured rather than assumed.
const (
	InstrumentAllocationMs = "overpass.allocation.duration_ms"
	InstrumentPlanValue    = "overpass.plan.value_credits"
	InstrumentCandidates   = "overpass.round.candidate_opportunities"
	InstrumentUtilisation  = "overpass.satellite.utilisation_ratio"
	InstrumentFulfilled    = "overpass.requests.fulfilled"
	InstrumentUnfulfilled  = "overpass.requests.unfulfilled"
)

// Instruments exports what a committed plan already knows about itself.
//
// Every number here is one planMetrics already computes, because the
// planning.plan.committed.v1 contract requires it on the event. Until now they
// were serialised onto the event and, from an operator's point of view,
// dropped on the floor.
//
// Requests-unfulfilled-BY-REASON is the one that justifies the rest. A count
// of losses says the constellation is busy; the reason code says whether it is
// contended, badly slewed, or power-limited — three completely different
// remedies. Charting it is only possible because the contract made the reason
// structured data rather than a message string.
type Instruments struct {
	allocationMs metric.Float64Histogram
	planValue    metric.Float64Histogram
	candidates   metric.Int64Histogram
	fulfilled    metric.Int64Counter
	unfulfilled  metric.Int64Counter

	// Utilisation is last-value-per-satellite rather than a distribution, so
	// it is served from a map through an observable gauge. A synchronous gauge
	// would report whichever round happened to land inside the export window
	// and silently drop the others.
	mu          sync.Mutex
	utilisation map[string]float64
}

// NewInstruments builds the planner's domain instruments.
func NewInstruments(meter metric.Meter) (*Instruments, error) {
	i := &Instruments{utilisation: map[string]float64{}}

	var err error
	if i.allocationMs, err = meter.Float64Histogram(InstrumentAllocationMs,
		metric.WithDescription("Wall time one allocation round spent inside the policy, in milliseconds."),
		// Milliseconds, bracketing the 800ms SLO PlannerRoundsSlow alerts on.
		// The SDK's defaults top out at 10 in the instrument's own units,
		// which would put every real round in the first bucket.
		metric.WithExplicitBucketBoundaries(1, 5, 10, 25, 50, 100, 250, 500, 800, 1500, 3000, 10000),
	); err != nil {
		return nil, fmt.Errorf("creating %s: %w", InstrumentAllocationMs, err)
	}

	if i.planValue, err = meter.Float64Histogram(InstrumentPlanValue,
		metric.WithDescription("Total effective value of the acquisitions in one committed plan, in credits."),
		metric.WithExplicitBucketBoundaries(100, 500, 1000, 2500, 5000, 10000, 25000, 50000, 100000),
	); err != nil {
		return nil, fmt.Errorf("creating %s: %w", InstrumentPlanValue, err)
	}

	if i.candidates, err = meter.Int64Histogram(InstrumentCandidates,
		metric.WithDescription("Candidate opportunities competing in one round."),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 25, 50, 100, 250, 500, 1000),
	); err != nil {
		return nil, fmt.Errorf("creating %s: %w", InstrumentCandidates, err)
	}

	if i.fulfilled, err = meter.Int64Counter(InstrumentFulfilled,
		metric.WithDescription("Requests that won an acquisition."),
	); err != nil {
		return nil, fmt.Errorf("creating %s: %w", InstrumentFulfilled, err)
	}

	// The denominator, and it is not optional. A count of losses with nothing
	// to divide by cannot tell a contended constellation from a busy one.
	if i.unfulfilled, err = meter.Int64Counter(InstrumentUnfulfilled,
		metric.WithDescription("Requests that competed and lost, by the constraint that actually bound."),
	); err != nil {
		return nil, fmt.Errorf("creating %s: %w", InstrumentUnfulfilled, err)
	}

	if _, err = meter.Float64ObservableGauge(InstrumentUtilisation,
		metric.WithDescription("Duty-cycle seconds used over budget in the last committed plan, per satellite."),
		metric.WithFloat64Callback(i.observeUtilisation),
	); err != nil {
		return nil, fmt.Errorf("creating %s: %w", InstrumentUtilisation, err)
	}

	return i, nil
}

func (i *Instruments) observeUtilisation(_ context.Context, observer metric.Float64Observer) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	for satelliteID, ratio := range i.utilisation {
		observer.Observe(ratio, metric.WithAttributes(attribute.String("satellite_id", satelliteID)))
	}
	return nil
}

// RecordPlan exports one committed plan's metrics.
//
// A nil receiver is a no-op, so a Trigger built without a meter — which is
// every unit test — plans exactly as it otherwise would. Telemetry is not
// allowed to become a correctness dependency.
func (i *Instruments) RecordPlan(
	ctx context.Context,
	policy, satelliteID string,
	m events.PlanCommittedDataMetrics,
) {
	if i == nil {
		return
	}
	policyAttr := metric.WithAttributes(attribute.String("policy", policy))

	i.allocationMs.Record(ctx, float64(m.AllocationDurationMs), policyAttr)
	i.planValue.Record(ctx, float64(m.TotalPlanValueCredits), policyAttr)
	i.candidates.Record(ctx, int64(m.CandidateOpportunityCount))
	i.fulfilled.Add(ctx, int64(m.RequestsFulfilled))

	// Absent rather than zero when the satellite has no duty-cycle budget:
	// nothing divided by nothing is not 0% utilisation, and a flat zero on the
	// dashboard reads as an idle satellite rather than an unknown one.
	if m.SatelliteUtilisationRatio != nil {
		i.mu.Lock()
		i.utilisation[satelliteID] = float64(*m.SatelliteUtilisationRatio)
		i.mu.Unlock()
	}
}

// RecordUnfulfilled counts one request that competed and lost.
//
// The reason code is the contract enum, passed through unchanged. Seven
// bounded values, which is why it is safe as a label where request_id would
// not be.
func (i *Instruments) RecordUnfulfilled(ctx context.Context, reasonCode string) {
	if i == nil {
		return
	}
	i.unfulfilled.Add(ctx, 1, metric.WithAttributes(attribute.String("reason_code", reasonCode)))
}
