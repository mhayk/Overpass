package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/planner/internal/adapter/postgres"
	"github.com/mhayk/overpass/services/planner/internal/domain"
	"github.com/mhayk/overpass/services/planner/internal/port"
)

// "Configurable per satellite" is an acceptance criterion, and storing the
// columns does not demonstrate it. This does: two satellites, different
// agility, different transition costs for the identical manoeuvre.
func TestAgilityIsPerSatellite(t *testing.T) {
	p := pool(t)
	satellites := postgres.NewSatellites(p)
	ctx := context.Background()

	nimble := fmt.Sprintf("SAT-AG%d", time.Now().UnixNano()%100000)
	sluggish := fmt.Sprintf("SAT-AH%d", time.Now().UnixNano()%100000+1)
	seedSatellite(t, p, nimble)
	seedSatellite(t, p, sluggish)

	if _, err := p.Exec(ctx, `
		UPDATE reference.satellites
		SET slew_rate_deg_s = 4.0, settle_time_s = 1.0, mode_transition_s = 0, max_roll_deg = 50
		WHERE satellite_id = $1`, nimble); err != nil {
		t.Fatalf("configuring %s: %v", nimble, err)
	}
	if _, err := p.Exec(ctx, `
		UPDATE reference.satellites
		SET slew_rate_deg_s = 0.5, settle_time_s = 20.0, mode_transition_s = 12, max_roll_deg = 30
		WHERE satellite_id = $1`, sluggish); err != nil {
		t.Fatalf("configuring %s: %v", sluggish, err)
	}

	fast, err := satellites.Agility(ctx, nimble)
	if err != nil {
		t.Fatalf("reading %s: %v", nimble, err)
	}
	slow, err := satellites.Agility(ctx, sluggish)
	if err != nil {
		t.Fatalf("reading %s: %v", sluggish, err)
	}

	if fast.SlewRateDegS != 4.0 || slow.SlewRateDegS != 0.5 {
		t.Errorf("rates came back as %v and %v", fast.SlewRateDegS, slow.SlewRateDegS)
	}

	// The same 20-degree manoeuvre, two spacecraft.
	from := domain.Attitude{RollDeg: 0, Mode: "STRIPMAP"}
	to := domain.Attitude{RollDeg: 20, Mode: "STRIPMAP"}
	if want := 6 * time.Second; fast.SlewTime(from, to) != want { // 20/4 + 1
		t.Errorf("nimble transition = %s, want %s", fast.SlewTime(from, to), want)
	}
	if want := 60 * time.Second; slow.SlewTime(from, to) != want { // 20/0.5 + 20
		t.Errorf("sluggish transition = %s, want %s", slow.SlewTime(from, to), want)
	}

	// And the roll authority differs, which is what decides whether a candidate
	// is flyable at all.
	steep := domain.Attitude{RollDeg: 40}
	if !fast.WithinRollAuthority(steep) {
		t.Error("the 50-degree spacecraft refused a 40-degree roll")
	}
	if slow.WithinRollAuthority(steep) {
		t.Error("the 30-degree spacecraft accepted a 40-degree roll; the plan would not be flyable")
	}
}

// An unknown satellite is a DIFFERENT fact from a satellite with default
// parameters. A planner that conflated them would schedule against a spacecraft
// that does not exist.
func TestUnknownSatelliteIsNotFound(t *testing.T) {
	p := pool(t)
	_, err := postgres.NewSatellites(p).Agility(context.Background(), "SAT-NEVER-SEEN")

	if err == nil {
		t.Fatal("an unknown satellite returned agility")
	}
	if !errors.Is(err, port.ErrNotFound) {
		t.Errorf("error does not wrap port.ErrNotFound, so a caller cannot tell it from a database failure: %v", err)
	}
}

// A seeded satellite that nobody configured must still be usable, or every
// existing row becomes unschedulable the moment the model lands.
func TestDefaultsAreUsable(t *testing.T) {
	p := pool(t)
	satellite := fmt.Sprintf("SAT-AD%d", time.Now().UnixNano()%100000)
	seedSatellite(t, p, satellite)

	got, err := postgres.NewSatellites(p).Agility(context.Background(), satellite)
	if err != nil {
		t.Fatalf("a satellite on the migration defaults was refused: %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("the defaults do not pass domain validation: %v", err)
	}
	at := domain.Attitude{RollDeg: 10, Mode: "STRIPMAP"}
	if got.SlewTime(at, at) != got.SettlingFloor() {
		t.Error("the defaults do not satisfy the settling-floor property")
	}
}
