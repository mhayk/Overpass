package domain

import (
	"fmt"
	"math"
	"time"
)

// The transition-time model: what has to happen between two acquisitions on one
// satellite before the second can start.
//
// This is where the real difficulty lives. Modelled as a function of the PAIR
// rather than as a constant gap, because a constant gap collapses the problem to
// ordinary interval scheduling — ADR-0007's strong NP-hardness reduction runs
// through this term, so a constant here would make the whole allocation argument
// false rather than merely approximate.
//
// IT IS AN APPROXIMATION, and stating that openly is the point. The real
// relationship between commanded angle and manoeuvre time is not linear: a
// reaction-wheel slew accelerates, coasts and decelerates, so short slews are
// dominated by acceleration and the linear model understates them while long
// slews approach the linear limit. A defensible simplification stated openly is
// stronger than an unstated one, and M2-13 is where the error would show up as
// plans that are optimistic about tightly-packed acquisitions.

// meanEarthRadiusKm is WGS84's mean radius.
//
// A sphere, not the ellipsoid. The roll derivation below is trigonometry on a
// circular cross-section, and the flattening it ignores moves the derived angle
// by well under a degree — small against the slew rates this feeds, and stated
// rather than hidden. Feasibility does the real geodesy; this is a scheduling
// term.
const meanEarthRadiusKm = 6371.0088

// LookSide values, mirroring sar.v1.schema.json.
const (
	LookRight = "RIGHT"
	LookLeft  = "LEFT"
)

// Agility is one satellite's transition parameters, from reference.satellites.
type Agility struct {
	SlewRateDegS    float64
	SettleTimeS     float64
	ModeTransitionS float64
	MaxRollDeg      float64
}

// Validate refuses parameters the transition function cannot use.
func (a Agility) Validate() error {
	if a.SlewRateDegS <= 0 {
		// Zero would divide by zero below. A satellite that cannot slew cannot
		// image off-nadir at all, which is a different statement from one that
		// slews slowly, and the model should not silently conflate them.
		return fmt.Errorf("%w: slew rate must be positive, got %v deg/s", ErrInvalid, a.SlewRateDegS)
	}
	if a.SettleTimeS < 0 {
		return fmt.Errorf("%w: settle time cannot be negative, got %v s", ErrInvalid, a.SettleTimeS)
	}
	if a.ModeTransitionS < 0 {
		return fmt.Errorf("%w: mode-transition overhead cannot be negative, got %v s", ErrInvalid, a.ModeTransitionS)
	}
	if a.MaxRollDeg <= 0 || a.MaxRollDeg > 60 {
		// 60 is the contract's own bound on roll_angle_deg.
		return fmt.Errorf("%w: max roll must be in (0, 60] degrees, got %v", ErrInvalid, a.MaxRollDeg)
	}
	return nil
}

// Attitude is the pointing state an acquisition requires.
type Attitude struct {
	RollDeg float64
	Mode    string
}

// SlewTime is the minimum gap between the end of a and the start of b.
//
// Three terms, and keeping them separate is what makes the model explicable:
//
//	slew    — |Δroll| / rate. Zero when the geometry is identical.
//	settle  — a constant floor, paid on EVERY transition including a zero-angle
//	          one. A satellite that has finished rotating is not yet stable
//	          enough to image, and folding this into an effective rate would make
//	          slew_time(a, a) zero, so back-to-back acquisitions at identical
//	          geometry would appear free.
//	mode    — charged only when the imaging mode changes. Reconfiguring the
//	          radar, not moving the spacecraft.
//
// SYMMETRIC IN ROLL, and that is a claim rather than an oversight. Under
// reaction-wheel control the torque available to roll away from nadir is the
// same as the torque available to roll back, so |Δroll| is the whole story. The
// case that WOULD be asymmetric is a momentum-biased or gravity-gradient
// stabilised spacecraft, where returning toward nadir is assisted and departing
// is resisted. Modelling that needs an asymmetry coefficient nobody here has
// measured, and an invented constant would make the benchmark measure the
// invention. See the M2-02 pull request for the argument.
//
// The mode overhead is ADDITIVE rather than max(slew, mode). Additive assumes
// the radar cannot reconfigure while the spacecraft is slewing; max() assumes it
// can, and is the more optimistic of the two. Additive is chosen because it
// cannot produce a plan that is infeasible in reality — the failure direction
// matters more than the average error.
func (a Agility) SlewTime(from, to Attitude) time.Duration {
	seconds := math.Abs(to.RollDeg-from.RollDeg)/a.SlewRateDegS + a.SettleTimeS
	if from.Mode != to.Mode {
		seconds += a.ModeTransitionS
	}
	return time.Duration(seconds * float64(time.Second))
}

// SettlingFloor is the transition cost between two acquisitions at identical
// attitude — the value SlewTime can never go below.
//
// Exported so the property test can state the invariant without restating the
// arithmetic, and so a policy estimating slew cost has a lower bound to reason
// with.
func (a Agility) SettlingFloor() time.Duration {
	return time.Duration(a.SettleTimeS * float64(time.Second))
}

// RollAngleDeg derives the spacecraft roll an acquisition requires.
//
// roll_angle_deg is OPTIONAL in sar.v1.schema.json while describing itself as
// "the input to slew_time(a, b)", so the planner cannot depend on it being
// present. It derives the angle instead, from two fields the contract does
// require: incidence_angle_deg and slant_range_km.
//
// The geometry is the triangle formed by the Earth's centre O, the satellite S
// and the target T:
//
//	|OT| = Re                      the Earth's radius at the target
//	|ST| = R                       the slant range
//	∠OTS = 180° − θ                θ is the incidence angle, measured from the
//	                               local vertical at T
//	∠OST = η                       the look angle at the spacecraft, which is
//	                               the roll required to point there
//
// Law of cosines gives the orbital radius without needing the altitude:
//
//	(Re + h)² = Re² + R² − 2·Re·R·cos(180° − θ)
//	          = Re² + R² + 2·Re·R·cos(θ)
//
// and the law of sines then gives the look angle:
//
//	sin(η) = Re · sin(θ) / (Re + h)
//
// η is always smaller than θ, which is the geometric fact that makes this
// worth deriving rather than approximating roll by incidence: at 30° incidence
// from a 500 km orbit the roll is about 27.6°, and treating them as equal would
// overstate every transition by around 8%.
//
// The sign is the look side. Right-looking is positive by the same convention
// the contract uses.
func RollAngleDeg(incidenceDeg, slantRangeKm float64, lookSide string) (float64, error) {
	if incidenceDeg <= 0 || incidenceDeg >= 90 {
		// The contract permits [0, 90]; the open interval is what the geometry
		// permits. Zero incidence is nadir, where the triangle degenerates, and
		// 90° is a horizon-grazing ray that never reaches the ground.
		return 0, fmt.Errorf("%w: incidence angle %v is outside (0, 90) degrees", ErrInvalid, incidenceDeg)
	}
	if slantRangeKm <= 0 {
		return 0, fmt.Errorf("%w: slant range must be positive, got %v km", ErrInvalid, slantRangeKm)
	}
	switch lookSide {
	case LookRight, LookLeft:
	default:
		return 0, fmt.Errorf("%w: look side %q is neither RIGHT nor LEFT", ErrInvalid, lookSide)
	}

	theta := incidenceDeg * math.Pi / 180
	re := meanEarthRadiusKm
	r := slantRangeKm

	orbitalRadius := math.Sqrt(re*re + r*r + 2*re*r*math.Cos(theta))

	sinEta := re * math.Sin(theta) / orbitalRadius
	if sinEta > 1 || sinEta < -1 {
		// Unreachable for a physically consistent (θ, R) pair, and checked
		// because math.Asin returns NaN rather than an error — a NaN roll would
		// propagate silently into every transition on that satellite and make
		// the whole plan's arithmetic meaningless without failing anything.
		return 0, fmt.Errorf("%w: incidence %v° and slant range %v km are geometrically inconsistent",
			ErrInvalid, incidenceDeg, slantRangeKm)
	}

	eta := math.Asin(sinEta) * 180 / math.Pi
	if lookSide == LookLeft {
		return -eta, nil
	}
	return eta, nil
}

// AttitudeFor resolves the attitude an acquisition needs, preferring the
// contract's roll_angle_deg when the producer supplied it and deriving it
// otherwise.
//
// Preferring the supplied value is deliberate: the producer knows its own
// spacecraft, and the derivation assumes a sphere. Deriving is the fallback that
// keeps a contract-valid opportunity schedulable rather than discarding it.
func AttitudeFor(rollDeg *float64, incidenceDeg, slantRangeKm float64, lookSide, mode string) (Attitude, error) {
	if rollDeg != nil {
		return Attitude{RollDeg: *rollDeg, Mode: mode}, nil
	}
	derived, err := RollAngleDeg(incidenceDeg, slantRangeKm, lookSide)
	if err != nil {
		return Attitude{}, err
	}
	return Attitude{RollDeg: derived, Mode: mode}, nil
}

// WithinRollAuthority reports whether an attitude is one the spacecraft can
// actually hold.
//
// Checked separately from the derivation, because a derived angle that exceeds
// the spacecraft's authority is not a computation error — it is a candidate
// that cannot be flown, and the difference matters to the unfulfilment reason
// M2-15 will report.
func (a Agility) WithinRollAuthority(at Attitude) bool {
	return math.Abs(at.RollDeg) <= a.MaxRollDeg
}
