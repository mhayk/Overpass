package app_test

import (
	"context"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/mhayk/overpass/gen/go/events"
	"github.com/mhayk/overpass/services/planner/internal/app"
)

// contractReasonCodes is the enum from
// contracts/events/planning.request.unfulfilled.v1.schema.json, restated here
// on purpose: if the contract gains a reason and this list is not updated, the
// test still passes but the gap is in the dashboard rather than the code. The
// list being visible is what makes that gap reviewable.
var contractReasonCodes = []string{
	"LOST_TO_HIGHER_VALUE",
	"BLOCKED_BY_SLEW_CONSTRAINT",
	"DUTY_CYCLE_EXHAUSTED",
	"DEADLINE_PASSED",
	"NO_OPPORTUNITY_IN_BUCKET",
	"SUPERSEDED",
	"CANCELLED_BY_CUSTOMER",
}

// Every reason the planner can emit must produce its own series.
//
// This is the metric the whole issue exists for. A count of losses says the
// constellation is busy; the reason says whether it is contended, badly
// slewed, or power-limited — three completely different remedies. A reason
// that is charted nowhere is a customer question nobody can answer.
func TestEveryContractReasonCodeIsRecordable(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	instruments := newInstruments(t, reader)

	for _, reason := range contractReasonCodes {
		instruments.RecordUnfulfilled(context.Background(), reason)
	}

	seen := map[string]bool{}
	for _, dp := range sumPoints(t, collect(t, reader), app.InstrumentUnfulfilled) {
		v, ok := dp.Attributes.Value("reason_code")
		if !ok {
			t.Fatal("an unfulfilment data point carries no reason_code")
		}
		seen[v.AsString()] = true
	}
	for _, reason := range contractReasonCodes {
		if !seen[reason] {
			t.Errorf("no series for reason_code %q", reason)
		}
	}
}

// The instrument name the committed alert rule depends on.
//
// deploy/prometheus/rules/alerts.yml queries
// overpass_allocation_duration_ms_bucket. Measured against the running
// collector: this OTel name with an EMPTY unit produces exactly that.
// Renaming the instrument, or "fixing" it to declare unit "ms", exports
// overpass_allocation_duration_milliseconds_bucket instead and silently
// orphans PlannerRoundsSlow. This test is the tripwire for both.
func TestAllocationInstrumentMatchesTheCommittedAlertRule(t *testing.T) {
	if app.InstrumentAllocationMs != "overpass.allocation.duration_ms" {
		t.Fatalf("instrument name = %q; alerts.yml queries overpass_allocation_duration_ms_bucket",
			app.InstrumentAllocationMs)
	}

	reader := sdkmetric.NewManualReader()
	instruments := newInstruments(t, reader)
	instruments.RecordPlan(context.Background(), "GREEDY_BY_BID", "SENTINEL-1A", samplePlanMetrics())

	for _, scope := range collect(t, reader).ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == app.InstrumentAllocationMs && m.Unit != "" {
				t.Errorf("unit = %q, want empty: any unit makes the exporter rename the series", m.Unit)
			}
		}
	}
}

// Utilisation is absent, not zero, when the satellite has no duty-cycle budget.
//
// Nothing divided by nothing is not 0% utilisation. A flat zero on the
// dashboard reads as an idle satellite rather than an unknown one, and those
// call for opposite reactions.
func TestUtilisationIsAbsentRatherThanZeroWithoutABudget(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	instruments := newInstruments(t, reader)

	metrics := samplePlanMetrics()
	metrics.SatelliteUtilisationRatio = nil
	instruments.RecordPlan(context.Background(), "GREEDY_BY_BID", "SENTINEL-1A", metrics)

	for _, scope := range collect(t, reader).ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != app.InstrumentUtilisation {
				continue
			}
			if g, ok := m.Data.(metricdata.Gauge[float64]); ok && len(g.DataPoints) > 0 {
				t.Errorf("utilisation reported %v with no budget; want no series at all", g.DataPoints)
			}
		}
	}
}

// A nil Instruments is a no-op.
//
// Every unit test builds a Trigger without a meter, and a planner that refused
// to plan without one would have made telemetry a correctness dependency —
// exactly the inversion lib/go/telemetry's Setup avoids for the same reason.
func TestNilInstrumentsIsANoOp(t *testing.T) {
	var instruments *app.Instruments
	instruments.RecordPlan(context.Background(), "GREEDY_BY_BID", "SENTINEL-1A", samplePlanMetrics())
	instruments.RecordUnfulfilled(context.Background(), "LOST_TO_HIGHER_VALUE")
}

func newInstruments(t *testing.T, reader sdkmetric.Reader) *app.Instruments {
	t.Helper()
	instruments, err := app.NewInstruments(
		sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"))
	if err != nil {
		t.Fatalf("NewInstruments: %v", err)
	}
	return instruments
}

func samplePlanMetrics() events.PlanCommittedDataMetrics {
	utilisation := events.Ratio(0.42)
	unfulfilled := 2
	return events.PlanCommittedDataMetrics{
		TotalPlanValueCredits:     events.Credits(9000),
		RequestsFulfilled:         3,
		RequestsUnfulfilled:       &unfulfilled,
		CandidateOpportunityCount: 12,
		AllocationDurationMs:      events.DurationMillis(37),
		SatelliteUtilisationRatio: &utilisation,
	}
}

func collect(t *testing.T, reader sdkmetric.Reader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return rm
}

func sumPoints(t *testing.T, rm metricdata.ResourceMetrics, name string) []metricdata.DataPoint[int64] {
	t.Helper()
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			s, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", name, m.Data)
			}
			return s.DataPoints
		}
	}
	t.Fatalf("no instrument named %q", name)
	return nil
}
