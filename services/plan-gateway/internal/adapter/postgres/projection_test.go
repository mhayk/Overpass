package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mhayk/overpass/services/plan-gateway/internal/adapter/postgres"
	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
)

// The replay test is the reason this file exists.
//
// A read model that can be rebuilt from the log is a read model whose bugs are
// fixable: change the projection, replay, done. One that cannot is one whose
// bugs are permanent, and every fix becomes a migration written by hand against
// production data.
//
// The property is stronger than "replay works": folding the same events in a
// DIFFERENT order must reach the same state, because that is what actually
// happens under redelivery.

func dsn(t *testing.T) string {
	t.Helper()
	v := os.Getenv("OVERPASS_TEST_DSN")
	if v == "" {
		t.Skip("set OVERPASS_TEST_DSN to run the projection tests")
	}
	return v
}

func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(t.Context(), dsn(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// GeoJSON, because that is what the contracts publish and what the projection
// now hands straight to ST_GeomFromGeoJSON. Longitude first.
var (
	pointGeoJSON = []byte(`{"type":"Point","coordinates":[4.412345678,51.987654321]}`)
	// Nine decimal places, which is what a propagator actually produces and what
	// PostGIS stores. The fixture used whole numbers first, and that made the
	// coordinate-precision test pass with the precision argument REMOVED —
	// there were no decimals to truncate, so it asserted nothing.
	polygonGeoJSON = []byte(`{"type":"Polygon","coordinates":` +
		`[[[4.123456789,51.987654321],[4.223456789,51.987654321],` +
		`[4.223456789,52.087654321],[4.123456789,52.087654321],` +
		`[4.123456789,51.987654321]]]}`)
)

var (
	epoch  = time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC).UTC()
	bucket = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC).UTC()

	// Two element sets, a day apart. Which of them a track came from is what
	// decides whether it survives a competing track for the same instants —
	// arrival order is explicitly not what decides it.
	staleTLE = time.Date(2026, 8, 6, 21, 41, 12, 0, time.UTC).UTC()
	freshTLE = time.Date(2026, 8, 7, 21, 44, 3, 0, time.UTC).UTC()
)

// fixture is a deterministic set of events. The ids are fixed rather than
// random, because a snapshot of state that contains random ids cannot be
// compared with anything.
type fixture struct {
	requestID     string
	opportunityID string
	planID        string
	acquisitionID string
	satelliteID   string
}

func newFixture() fixture {
	return fixture{
		requestID:     "11111111-1111-1111-1111-111111111111",
		opportunityID: "22222222-2222-2222-2222-222222222222",
		planID:        "33333333-3333-3333-3333-333333333333",
		acquisitionID: "44444444-4444-4444-4444-444444444444",
		satelliteID:   "REPLAY-SAT",
	}
}

func (f fixture) received() port.RequestReceived {
	return port.RequestReceived{
		EventAt: epoch, RequestID: f.requestID, CustomerID: "replay-customer",
		TargetName:  "replay target",
		WindowStart: bucket, WindowEnd: bucket.Add(6 * time.Hour),
		TargetGeoJSON: pointGeoJSON,
	}
}

func (f fixture) opportunities() port.OpportunitiesComputed {
	return port.OpportunitiesComputed{
		EventAt: epoch.Add(time.Minute), RequestID: f.requestID,
		Opportunities: []port.Opportunity{{
			OpportunityID: f.opportunityID, SatelliteID: f.satelliteID, Mode: "STRIPMAP",
			AccessStart: bucket, AccessEnd: bucket.Add(10 * time.Minute),
			AcquisitionDurationS: 8, QualityScore: 0.87,
			FootprintGeoJSON: polygonGeoJSON,
		}},
	}
}

func (f fixture) plan(version int) port.PlanCommitted {
	opp := f.opportunityID
	return port.PlanCommitted{
		EventAt:     epoch.Add(time.Duration(version) * time.Minute * 2),
		PlanID:      fmt.Sprintf("%s%d", f.planID[:len(f.planID)-1], version),
		SatelliteID: f.satelliteID,
		BucketStart: bucket, BucketEnd: bucket.Add(3 * time.Hour),
		PlanVersion: version, Policy: "GreedyByBid",
		MetricsJSON: []byte(`{"requests_fulfilled":1}`),
		CommittedAt: bucket,
		Acquisitions: []port.Acquisition{{
			AcquisitionID: fmt.Sprintf("%s%d", f.acquisitionID[:len(f.acquisitionID)-1], version),
			RequestID:     f.requestID, OpportunityID: &opp, CustomerID: "replay-customer",
			Mode:        "STRIPMAP",
			WindowStart: bucket, WindowEnd: bucket.Add(8 * time.Second),
			FootprintGeoJSON:    polygonGeoJSON,
			AwardedValueCredits: 500,
		}},
	}
}

// ephemeris is one satellite's track over the fixture's bucket.
//
// `tleEpoch` and `longitude` are parameters because the interesting property is
// which of two tracks for the SAME instants survives, and that is decided by
// the element set behind them rather than by arrival order.
func (f fixture) ephemeris(at time.Time, tleEpoch time.Time, longitude float64) port.EphemerisComputed {
	e := port.EphemerisComputed{
		EventAt: at, SatelliteID: f.satelliteID, TleEpoch: tleEpoch,
	}
	for i := range 6 {
		e.Samples = append(e.Samples, port.EphemerisSample{
			At:           bucket.Add(time.Duration(i*10) * time.Second),
			LongitudeDeg: longitude + float64(i)*0.041234,
			LatitudeDeg:  51.9 + float64(i)*0.663211,
			AltitudeM:    693412.8,
		})
	}
	return e
}

// snapshot digests every projected row.
//
// Ordered explicitly, and excluding updated_at. updated_at is now() and would
// differ between two runs of the same events — including it would make this
// test assert that the clock did not move, which is not the property anyone
// wants.
func snapshot(t *testing.T, p *pgxpool.Pool) string {
	t.Helper()
	queries := []string{
		`SELECT request_id::text, customer_id, target_name, state,
		        lower(request_window)::text, upper(request_window)::text,
		        opportunity_count::text, coalesce(unfulfilment::text,''),
		        last_event_at::text, coalesce(ST_AsText(target),'')
		 FROM readmodel.request_views ORDER BY request_id`,
		`SELECT plan_id::text, satellite_id, lower(bucket)::text, plan_version::text,
		        superseded::text, policy, metrics::text, committed_at::text, last_event_at::text
		 FROM readmodel.plan_views ORDER BY satellite_id, lower(bucket), plan_version`,
		`SELECT acquisition_id::text, plan_id::text, request_id::text, satellite_id, mode,
		        lower(acq_window)::text, status, ST_AsText(footprint), awarded_value_credits::text
		 FROM readmodel.acquisition_views ORDER BY acquisition_id`,
		`SELECT opportunity_id::text, request_id::text, satellite_id, mode,
		        quality_score::text, won::text, ST_AsText(footprint)
		 FROM readmodel.opportunity_views ORDER BY opportunity_id`,
		// The ephemeris belongs in the digest for the same reason the others
		// do: the replay and convergence properties are claims about the WHOLE
		// read model, and a table left out of the snapshot is a table those
		// tests do not actually cover.
		`SELECT satellite_id, sample_at::text, longitude_deg::text, latitude_deg::text,
		        altitude_m::text, tle_epoch::text, last_event_at::text
		 FROM readmodel.ephemeris ORDER BY satellite_id, sample_at`,
	}

	digest := sha256.New()
	for _, q := range queries {
		rows, err := p.Query(t.Context(), q)
		if err != nil {
			t.Fatalf("snapshot query: %v", err)
		}
		for rows.Next() {
			values, err := rows.Values()
			if err != nil {
				t.Fatalf("snapshot scan: %v", err)
			}
			for _, v := range values {
				// sha256's Write never returns an error, but discarding it
				// silently is how a snapshot ends up hashing less than it
				// claims to and matching everything.
				if _, err := fmt.Fprintf(digest, "%v|", v); err != nil {
					t.Fatalf("hashing snapshot: %v", err)
				}
			}
			if _, err := digest.Write([]byte("\n")); err != nil {
				t.Fatalf("hashing snapshot: %v", err)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("snapshot rows: %v", err)
		}
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func fold(t *testing.T, pr *postgres.Projection, events []func(context.Context) error) {
	t.Helper()
	for i, apply := range events {
		if err := apply(t.Context()); err != nil {
			t.Fatalf("folding event %d: %v", i, err)
		}
	}
}

func TestAFullReplayProducesIdenticalReadModels(t *testing.T) {
	// The acceptance test. This is what makes a read-model rebuild a routine
	// operation rather than an incident, and it is the practical payoff for
	// choosing a broker with first-class replay.
	p := pool(t)
	pr := postgres.NewProjection(p)
	f := newFixture()

	t.Cleanup(func() {
		if err := pr.Reset(context.Background()); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	events := []func(context.Context) error{
		func(ctx context.Context) error { return pr.ProjectRequestReceived(ctx, f.received()) },
		func(ctx context.Context) error { return pr.ProjectOpportunities(ctx, f.opportunities()) },
		func(ctx context.Context) error {
			return pr.ProjectEphemeris(ctx, f.ephemeris(epoch.Add(30*time.Second), staleTLE, 4.0))
		},
		func(ctx context.Context) error { return pr.ProjectPlanCommitted(ctx, f.plan(1)) },
		func(ctx context.Context) error { return pr.ProjectPlanCommitted(ctx, f.plan(2)) },
		func(ctx context.Context) error {
			return pr.ProjectEphemeris(ctx, f.ephemeris(epoch.Add(90*time.Second), freshTLE, 9.0))
		},
	}

	if err := pr.Reset(t.Context()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	fold(t, pr, events)
	first := snapshot(t, p)

	if err := pr.Reset(t.Context()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	fold(t, pr, events)
	second := snapshot(t, p)

	if first != second {
		t.Fatalf("a replay produced different state:\n  first  %s\n  second %s", first, second)
	}
	if first == snapshotOfNothing(t, p, pr) {
		t.Fatal("the snapshot is empty, so this test compares nothing with nothing")
	}
}

// snapshotOfNothing guards the test above from passing vacuously: two empty
// projections also digest identically.
func snapshotOfNothing(t *testing.T, p *pgxpool.Pool, pr *postgres.Projection) string {
	t.Helper()
	if err := pr.Reset(t.Context()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	return snapshot(t, p)
}

func TestReplayingEventsInADifferentOrderConverges(t *testing.T) {
	// The stronger property, and the one that matters under redelivery. Events
	// do not arrive in the order they happened, so a projection that only works
	// in order works only until the first retry.
	p := pool(t)
	pr := postgres.NewProjection(p)
	f := newFixture()
	t.Cleanup(func() {
		// A failed reset leaves rows behind that the NEXT test will read as
		// its own. Reporting it is the difference between one red test and a
		// suite that fails somewhere else for no visible reason.
		if err := pr.Reset(context.Background()); err != nil {
			t.Errorf("reset failed; later tests will see this test's rows: %v", err)
		}
	})

	inOrder := []func(context.Context) error{
		func(ctx context.Context) error { return pr.ProjectRequestReceived(ctx, f.received()) },
		func(ctx context.Context) error { return pr.ProjectOpportunities(ctx, f.opportunities()) },
		func(ctx context.Context) error {
			return pr.ProjectEphemeris(ctx, f.ephemeris(epoch.Add(30*time.Second), staleTLE, 4.0))
		},
		func(ctx context.Context) error {
			return pr.ProjectEphemeris(ctx, f.ephemeris(epoch.Add(90*time.Second), freshTLE, 9.0))
		},
		func(ctx context.Context) error { return pr.ProjectPlanCommitted(ctx, f.plan(1)) },
		func(ctx context.Context) error { return pr.ProjectPlanCommitted(ctx, f.plan(2)) },
	}
	// The newer plan first, then the older one — exactly what a redelivery of
	// version 1 after version 2 looks like. The ephemeris is reversed too: the
	// fresher element set's track arrives first and the older one behind it,
	// which is the case its tle_epoch guard exists for.
	shuffled := []func(context.Context) error{
		func(ctx context.Context) error { return pr.ProjectRequestReceived(ctx, f.received()) },
		func(ctx context.Context) error { return pr.ProjectPlanCommitted(ctx, f.plan(2)) },
		func(ctx context.Context) error {
			return pr.ProjectEphemeris(ctx, f.ephemeris(epoch.Add(90*time.Second), freshTLE, 9.0))
		},
		func(ctx context.Context) error { return pr.ProjectOpportunities(ctx, f.opportunities()) },
		func(ctx context.Context) error {
			return pr.ProjectEphemeris(ctx, f.ephemeris(epoch.Add(30*time.Second), staleTLE, 4.0))
		},
		func(ctx context.Context) error { return pr.ProjectPlanCommitted(ctx, f.plan(1)) },
	}

	if err := pr.Reset(t.Context()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	fold(t, pr, inOrder)
	ordered := snapshot(t, p)

	if err := pr.Reset(t.Context()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	fold(t, pr, shuffled)
	out := snapshot(t, p)

	if ordered != out {
		t.Fatalf("arrival order changed the result:\n  in order %s\n  shuffled %s", ordered, out)
	}
}

func TestFoldingTheSameEventTwiceChangesNothing(t *testing.T) {
	p := pool(t)
	pr := postgres.NewProjection(p)
	f := newFixture()
	t.Cleanup(func() {
		// A failed reset leaves rows behind that the NEXT test will read as
		// its own. Reporting it is the difference between one red test and a
		// suite that fails somewhere else for no visible reason.
		if err := pr.Reset(context.Background()); err != nil {
			t.Errorf("reset failed; later tests will see this test's rows: %v", err)
		}
	})

	if err := pr.Reset(t.Context()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	once := []func(context.Context) error{
		func(ctx context.Context) error { return pr.ProjectRequestReceived(ctx, f.received()) },
		func(ctx context.Context) error { return pr.ProjectOpportunities(ctx, f.opportunities()) },
	}
	fold(t, pr, once)
	before := snapshot(t, p)

	fold(t, pr, once)
	if after := snapshot(t, p); after != before {
		t.Fatal("folding the same events twice changed the projection")
	}
}

func TestAStalePlanVersionDoesNotBecomeCurrent(t *testing.T) {
	// The contract says a lower plan_version arriving after a higher one is
	// stale and must be dropped. Dropped means "not current", not "discarded" —
	// ADR-0012 retains the history and a read can name it.
	p := pool(t)
	pr := postgres.NewProjection(p)
	f := newFixture()
	t.Cleanup(func() {
		// A failed reset leaves rows behind that the NEXT test will read as
		// its own. Reporting it is the difference between one red test and a
		// suite that fails somewhere else for no visible reason.
		if err := pr.Reset(context.Background()); err != nil {
			t.Errorf("reset failed; later tests will see this test's rows: %v", err)
		}
	})

	if err := pr.Reset(t.Context()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := pr.ProjectPlanCommitted(t.Context(), f.plan(3)); err != nil {
		t.Fatalf("v3: %v", err)
	}
	if err := pr.ProjectPlanCommitted(t.Context(), f.plan(1)); err != nil {
		t.Fatalf("v1: %v", err)
	}

	var currentVersion int
	if err := p.QueryRow(t.Context(), `
		SELECT plan_version FROM readmodel.plan_views
		WHERE satellite_id = $1 AND NOT superseded
	`, f.satelliteID).Scan(&currentVersion); err != nil {
		t.Fatalf("reading current: %v", err)
	}
	if currentVersion != 3 {
		t.Fatalf("the current plan is version %d", currentVersion)
	}

	var kept int
	if err := p.QueryRow(t.Context(),
		`SELECT count(*) FROM readmodel.plan_views WHERE satellite_id = $1`,
		f.satelliteID).Scan(&kept); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if kept != 2 {
		t.Fatalf("%d plan versions retained, want both", kept)
	}
}

func TestTheCursorNeverRewinds(t *testing.T) {
	// A redelivery of an older message must not rewind the cursor, or
	// everything after it is folded again — which is harmless for correctness
	// and ruinous for a rebuild's runtime.
	p := pool(t)
	pr := postgres.NewProjection(p)
	stream := "TEST-" + uuid.NewString()[:8]
	t.Cleanup(func() {
		if _, err := p.Exec(context.Background(),
			`DELETE FROM readmodel.stream_cursors WHERE stream = $1`, stream); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	if err := pr.Advance(t.Context(), stream, 100, epoch.Add(time.Hour)); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := pr.Advance(t.Context(), stream, 50, epoch); err != nil {
		t.Fatalf("rewind attempt: %v", err)
	}

	got, err := pr.Cursor(t.Context(), stream)
	if err != nil {
		t.Fatalf("cursor: %v", err)
	}
	if got.Sequence != 100 {
		t.Fatalf("the cursor rewound to %d", got.Sequence)
	}
}

func TestAnUnknownStreamStartsAtZero(t *testing.T) {
	pr := postgres.NewProjection(pool(t))
	got, err := pr.Cursor(t.Context(), "NEVER-SEEN")
	if err != nil {
		t.Fatalf("cursor: %v", err)
	}
	if got.Sequence != 0 {
		t.Fatalf("an unseen stream reported sequence %d", got.Sequence)
	}
}
