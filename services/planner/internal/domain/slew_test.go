package domain_test

import (
	"errors"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/planner/internal/domain"
)

func agility() domain.Agility {
	return domain.Agility{
		SlewRateDegS:    1.0,
		SettleTimeS:     5.0,
		ModeTransitionS: 3.0,
		MaxRollDeg:      45.0,
	}
}

// ---------------------------------------------------------------------------
// The roll derivation, against an independent oracle
// ---------------------------------------------------------------------------

// TestRollAngleAgainstForwardGeometry is the golden-reference test.
//
// The expected values were produced by the FORWARD geometry — given an orbit
// altitude and an incidence angle, compute the look angle and the slant range —
// while RollAngleDeg goes the other way, from incidence and slant range back to
// the look angle, without ever knowing the altitude. Two different routes
// through the same triangle, so agreement is evidence rather than a tautology.
//
// Reference (spherical Earth, Re = 6371.0088 km):
//
//	alt km  incidence   look/roll deg   slant km
//	   500       15.0       13.885650   516.29275
//	   500       30.0       27.620640   570.51001
//	   500       45.0       40.969028   683.06865
//	   600       25.0       22.720941   655.94262
//	   700       40.0       35.391086   883.93711
//	   400       20.0       18.772734   424.01818
//	   500       35.0       32.129695   599.86414
func TestRollAngleAgainstForwardGeometry(t *testing.T) {
	tests := []struct {
		altKm        float64 // context only; the function never sees it
		incidenceDeg float64
		slantRangeKm float64
		wantRollDeg  float64
	}{
		{500, 15, 516.29275, 13.885650},
		{500, 30, 570.51001, 27.620640},
		{500, 45, 683.06865, 40.969028},
		{600, 25, 655.94262, 22.720941},
		{700, 40, 883.93711, 35.391086},
		{400, 20, 424.01818, 18.772734},
		{500, 35, 599.86414, 32.129695},
	}

	for _, tt := range tests {
		got, err := domain.RollAngleDeg(tt.incidenceDeg, tt.slantRangeKm, domain.LookRight)
		if err != nil {
			t.Errorf("incidence %v° at %v km: %v", tt.incidenceDeg, tt.slantRangeKm, err)
			continue
		}
		// A thousandth of a degree. The inputs are quoted to five decimal
		// places, so a looser tolerance would let a genuinely wrong formula
		// through and a tighter one would fail on the quoting.
		if math.Abs(got-tt.wantRollDeg) > 1e-3 {
			t.Errorf("incidence %v° at %v km (alt %v km): roll = %.6f°, want %.6f°",
				tt.incidenceDeg, tt.slantRangeKm, tt.altKm, got, tt.wantRollDeg)
		}
	}
}

// The look angle is ALWAYS smaller than the incidence angle. That is the
// geometric fact that makes deriving worth doing rather than approximating roll
// by incidence, and it holds for every orbit and every incidence.
func TestLookAngleIsAlwaysSmallerThanIncidence(t *testing.T) {
	for _, tt := range []struct{ incidence, slant float64 }{
		{15, 516.29275}, {30, 570.51001}, {45, 683.06865},
		{25, 655.94262}, {40, 883.93711}, {20, 424.01818},
	} {
		roll, err := domain.RollAngleDeg(tt.incidence, tt.slant, domain.LookRight)
		if err != nil {
			t.Fatalf("incidence %v: %v", tt.incidence, err)
		}
		if roll >= tt.incidence {
			t.Errorf("roll %.4f° is not smaller than incidence %.4f°; the Earth's curvature has gone the wrong way",
				roll, tt.incidence)
		}
	}
}

func TestLookSideSetsTheSign(t *testing.T) {
	right, err := domain.RollAngleDeg(30, 570.51001, domain.LookRight)
	if err != nil {
		t.Fatalf("right: %v", err)
	}
	left, err := domain.RollAngleDeg(30, 570.51001, domain.LookLeft)
	if err != nil {
		t.Fatalf("left: %v", err)
	}

	if right <= 0 {
		t.Errorf("right-looking roll = %v, want positive", right)
	}
	if left >= 0 {
		t.Errorf("left-looking roll = %v, want negative", left)
	}
	if math.Abs(right+left) > 1e-9 {
		t.Errorf("the two sides are not mirror images: %v and %v", right, left)
	}
	// And the sign is not cosmetic: a satellite crossing from a left-looking to
	// a right-looking acquisition slews through nadir and covers BOTH angles.
	across := agility().SlewTime(
		domain.Attitude{RollDeg: left, Mode: "STRIPMAP"},
		domain.Attitude{RollDeg: right, Mode: "STRIPMAP"})
	same := agility().SlewTime(
		domain.Attitude{RollDeg: right, Mode: "STRIPMAP"},
		domain.Attitude{RollDeg: right, Mode: "STRIPMAP"})
	if across <= same {
		t.Error("crossing nadir cost no more than staying put; the look side is being ignored")
	}
}

func TestRollAngleRefusesImpossibleGeometry(t *testing.T) {
	tests := []struct {
		name      string
		incidence float64
		slant     float64
		side      string
	}{
		{"nadir, where the triangle degenerates", 0, 500, domain.LookRight},
		{"negative incidence", -10, 500, domain.LookRight},
		{"horizon-grazing", 90, 500, domain.LookRight},
		{"beyond the horizon", 91, 500, domain.LookRight},
		{"zero slant range", 30, 0, domain.LookRight},
		{"negative slant range", 30, -500, domain.LookRight},
		{"unknown look side", 30, 570, "UP"},
		{"empty look side", 30, 570, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := domain.RollAngleDeg(tt.incidence, tt.slant, tt.side)
			if err == nil {
				t.Fatalf("accepted %s, returning %v", tt.name, got)
			}
			if !errors.Is(err, domain.ErrInvalid) {
				t.Errorf("does not wrap ErrInvalid: %v", err)
			}
		})
	}
}

// A NaN roll would propagate silently into every transition on that satellite
// and make the plan's arithmetic meaningless without failing anything.
func TestNoInputProducesNaN(t *testing.T) {
	for incidence := 0.5; incidence < 90; incidence += 0.5 {
		for slant := 100.0; slant < 3000; slant += 100 {
			roll, err := domain.RollAngleDeg(incidence, slant, domain.LookRight)
			if err != nil {
				continue // refused, which is a fine answer
			}
			if math.IsNaN(roll) || math.IsInf(roll, 0) {
				t.Fatalf("incidence %v° at %v km produced %v", incidence, slant, roll)
			}
		}
	}
}

// The contract makes roll_angle_deg optional. When it IS supplied, the producer
// knows its own spacecraft and the derivation — which assumes a sphere — must
// not override it.
func TestASuppliedRollWins(t *testing.T) {
	supplied := 12.5
	got, err := domain.AttitudeFor(&supplied, 30, 570.51001, domain.LookRight, "STRIPMAP")
	if err != nil {
		t.Fatalf("AttitudeFor: %v", err)
	}
	if got.RollDeg != supplied {
		t.Errorf("roll = %v, want the supplied %v — the derivation overrode the producer", got.RollDeg, supplied)
	}

	derived, err := domain.AttitudeFor(nil, 30, 570.51001, domain.LookRight, "STRIPMAP")
	if err != nil {
		t.Fatalf("AttitudeFor with no roll: %v", err)
	}
	if math.Abs(derived.RollDeg-27.620640) > 1e-3 {
		t.Errorf("derived roll = %v, want ~27.62", derived.RollDeg)
	}
}

// ---------------------------------------------------------------------------
// The transition function
// ---------------------------------------------------------------------------

func TestSlewTimeAgainstHandComputedValues(t *testing.T) {
	// rate 1 deg/s, settle 5 s, mode change 3 s.
	a := agility()

	tests := []struct {
		name string
		from domain.Attitude
		to   domain.Attitude
		want time.Duration
	}{
		{"identical attitude is the settling floor",
			domain.Attitude{RollDeg: 20, Mode: "STRIPMAP"},
			domain.Attitude{RollDeg: 20, Mode: "STRIPMAP"},
			5 * time.Second},
		{"10 degrees at 1 deg/s plus settling",
			domain.Attitude{RollDeg: 20, Mode: "STRIPMAP"},
			domain.Attitude{RollDeg: 30, Mode: "STRIPMAP"},
			15 * time.Second},
		{"the same 10 degrees the other way costs the same",
			domain.Attitude{RollDeg: 30, Mode: "STRIPMAP"},
			domain.Attitude{RollDeg: 20, Mode: "STRIPMAP"},
			15 * time.Second},
		{"crossing nadir covers both angles",
			domain.Attitude{RollDeg: -20, Mode: "STRIPMAP"},
			domain.Attitude{RollDeg: 20, Mode: "STRIPMAP"},
			45 * time.Second},
		{"a mode change adds its overhead",
			domain.Attitude{RollDeg: 20, Mode: "STRIPMAP"},
			domain.Attitude{RollDeg: 20, Mode: "SPOTLIGHT"},
			8 * time.Second},
		{"slew and mode change are additive, not overlapped",
			domain.Attitude{RollDeg: 20, Mode: "STRIPMAP"},
			domain.Attitude{RollDeg: 30, Mode: "SPOTLIGHT"},
			18 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := a.SlewTime(tt.from, tt.to); got != tt.want {
				t.Errorf("SlewTime = %s, want %s", got, tt.want)
			}
		})
	}
}

// PROPERTY: slew_time(a, a) is exactly the settling floor.
//
// The load-bearing one. Fold settling into an effective slew rate and this
// becomes zero, so back-to-back acquisitions at identical geometry appear free —
// and a plan built on that is one that cannot be flown.
func TestSlewTimeToItselfIsTheSettlingFloor(t *testing.T) {
	a := agility()
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic by design

	for range 500 {
		at := domain.Attitude{
			RollDeg: rng.Float64()*90 - 45,
			Mode:    []string{"SPOTLIGHT", "STRIPMAP", "SCAN"}[rng.IntN(3)],
		}
		if got := a.SlewTime(at, at); got != a.SettlingFloor() {
			t.Fatalf("SlewTime(%+v, itself) = %s, want the floor %s", at, got, a.SettlingFloor())
		}
	}
}

// PROPERTY: monotonic in |Δroll|. A larger manoeuvre never costs less.
func TestSlewTimeIsMonotonicInRollDelta(t *testing.T) {
	a := agility()
	rng := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // deterministic by design

	for range 1000 {
		base := rng.Float64()*90 - 45
		d1 := rng.Float64() * 45
		d2 := d1 + rng.Float64()*45 // strictly further

		mode := "STRIPMAP"
		from := domain.Attitude{RollDeg: base, Mode: mode}
		near := domain.Attitude{RollDeg: base + d1, Mode: mode}
		far := domain.Attitude{RollDeg: base + d2, Mode: mode}

		if a.SlewTime(from, far) < a.SlewTime(from, near) {
			t.Fatalf("a %.3f° slew cost %s, less than a %.3f° slew at %s",
				d2, a.SlewTime(from, far), d1, a.SlewTime(from, near))
		}
	}
}

// PROPERTY: symmetric in roll, which is a CLAIM and not an oversight.
//
// Under reaction-wheel control the torque to roll away from nadir equals the
// torque to roll back, so |Δroll| is the whole story. If this test is ever
// changed to expect asymmetry, the model has taken on an asymmetry coefficient
// nobody measured — and the benchmark would then be measuring the invention.
func TestSlewTimeIsSymmetricInRoll(t *testing.T) {
	a := agility()
	rng := rand.New(rand.NewPCG(5, 6)) //nolint:gosec // deterministic by design

	for range 1000 {
		mode := "STRIPMAP"
		x := domain.Attitude{RollDeg: rng.Float64()*90 - 45, Mode: mode}
		y := domain.Attitude{RollDeg: rng.Float64()*90 - 45, Mode: mode}

		if a.SlewTime(x, y) != a.SlewTime(y, x) {
			t.Fatalf("SlewTime(%.3f→%.3f) = %s but the reverse = %s",
				x.RollDeg, y.RollDeg, a.SlewTime(x, y), a.SlewTime(y, x))
		}
	}
}

// PROPERTY: never below the floor, whatever the inputs.
func TestSlewTimeIsNeverBelowTheFloor(t *testing.T) {
	a := agility()
	rng := rand.New(rand.NewPCG(7, 8)) //nolint:gosec // deterministic by design
	modes := []string{"SPOTLIGHT", "STRIPMAP", "SCAN"}

	for range 1000 {
		x := domain.Attitude{RollDeg: rng.Float64()*120 - 60, Mode: modes[rng.IntN(3)]}
		y := domain.Attitude{RollDeg: rng.Float64()*120 - 60, Mode: modes[rng.IntN(3)]}

		if got := a.SlewTime(x, y); got < a.SettlingFloor() {
			t.Fatalf("SlewTime(%+v, %+v) = %s, below the floor %s", x, y, got, a.SettlingFloor())
		}
	}
}

func TestAgilityValidation(t *testing.T) {
	if err := agility().Validate(); err != nil {
		t.Fatalf("the baseline must be valid: %v", err)
	}

	tests := []struct {
		name    string
		agility domain.Agility
		want    string
	}{
		{"zero slew rate", domain.Agility{SlewRateDegS: 0, MaxRollDeg: 45}, "slew rate"},
		{"negative slew rate", domain.Agility{SlewRateDegS: -1, MaxRollDeg: 45}, "slew rate"},
		{"negative settle", domain.Agility{SlewRateDegS: 1, SettleTimeS: -1, MaxRollDeg: 45}, "settle"},
		{"negative mode transition", domain.Agility{SlewRateDegS: 1, ModeTransitionS: -1, MaxRollDeg: 45}, "mode-transition"},
		{"no roll authority", domain.Agility{SlewRateDegS: 1, MaxRollDeg: 0}, "max roll"},
		{"roll authority beyond the contract", domain.Agility{SlewRateDegS: 1, MaxRollDeg: 61}, "max roll"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.agility.Validate()
			if err == nil {
				t.Fatalf("accepted %s", tt.name)
			}
			if !errors.Is(err, domain.ErrInvalid) {
				t.Errorf("does not wrap ErrInvalid: %v", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not name %q", err, tt.want)
			}
		})
	}
}

// A zero settling time is optimistic, not impossible, and must be permitted —
// forbidding it would stop M2-13 isolating the slew term from the floor.
func TestZeroSettlingIsPermitted(t *testing.T) {
	a := domain.Agility{SlewRateDegS: 1, SettleTimeS: 0, MaxRollDeg: 45}
	if err := a.Validate(); err != nil {
		t.Fatalf("zero settling was refused: %v", err)
	}
	at := domain.Attitude{RollDeg: 20, Mode: "STRIPMAP"}
	if got := a.SlewTime(at, at); got != 0 {
		t.Errorf("with no settling, an identical transition cost %s, want 0", got)
	}
}

// A derived angle beyond the spacecraft's authority is not a computation error —
// it is a candidate that cannot be flown, and M2-15 has to tell them apart.
func TestRollAuthorityIsCheckedSeparately(t *testing.T) {
	a := agility() // 45 degrees

	if !a.WithinRollAuthority(domain.Attitude{RollDeg: 45}) {
		t.Error("exactly at the limit was refused")
	}
	if !a.WithinRollAuthority(domain.Attitude{RollDeg: -45}) {
		t.Error("the limit is not symmetric about nadir")
	}
	if a.WithinRollAuthority(domain.Attitude{RollDeg: 45.1}) {
		t.Error("beyond the limit was accepted; the plan would not be flyable")
	}
	if a.WithinRollAuthority(domain.Attitude{RollDeg: -50}) {
		t.Error("beyond the limit on the left was accepted")
	}
}

func TestSatelliteProfileValidation(t *testing.T) {
	good := domain.SatelliteProfile{Agility: agility(), DutyCycleBudgetS: 600}
	if err := good.Validate(); err != nil {
		t.Fatalf("the baseline profile must be valid: %v", err)
	}

	tests := []struct {
		name    string
		profile domain.SatelliteProfile
		want    string
	}{
		{"unusable agility", domain.SatelliteProfile{
			Agility: domain.Agility{SlewRateDegS: 0, MaxRollDeg: 45}, DutyCycleBudgetS: 600,
		}, "slew rate"},
		{"no power budget", domain.SatelliteProfile{
			Agility: agility(), DutyCycleBudgetS: 0,
		}, "duty-cycle budget"},
		{"negative power budget", domain.SatelliteProfile{
			Agility: agility(), DutyCycleBudgetS: -1,
		}, "duty-cycle budget"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.profile.Validate()
			if err == nil {
				t.Fatalf("accepted %s", tt.name)
			}
			if !errors.Is(err, domain.ErrInvalid) {
				t.Errorf("does not wrap ErrInvalid: %v", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not name %q", err, tt.want)
			}
		})
	}
}

// When roll is absent AND the geometry is unusable, the failure must surface
// rather than resolving to a zero attitude — a zero roll is nadir, which is a
// perfectly plausible-looking answer and completely wrong.
func TestAttitudeForPropagatesADerivationFailure(t *testing.T) {
	got, err := domain.AttitudeFor(nil, 0, 570, domain.LookRight, "STRIPMAP")
	if err == nil {
		t.Fatalf("unusable geometry produced attitude %+v", got)
	}
	if !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("does not wrap ErrInvalid: %v", err)
	}
	if got.RollDeg != 0 || got.Mode != "" {
		t.Errorf("a failed derivation returned %+v; a partially-filled attitude invites use", got)
	}
}
