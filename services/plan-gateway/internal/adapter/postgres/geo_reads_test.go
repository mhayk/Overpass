package postgres_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
)

// These run against a real Postgres because the thing under test IS the SQL.
//
// The interesting mistakes in both queries are things a fake cannot have: a
// range operator inclusive at the wrong end, and a three-state boolean filter
// written as `won = $4` — which, with a NULL parameter, matches NOTHING rather
// than everything and would silently return an empty contention layer.

func TestTargetsReturnsTheRequestTargetWithItsWindow(t *testing.T) {
	reads, f := seeded(t)

	got, _, err := reads.Targets(t.Context(), port.TargetQuery{
		WindowStart: bucket.Add(-time.Hour),
		WindowEnd:   bucket.Add(12 * time.Hour),
	})
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("targets = %d, want 1", len(got))
	}

	target := got[0]
	if target.RequestID != f.requestID {
		t.Errorf("request_id = %s, want %s", target.RequestID, f.requestID)
	}
	if !target.WindowStart.Equal(bucket) {
		t.Errorf("window start = %s, want %s", target.WindowStart, bucket)
	}
	// The geometry is whatever PostGIS serialised, decoded only to prove it is
	// valid GeoJSON rather than to inspect it — re-encoding coordinates is the
	// one thing this path must never do.
	var geometry map[string]any
	if err := json.Unmarshal(target.GeoJSON, &geometry); err != nil {
		t.Fatalf("target geometry is not valid GeoJSON: %v\n%s", err, target.GeoJSON)
	}
	if geometry["type"] != "Point" {
		t.Errorf("geometry type = %v, want Point", geometry["type"])
	}
}

// The window filters on the REQUEST'S window, and misses outside it.
//
// Half-open at the end, matching every other range in this service: a request
// whose window starts exactly when the query's ends is not in the query.
func TestTargetsWindowOverlapIsHalfOpen(t *testing.T) {
	reads, _ := seeded(t)

	// The fixture's request window is [bucket, bucket+6h).
	inside, _, err := reads.Targets(t.Context(), port.TargetQuery{
		WindowStart: bucket.Add(5 * time.Hour),
		WindowEnd:   bucket.Add(7 * time.Hour),
	})
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(inside) != 1 {
		t.Errorf("overlapping window returned %d, want 1", len(inside))
	}

	after, _, err := reads.Targets(t.Context(), port.TargetQuery{
		WindowStart: bucket.Add(6 * time.Hour),
		WindowEnd:   bucket.Add(9 * time.Hour),
	})
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("a window starting where the request's ends returned %d, want 0", len(after))
	}
}

// An unknown state returns nothing rather than everything.
//
// The handler refuses unknown states, but the query must not be the thing that
// makes a typo harmless: `state = ”` meaning "no filter" is deliberate, and
// any other value must actually filter.
func TestTargetsStateFilterActuallyFilters(t *testing.T) {
	reads, _ := seeded(t)
	window := port.TargetQuery{
		WindowStart: bucket.Add(-time.Hour),
		WindowEnd:   bucket.Add(12 * time.Hour),
	}

	unfiltered, _, err := reads.Targets(t.Context(), window)
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(unfiltered) == 0 {
		t.Fatal("fixture produced no targets")
	}

	filtered := window
	filtered.State = "CANCELLED"
	got, _, err := reads.Targets(t.Context(), filtered)
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("state=CANCELLED returned %d rows for a request that is not cancelled", len(got))
	}
}

// THE ONE THIS FILE EXISTS FOR.
//
// A nil Awarded must mean BOTH. Written the obvious way — `won = $4` — a NULL
// parameter matches nothing in SQL, so the contention layer would come back
// empty and look like a system with no contention at all. That is a wrong
// answer that reads as good news, which is the worst kind.
func TestANilAwardedFilterReturnsBothWonAndLost(t *testing.T) {
	reads, _ := seeded(t, 1)
	window := port.OpportunityFootprintQuery{
		WindowStart: bucket.Add(-time.Hour),
		WindowEnd:   bucket.Add(12 * time.Hour),
	}

	both, _, err := reads.OpportunityFootprints(t.Context(), window)
	if err != nil {
		t.Fatalf("footprints: %v", err)
	}
	if len(both) == 0 {
		t.Fatal("a nil awarded filter returned nothing; NULL in SQL matches nothing, " +
			"and the contention layer would read as a system with no contention")
	}
}

func TestTheAwardedFilterSelectsWinnersAndLosersSeparately(t *testing.T) {
	// Version 1 awards the fixture's single opportunity, so `won` is true.
	reads, _ := seeded(t, 1)
	window := port.OpportunityFootprintQuery{
		WindowStart: bucket.Add(-time.Hour),
		WindowEnd:   bucket.Add(12 * time.Hour),
	}

	yes, no := true, false

	won := window
	won.Awarded = &yes
	winners, _, err := reads.OpportunityFootprints(t.Context(), won)
	if err != nil {
		t.Fatalf("footprints: %v", err)
	}
	for _, w := range winners {
		if !w.Awarded {
			t.Errorf("awarded=true returned an unawarded opportunity %s", w.OpportunityID)
		}
	}

	lost := window
	lost.Awarded = &no
	losers, _, err := reads.OpportunityFootprints(t.Context(), lost)
	if err != nil {
		t.Fatalf("footprints: %v", err)
	}
	for _, l := range losers {
		if l.Awarded {
			t.Errorf("awarded=false returned an awarded opportunity %s", l.OpportunityID)
		}
	}

	if len(winners)+len(losers) == 0 {
		t.Fatal("neither filter returned anything; the fixture projects one opportunity")
	}
}

// The satellite filter narrows, and an empty one does not.
func TestOpportunityFootprintsSatelliteFilter(t *testing.T) {
	reads, f := seeded(t)
	window := port.OpportunityFootprintQuery{
		WindowStart: bucket.Add(-time.Hour),
		WindowEnd:   bucket.Add(12 * time.Hour),
	}

	matching := window
	matching.SatelliteID = f.satelliteID
	got, _, err := reads.OpportunityFootprints(t.Context(), matching)
	if err != nil {
		t.Fatalf("footprints: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("filtering to %s returned nothing", f.satelliteID)
	}

	other := window
	other.SatelliteID = "NOT-A-SATELLITE"
	got, _, err = reads.OpportunityFootprints(t.Context(), other)
	if err != nil {
		t.Fatalf("footprints: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("filtering to an unknown satellite returned %d rows", len(got))
	}
}

// Both queries carry the geometry PostGIS produced, as bytes.
func TestOpportunityFootprintsCarryValidGeoJSON(t *testing.T) {
	reads, _ := seeded(t)

	got, _, err := reads.OpportunityFootprints(t.Context(), port.OpportunityFootprintQuery{
		WindowStart: bucket.Add(-time.Hour),
		WindowEnd:   bucket.Add(12 * time.Hour),
	})
	if err != nil {
		t.Fatalf("footprints: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("fixture produced no footprints")
	}

	var geometry map[string]any
	if err := json.Unmarshal(got[0].GeoJSON, &geometry); err != nil {
		t.Fatalf("footprint is not valid GeoJSON: %v\n%s", err, got[0].GeoJSON)
	}
	if geometry["type"] != "Polygon" {
		t.Errorf("geometry type = %v, want Polygon", geometry["type"])
	}
}

// Staleness comes from the cursor, so a client can tell a quiet region from a
// stale read model.
func TestGeoReadsReportStaleness(t *testing.T) {
	reads, _ := seeded(t)
	window := port.TargetQuery{
		WindowStart: bucket.Add(-time.Hour),
		WindowEnd:   bucket.Add(12 * time.Hour),
	}

	_, cursor, err := reads.Targets(t.Context(), window)
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if cursor.LastEventAt.IsZero() {
		t.Error("no cursor; every geo document must be able to state its staleness")
	}
}
