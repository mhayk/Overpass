package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/plan-gateway/internal/adapter/postgres"
	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
)

// These run against a real Postgres because the thing under test IS the SQL.
//
// A fake would exercise the Go around the query and skip the query, which is
// where every one of the interesting mistakes lives: a range operator that is
// inclusive at the wrong end, a version filter that silently returns the
// superseded row, a NULL comparison that quietly matches nothing.

// seeded folds the standard fixture and hands back the reader.
func seeded(t *testing.T, versions ...int) (*postgres.Reads, fixture) {
	t.Helper()
	p := pool(t)
	pr := postgres.NewProjection(p)
	f := newFixture()

	if err := pr.Reset(t.Context()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	t.Cleanup(func() {
		if err := pr.Reset(context.Background()); err != nil {
			t.Errorf("reset failed; later tests will see this test's rows: %v", err)
		}
	})

	if err := pr.ProjectRequestReceived(t.Context(), f.received()); err != nil {
		t.Fatalf("request: %v", err)
	}
	if err := pr.ProjectOpportunities(t.Context(), f.opportunities()); err != nil {
		t.Fatalf("opportunities: %v", err)
	}
	for _, v := range versions {
		if err := pr.ProjectPlanCommitted(t.Context(), f.plan(v)); err != nil {
			t.Fatalf("plan v%d: %v", v, err)
		}
	}
	if err := pr.Advance(t.Context(), "PLANNING", 42, epoch.Add(time.Hour)); err != nil {
		t.Fatalf("advance: %v", err)
	}
	return postgres.NewReads(p), f
}

// TestPlanReadsTheCurrentVersionNotTheLatestRow is the one worth the setup.
//
// Two versions of the same bucket are both stored — ADR-0012 keeps the history
// — so "the plan" cannot mean "the row that exists". An ORDER BY that got this
// wrong would serve a superseded plan as current, and nothing about the
// response would look unusual.
func TestPlanReadsTheCurrentVersionNotTheLatestRow(t *testing.T) {
	reads, f := seeded(t, 1, 2)

	plan, err := reads.Plan(t.Context(), f.satelliteID, bucket, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.PlanVersion != 2 {
		t.Fatalf("served version %d as current, want 2", plan.PlanVersion)
	}
	if plan.Superseded {
		t.Error("the current plan is marked superseded")
	}
	if len(plan.Acquisitions) != 1 {
		t.Fatalf("got %d acquisitions, want 1", len(plan.Acquisitions))
	}
	// The footprint must come back as GeoJSON, not WKT: the browser cannot read
	// WKT and a string that looks like data is the hardest kind of wrong.
	var geom map[string]any
	if err := json.Unmarshal(plan.Acquisitions[0].FootprintGeoJSON, &geom); err != nil {
		t.Fatalf("footprint is not JSON: %v (%s)", err, plan.Acquisitions[0].FootprintGeoJSON)
	}
	if geom["type"] != "Polygon" {
		t.Errorf("footprint type = %v, want Polygon", geom["type"])
	}
}

// TestASupersededVersionIsStillReachableByName is what ADR-0012's retention is
// FOR. Keeping history that nothing can read is just disk.
func TestASupersededVersionIsStillReachableByName(t *testing.T) {
	reads, f := seeded(t, 1, 2)

	one := 1
	plan, err := reads.Plan(t.Context(), f.satelliteID, bucket, &one)
	if err != nil {
		t.Fatalf("plan v1: %v", err)
	}
	if plan.PlanVersion != 1 {
		t.Fatalf("asked for version 1, got %d", plan.PlanVersion)
	}
	if !plan.Superseded {
		t.Error("version 1 is not marked superseded even though version 2 exists")
	}
}

// TestSupersededPlansAreHiddenFromTheListUnlessAsked keeps the default view
// readable. Two versions of every bucket is not a timeline anyone can use.
func TestSupersededPlansAreHiddenFromTheListUnlessAsked(t *testing.T) {
	reads, f := seeded(t, 1, 2)

	live, _, err := reads.Plans(t.Context(), port.PlanQuery{SatelliteID: f.satelliteID})
	if err != nil {
		t.Fatalf("plans: %v", err)
	}
	if len(live) != 1 || live[0].PlanVersion != 2 {
		t.Fatalf("default list = %d plans (versions %v), want just version 2", len(live), versionsOf(live))
	}

	all, _, err := reads.Plans(t.Context(), port.PlanQuery{
		SatelliteID: f.satelliteID, IncludeSuperseded: true,
	})
	if err != nil {
		t.Fatalf("plans: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("include_superseded gave %d plans (versions %v), want 2", len(all), versionsOf(all))
	}
}

func versionsOf(plans []port.PlanView) []int {
	out := make([]int, 0, len(plans))
	for _, p := range plans {
		out = append(out, p.PlanVersion)
	}
	return out
}

// TestAcquisitionOverlapIsHalfOpen pins the range semantics.
//
// tstzrange '[)' — a window that ends exactly when the query starts must NOT
// match. Get this wrong and every acquisition appears in two adjacent buckets,
// which reads as duplicate work on a timeline and is very hard to trace back to
// a bracket.
func TestAcquisitionOverlapIsHalfOpen(t *testing.T) {
	reads, _ := seeded(t, 1)

	// The fixture acquisition runs [bucket, bucket+8s).
	for _, tc := range []struct {
		name       string
		start, end time.Time
		want       int
	}{
		{"contains it", bucket.Add(-time.Hour), bucket.Add(time.Hour), 1},
		{"starts exactly where it ends", bucket.Add(8 * time.Second), bucket.Add(time.Hour), 0},
		{"ends exactly where it starts", bucket.Add(-time.Hour), bucket, 0},
		{"overlaps the first second", bucket.Add(-time.Hour), bucket.Add(time.Second), 1},
		{"entirely after", bucket.Add(time.Hour), bucket.Add(2 * time.Hour), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := reads.Acquisitions(t.Context(), port.AcquisitionQuery{
				WindowStart: tc.start, WindowEnd: tc.end,
			})
			if err != nil {
				t.Fatalf("acquisitions: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("got %d acquisitions, want %d", len(got), tc.want)
			}
		})
	}
}

// TestSupersededAcquisitionsAreExcludedByDefault is the read-side half of
// ADR-0012. The rows stay; the default timeline does not show them.
func TestSupersededAcquisitionsAreExcludedByDefault(t *testing.T) {
	reads, _ := seeded(t, 1, 2)
	wide := port.AcquisitionQuery{
		WindowStart: bucket.Add(-time.Hour), WindowEnd: bucket.Add(time.Hour),
	}

	live, _, err := reads.Acquisitions(t.Context(), wide)
	if err != nil {
		t.Fatalf("acquisitions: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("default view has %d acquisitions, want 1 — superseded rows are leaking in", len(live))
	}

	wide.Statuses = []string{"ACTIVE", "SUPERSEDED"}
	all, _, err := reads.Acquisitions(t.Context(), wide)
	if err != nil {
		t.Fatalf("acquisitions: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("asking for SUPERSEDED gave %d, want 2 — the history is unreachable", len(all))
	}
}

// TestOpportunitiesReportWhichOneWon is what makes the plan explainable. A
// winner with no losers next to it explains nothing about why it won.
func TestOpportunitiesReportWhichOneWon(t *testing.T) {
	reads, f := seeded(t, 1)

	opps, _, err := reads.RequestOpportunities(t.Context(), f.requestID)
	if err != nil {
		t.Fatalf("opportunities: %v", err)
	}
	if len(opps) != 1 {
		t.Fatalf("got %d opportunities, want 1", len(opps))
	}
	if !opps[0].Won {
		t.Error("the opportunity that was scheduled is not marked won")
	}
}

// TestAMissIsErrNotFoundAndNotAZeroValue stops a caller mistaking an empty
// struct for a real answer.
func TestAMissIsErrNotFoundAndNotAZeroValue(t *testing.T) {
	reads, f := seeded(t, 1)

	if _, err := reads.Plan(t.Context(), "NO-SUCH-SAT", bucket, nil); !errors.Is(err, port.ErrNotFound) {
		t.Errorf("missing plan gave %v, want ErrNotFound", err)
	}
	if _, err := reads.Request(t.Context(), "99999999-9999-9999-9999-999999999999"); !errors.Is(err, port.ErrNotFound) {
		t.Errorf("missing request gave %v, want ErrNotFound", err)
	}
	// And the ones that exist must not.
	if _, err := reads.Request(t.Context(), f.requestID); err != nil {
		t.Errorf("an existing request was reported missing: %v", err)
	}
}

// TestLosingARoundKeepsTheReasonAndKeepsCompeting pins two things that are
// easy to get backwards.
//
// There is no UNFULFILLED state, deliberately: ROUND_LOST returns a request to
// AWAITING_PLANNING so it ages, gains fairness weight, and competes in the next
// round. A terminal LOST state would make the fairness model impossible, and
// the projection must not invent one.
//
// The explanation is kept regardless. "No" without a reason is not an answer,
// and the planner's structured shortfall is the only thing that can supply one.
func TestLosingARoundKeepsTheReasonAndKeepsCompeting(t *testing.T) {
	p := pool(t)
	pr := postgres.NewProjection(p)
	f := newFixture()
	if err := pr.Reset(t.Context()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	t.Cleanup(func() {
		if err := pr.Reset(context.Background()); err != nil {
			t.Errorf("reset failed; later tests will see this test's rows: %v", err)
		}
	})

	// Received, then feasible, which is what puts it in the running.
	if err := pr.ProjectRequestReceived(t.Context(), f.received()); err != nil {
		t.Fatalf("request: %v", err)
	}
	if err := pr.ProjectOpportunities(t.Context(), f.opportunities()); err != nil {
		t.Fatalf("opportunities: %v", err)
	}
	reason := []byte(`{"reason":"OUTBID","shortfall_credits":120}`)
	if err := pr.ProjectUnfulfilled(t.Context(), port.RequestUnfulfilled{
		EventAt: epoch.Add(2 * time.Minute), RequestID: f.requestID, ReasonJSON: reason,
	}); err != nil {
		t.Fatalf("unfulfilled: %v", err)
	}

	view, err := postgres.NewReads(p).Request(t.Context(), f.requestID)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if view.State != "AWAITING_PLANNING" {
		t.Errorf("state = %q, want AWAITING_PLANNING — losing a round is not terminal", view.State)
	}
	var got map[string]any
	if err := json.Unmarshal(view.UnfulfilmentJSON, &got); err != nil {
		t.Fatalf("unfulfilment is not JSON: %v (%s)", err, view.UnfulfilmentJSON)
	}
	if got["reason"] != "OUTBID" {
		t.Errorf("reason = %v; the explanation did not survive the fold", got["reason"])
	}
	// A number, not a string. "Requests unfulfilled by reason" is a domain
	// metric and a bid suggestion is only possible if the shortfall is numeric.
	if got["shortfall_credits"] != 120.0 {
		t.Errorf("shortfall_credits = %#v, want a number", got["shortfall_credits"])
	}
}

// TestAPlannedRequestThatLosesGoesBackToCompeting is the supersede path from
// ADR-0012, seen from the request's side: the acquisition is retained as
// history, and the request re-enters the queue rather than being stranded in
// PLANNED with no acquisition.
func TestAPlannedRequestThatLosesGoesBackToCompeting(t *testing.T) {
	p := pool(t)
	pr := postgres.NewProjection(p)
	f := newFixture()
	if err := pr.Reset(t.Context()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	t.Cleanup(func() {
		if err := pr.Reset(context.Background()); err != nil {
			t.Errorf("reset failed; later tests will see this test's rows: %v", err)
		}
	})

	for _, step := range []func() error{
		func() error { return pr.ProjectRequestReceived(t.Context(), f.received()) },
		func() error { return pr.ProjectOpportunities(t.Context(), f.opportunities()) },
		func() error { return pr.ProjectPlanCommitted(t.Context(), f.plan(1)) },
	} {
		if err := step(); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}

	reads := postgres.NewReads(p)
	planned, err := reads.Request(t.Context(), f.requestID)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if planned.State != "PLANNED" {
		t.Fatalf("state = %q after a plan committed, want PLANNED", planned.State)
	}

	if lostErr := pr.ProjectUnfulfilled(t.Context(), port.RequestUnfulfilled{
		EventAt:    epoch.Add(10 * time.Minute),
		RequestID:  f.requestID,
		ReasonJSON: []byte(`{"reason":"SUPERSEDED_BY_HIGHER_BID"}`),
	}); lostErr != nil {
		t.Fatalf("unfulfilled: %v", lostErr)
	}

	lost, err := reads.Request(t.Context(), f.requestID)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if lost.State != "AWAITING_PLANNING" {
		t.Errorf("state = %q, want AWAITING_PLANNING — a superseded request must compete again", lost.State)
	}
}

// TestStalenessComesFromTheNewestCursor pins the aggregate.
//
// The maximum across streams, not the minimum: a stream that is simply quiet
// would otherwise report the whole projection as hours behind.
func TestStalenessComesFromTheNewestCursor(t *testing.T) {
	reads, _ := seeded(t, 1)

	_, cursor, err := reads.Plans(t.Context(), port.PlanQuery{})
	if err != nil {
		t.Fatalf("plans: %v", err)
	}
	// seeded advanced PLANNING to epoch+1h, later than any folded event.
	want := epoch.Add(time.Hour)
	if !cursor.LastEventAt.Equal(want) {
		t.Fatalf("cursor = %s, want the newest across streams (%s)", cursor.LastEventAt, want)
	}
}

// TestTheLimitIsBoundedRegardlessOfWhatIsAsked keeps one query from pulling the
// whole table into memory.
func TestTheLimitIsBoundedRegardlessOfWhatIsAsked(t *testing.T) {
	reads, f := seeded(t, 1, 2)

	for _, limit := range []int{0, -1, 100000} {
		got, _, err := reads.Plans(t.Context(), port.PlanQuery{
			SatelliteID: f.satelliteID, IncludeSuperseded: true, Limit: limit,
		})
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if len(got) != 2 {
			t.Fatalf("limit %d returned %d rows, want the 2 that exist", limit, len(got))
		}
	}

	// And an explicit small limit is honoured.
	got, _, err := reads.Plans(t.Context(), port.PlanQuery{
		SatelliteID: f.satelliteID, IncludeSuperseded: true, Limit: 1,
	})
	if err != nil {
		t.Fatalf("limit 1: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("limit=1 returned %d rows", len(got))
	}
}

// TestBucketFiltersAreInclusiveAtBothEnds documents the comparison, because
// >= vs > on a bucket boundary is exactly the sort of thing that is silently
// off by one whole bucket.
func TestBucketFiltersAreInclusiveAtBothEnds(t *testing.T) {
	reads, f := seeded(t, 1)
	exact := bucket

	got, _, err := reads.Plans(t.Context(), port.PlanQuery{
		SatelliteID:      f.satelliteID,
		BucketStartAfter: &exact, BucketStartBefore: &exact,
	})
	if err != nil {
		t.Fatalf("plans: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("a bucket filtered to its own start returned %d plans, want 1", len(got))
	}
}
