package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The de-confliction invariant, exercised as raw SQL against a real Postgres.
//
// Through SQL and not through the planner deliberately. The claim is that a
// satellite cannot be double-booked NO MATTER WHAT writes to the table — a
// buggy planner, a manual fix at 3am, a future service nobody has written yet.
// A test that goes through the planner tests the planner; only a test that
// tries to write the bad row directly tests the constraint.

const overlapConstraint = "acquisitions_no_overlap_live"

const testCustomerID = "itest-customer"

// testSatelliteID produces an id the reference table will accept.
//
// satellites_id_format is ^[A-Z0-9][A-Z0-9_-]{0,31}$ — uppercase only, and uuid
// hex is lowercase. Worth a helper rather than a ToUpper at each call site,
// because the failure is a check-constraint violation during setup that reads
// like the test is broken rather than the id is.
func testSatelliteID(tag string) string {
	return strings.ToUpper("ITEST-" + tag + "-" + uuid.NewString()[:6])
}

// seedPlanningRow inserts the reference and plan rows an acquisition needs.
//
// The full foreign-key chain, not a stub: an acquisition references a plan, a
// satellite and a customer, and cutting any of them would mean testing the
// constraint against a table shape the planner will never write.
func seedPlanningRow(t *testing.T, tx pgx.Tx, satelliteID string) string {
	t.Helper()
	ctx := context.Background()

	if _, err := tx.Exec(ctx, `
		INSERT INTO reference.satellites
			(satellite_id, norad_id, display_name, sensor_modes, duty_cycle_budget_s)
		VALUES ($1, $2, $1, '{}'::jsonb, 600)
		ON CONFLICT (satellite_id) DO NOTHING
	`, satelliteID, noradFor(satelliteID)); err != nil {
		t.Fatalf("seeding satellite: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO reference.customers (customer_id, display_name)
		VALUES ($1, 'Integration Test Customer')
		ON CONFLICT (customer_id) DO NOTHING
	`, testCustomerID); err != nil {
		t.Fatalf("seeding customer: %v", err)
	}

	planID := uuid.NewString()
	if _, err := tx.Exec(ctx, `
		INSERT INTO planning.collection_plans
			(plan_id, round_id, satellite_id, bucket, plan_version, policy, committed_at)
		VALUES ($1, $2, $3, tstzrange($4, $5, '[)'), 1, 'GREEDY_BY_BID', $4)
	`, planID, uuid.NewString(), satelliteID,
		time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("seeding plan: %v", err)
	}
	return planID
}

// noradFor derives a stable, unique NORAD id from the satellite name. The
// column is UNIQUE, and a fixed number would collide across cases.
func noradFor(satelliteID string) int {
	sum := 0
	for _, r := range satelliteID {
		sum = sum*31 + int(r)
	}
	if sum < 0 {
		sum = -sum
	}
	return 10000 + sum%80000
}

func insertAcquisition(
	ctx context.Context, tx pgx.Tx, planID, satelliteID string, start, end time.Time, status string,
) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO planning.acquisitions
			(acquisition_id, plan_id, request_id, opportunity_id, customer_id, satellite_id,
			 mode, acq_window, geometry, footprint, duty_cycle_cost_s,
			 awarded_value_credits, status, superseded_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'STRIPMAP', tstzrange($7, $8, '[)'),
		        '{}'::jsonb,
		        ST_SetSRID(ST_GeomFromGeoJSON('{"type":"Polygon","coordinates":[[[4,51],[4,52],[5,52],[5,51],[4,51]]]}'), 4326),
		        30, 100, $9, $10)
	`, uuid.NewString(), planID, uuid.NewString(), uuid.NewString(), testCustomerID,
		satelliteID, start, end, status, supersededAt(status))
	return err
}

// supersededAt keeps status and timestamp in step.
//
// acquisitions_superseded_at_agrees is an equivalence, not an implication:
// SUPERSEDED requires a timestamp AND any other status requires its absence. A
// row carrying a superseded_at while still ACTIVE is as rejected as one without.
func supersededAt(status string) any {
	if status == "SUPERSEDED" {
		return time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC)
	}
	return nil
}

// TestOverlappingAcquisitionsAreRejectedByTheDatabase is the invariant.
func TestOverlappingAcquisitionsAreRejectedByTheDatabase(t *testing.T) {
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name        string
		offset      time.Duration
		duration    time.Duration
		wantRefused bool
	}{
		{"identical window", 0, 30 * time.Second, true},
		{"starts inside the first", 10 * time.Second, 30 * time.Second, true},
		{"ends inside the first", -10 * time.Second, 30 * time.Second, true},
		{"strictly contains the first", -10 * time.Second, 60 * time.Second, true},
		// Half-open ranges: touching at the boundary is not overlapping, and
		// this is the case that makes back-to-back acquisitions possible at all.
		{"starts exactly when the first ends", 30 * time.Second, 30 * time.Second, false},
		{"ends exactly when the first starts", -30 * time.Second, 30 * time.Second, false},
		{"comfortably later", 10 * time.Minute, 30 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// One transaction per case, rolled back. The constraint is
			// DEFERRABLE INITIALLY DEFERRED, so the violation surfaces at
			// COMMIT rather than at INSERT — which is the entire reason a plan
			// can rewrite a whole bucket without tripping over itself midway.
			// A satellite per case. The cases that legitimately commit leave
			// their plan row behind, and collection_plans_unique_version is
			// (satellite_id, bucket, plan_version) — sharing one satellite made
			// the later cases fail on setup rather than on the thing under test.
			ctx := t.Context()
			satelliteID := testSatelliteID("EX")
			tx, err := env.pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback(ctx) //nolint:errcheck // every case rolls back

			planID := seedPlanningRow(t, tx, satelliteID)
			if firstErr := insertAcquisition(ctx, tx, planID, satelliteID, base, base.Add(30*time.Second), "ACTIVE"); firstErr != nil {
				t.Fatalf("first acquisition: %v", firstErr)
			}

			start := base.Add(tc.offset)
			err = insertAcquisition(ctx, tx, planID, satelliteID, start, start.Add(tc.duration), "ACTIVE")
			if err == nil {
				// Deferred: the INSERT succeeds and COMMIT is what refuses.
				err = tx.Commit(ctx)
			}

			refused := err != nil && strings.Contains(err.Error(), overlapConstraint)
			switch {
			case tc.wantRefused && !refused:
				t.Fatalf("the database accepted an overlapping acquisition (err=%v)", err)
			case !tc.wantRefused && err != nil:
				t.Fatalf("a non-overlapping acquisition was refused: %v", err)
			}
		})
	}
}

// TestASupersededAcquisitionDoesNotBlockItsReplacement is ADR-0012's whole
// point, and the reason the constraint is PARTIAL.
//
// Re-planning writes a new acquisition over the same window. If superseded rows
// still occupied the exclusion, the second plan for a bucket could never be
// committed and the history would have to be deleted to make room — which is
// exactly the history the ADR exists to keep.
func TestASupersededAcquisitionDoesNotBlockItsReplacement(t *testing.T) {
	ctx := t.Context()
	satelliteID := testSatelliteID("SUP")
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // this test does not keep its rows

	planID := seedPlanningRow(t, tx, satelliteID)
	if err := insertAcquisition(ctx, tx, planID, satelliteID, base, base.Add(30*time.Second), "SUPERSEDED"); err != nil {
		t.Fatalf("superseded acquisition: %v", err)
	}
	if err := insertAcquisition(ctx, tx, planID, satelliteID, base, base.Add(30*time.Second), "ACTIVE"); err != nil {
		t.Fatalf("the replacement was refused over a superseded row: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestTwoLiveStatusesStillConflict guards the predicate itself.
//
// The exclusion is WHERE status <> 'SUPERSEDED', so every other status is live.
// Writing it as `WHERE status = 'ACTIVE'` would look equivalent and would let an
// EXECUTED acquisition be double-booked — a satellite cannot re-point to honour
// a booking it has already flown.
func TestTwoLiveStatusesStillConflict(t *testing.T) {
	ctx := t.Context()
	satelliteID := testSatelliteID("LIVE")
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rolled back either way

	planID := seedPlanningRow(t, tx, satelliteID)
	if execErr := insertAcquisition(ctx, tx, planID, satelliteID, base, base.Add(30*time.Second), "EXECUTED"); execErr != nil {
		t.Fatalf("executed acquisition: %v", execErr)
	}
	err = insertAcquisition(ctx, tx, planID, satelliteID, base, base.Add(30*time.Second), "ACTIVE")
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err == nil || !strings.Contains(err.Error(), overlapConstraint) {
		t.Fatalf("an ACTIVE acquisition was booked over an EXECUTED one (err=%v)", err)
	}
}

// TestDifferentSatellitesDoNotConflict is the guard on the guard.
//
// If the exclusion were missing its satellite_id term it would reject every
// simultaneous acquisition across the whole constellation, and every test above
// would still pass — they only ever use one satellite.
func TestDifferentSatellitesDoNotConflict(t *testing.T) {
	ctx := t.Context()
	base := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rolled back

	for _, id := range []string{testSatelliteID("A"), testSatelliteID("B")} {
		planID := seedPlanningRow(t, tx, id)
		if err := insertAcquisition(ctx, tx, planID, id, base, base.Add(30*time.Second), "ACTIVE"); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("two satellites imaging at once were refused: %v", err)
	}
}
