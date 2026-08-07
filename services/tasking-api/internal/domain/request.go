package domain

import (
	"fmt"
	"math"
	"time"
)

// ReasonCode is the closed vocabulary shared by the synchronous 4xx and the
// asynchronous tasking.request.rejected.v1 event.
//
// One taxonomy, two transports. A bespoke set of HTTP error strings alongside
// an event enum is how the "why was my request rejected?" panel ends up unable
// to explain a rejection the API already explained.
type ReasonCode string

const (
	ReasonValidationFailed         ReasonCode = "VALIDATION_FAILED"
	ReasonDeadlineInPast           ReasonCode = "DEADLINE_IN_PAST"
	ReasonWindowInverted           ReasonCode = "WINDOW_INVERTED"
	ReasonHorizonTooLong           ReasonCode = "HORIZON_TOO_LONG"
	ReasonTargetUnsupportedGeom    ReasonCode = "TARGET_UNSUPPORTED_GEOMETRY"
	ReasonTargetTooLarge           ReasonCode = "TARGET_TOO_LARGE"
	ReasonUnsupportedMode          ReasonCode = "UNSUPPORTED_MODE"
	ReasonConstraintsUnsatisfiable ReasonCode = "CONSTRAINTS_UNSATISFIABLE"
)

// FieldError points at the offending field so a client can highlight it rather
// than show a wall of text. Pointer is RFC 6901.
type FieldError struct {
	Pointer string
	Code    ReasonCode
	Message string
}

// ValidationResult is every problem found, not just the first.
//
// Returning on the first failure means a client with three mistakes makes three
// round trips to discover them, and each round trip is a chance to give up.
type ValidationResult struct {
	Errors []FieldError
}

// OK reports whether the request may be accepted.
func (v ValidationResult) OK() bool { return len(v.Errors) == 0 }

// Primary is the reason code to put on the Problem response and, later, on the
// rejection event.
//
// The FIRST error wins rather than the most severe, because the order below is
// deliberately from most-fundamental to most-specific: a request with an
// inverted window and an unsupported mode has an inverted window, and telling
// the customer about the mode first sends them to fix the wrong thing.
func (v ValidationResult) Primary() ReasonCode {
	if len(v.Errors) == 0 {
		return ""
	}
	return v.Errors[0].Code
}

// Position is a WGS84 coordinate pair, longitude first — GeoJSON order.
type Position struct {
	Lon float64
	Lat float64
}

// TargetKind distinguishes the two geometries a request may carry.
type TargetKind string

const (
	TargetPoint   TargetKind = "Point"
	TargetPolygon TargetKind = "Polygon"
)

// Target is the customer's area or point of interest.
type Target struct {
	Kind TargetKind
	// Point carries one position; Polygon carries its exterior ring.
	Point Position
	Ring  []Position
}

// SubmitRequest is the validated shape of an inbound submission.
type SubmitRequest struct {
	CustomerID     string
	TargetName     string
	Target         Target
	WindowStart    time.Time
	WindowEnd      time.Time
	PriorityTier   string
	BidCredits     int64
	RequestedModes []string
	Constraints    RequestConstraints
}

// RequestConstraints is the customer's narrowing, as submitted.
type RequestConstraints struct {
	LookSide        string
	MinIncidenceDeg *float64
	MaxIncidenceDeg *float64
	MaxSquintDeg    *float64
}

// SensorCapability is what some configured sensor can actually do.
//
// Passed in rather than looked up, so validation stays a pure function and the
// "could any sensor ever satisfy this?" question is answerable in a unit test.
type SensorCapability struct {
	Mode            string
	MinIncidenceDeg float64
	MaxIncidenceDeg float64
	MaxSquintDeg    float64
	LookSides       []string
}

// ValidationPolicy holds the limits validation enforces.
type ValidationPolicy struct {
	MaxHorizon        time.Duration
	MaxTargetAreaKM2  float64
	MinWindowDuration time.Duration
}

// DefaultValidationPolicy matches the horizon clamp in feasibility.
func DefaultValidationPolicy() ValidationPolicy {
	return ValidationPolicy{
		MaxHorizon: 72 * time.Hour,
		// A target larger than this cannot be covered by one acquisition in any
		// configured mode, so accepting it promises something no plan can
		// deliver. The widest SCAN swath is a few hundred kilometres.
		MaxTargetAreaKM2:  500_000,
		MinWindowDuration: time.Minute,
	}
}

// Validate applies every cheap, local check.
//
// Cheap and local ON PURPOSE. Whether the target is actually imageable requires
// propagating the constellation, and that is the entire reason this endpoint is
// asynchronous. What is caught here is what can be known without leaving the
// process.
func Validate(
	r SubmitRequest,
	now time.Time,
	sensors []SensorCapability,
	policy ValidationPolicy,
) ValidationResult {
	var out ValidationResult
	add := func(pointer string, code ReasonCode, format string, args ...any) {
		out.Errors = append(out.Errors, FieldError{
			Pointer: pointer,
			Code:    code,
			Message: fmt.Sprintf(format, args...),
		})
	}

	if r.CustomerID == "" {
		add("/customer_id", ReasonValidationFailed, "customer_id is required")
	}
	if r.TargetName == "" {
		add("/target_name", ReasonValidationFailed, "target_name is required")
	}
	if r.BidCredits < 0 {
		add("/bid_credits", ReasonValidationFailed, "bid_credits must not be negative")
	}

	validateWindow(r, now, policy, add)
	validateTarget(r.Target, policy, add)
	validateModes(r.RequestedModes, sensors, add)
	validateConstraints(r, sensors, add)

	return out
}

type addFunc func(pointer string, code ReasonCode, format string, args ...any)

func validateWindow(r SubmitRequest, now time.Time, policy ValidationPolicy, add addFunc) {
	if r.WindowStart.IsZero() || r.WindowEnd.IsZero() {
		add("/window", ReasonValidationFailed, "window start and end are both required")
		return
	}

	// Inverted first. A window that ends before it starts makes every other
	// window check meaningless, and reporting three consequences of one mistake
	// helps nobody.
	if !r.WindowEnd.After(r.WindowStart) {
		add("/window", ReasonWindowInverted, "window end %s is not after start %s",
			r.WindowEnd.Format(time.RFC3339), r.WindowStart.Format(time.RFC3339))
		return
	}

	// The DEADLINE is the end of the window: a request whose window has already
	// closed can never be fulfilled, whatever its start says.
	if !r.WindowEnd.After(now) {
		add("/window/end", ReasonDeadlineInPast, "window closed at %s, which is in the past",
			r.WindowEnd.Format(time.RFC3339))
	}

	if r.WindowEnd.Sub(r.WindowStart) > policy.MaxHorizon {
		add("/window", ReasonHorizonTooLong, "window spans %s, the maximum is %s",
			r.WindowEnd.Sub(r.WindowStart), policy.MaxHorizon)
	}

	if r.WindowEnd.Sub(r.WindowStart) < policy.MinWindowDuration {
		add("/window", ReasonValidationFailed, "window is shorter than %s and cannot contain an acquisition",
			policy.MinWindowDuration)
	}
}

func validateTarget(t Target, policy ValidationPolicy, add addFunc) {
	switch t.Kind {
	case TargetPoint:
		validatePosition("/target/coordinates", t.Point, add)

	case TargetPolygon:
		// A GeoJSON linear ring needs four positions, the last equal to the
		// first. Three distinct corners plus the repeat is the minimum that
		// encloses anything.
		if len(t.Ring) < 4 {
			add("/target/coordinates", ReasonTargetUnsupportedGeom,
				"a polygon ring needs at least 4 positions, got %d", len(t.Ring))
			return
		}
		if t.Ring[0] != t.Ring[len(t.Ring)-1] {
			add("/target/coordinates", ReasonTargetUnsupportedGeom,
				"polygon ring is not closed: first position %v does not equal last %v",
				t.Ring[0], t.Ring[len(t.Ring)-1])
			return
		}
		for i, p := range t.Ring {
			validatePosition(fmt.Sprintf("/target/coordinates/0/%d", i), p, add)
		}
		if area := ringAreaKM2(t.Ring); area > policy.MaxTargetAreaKM2 {
			add("/target", ReasonTargetTooLarge,
				"target covers about %.0f km2, the maximum is %.0f km2", area, policy.MaxTargetAreaKM2)
		}

	default:
		add("/target/type", ReasonTargetUnsupportedGeom,
			"target type %q is not supported; use Point or Polygon", t.Kind)
	}
}

func validatePosition(pointer string, p Position, add addFunc) {
	// Longitude first. A swapped pair parses cleanly and puts the target in the
	// wrong hemisphere, which is the failure CLAUDE.md's prefixItems note is
	// about — it is caught here only when the swap pushes latitude out of range.
	if p.Lat < -90 || p.Lat > 90 {
		add(pointer, ReasonValidationFailed, "latitude %v is outside -90..90", p.Lat)
	}
	if p.Lon < -180 || p.Lon > 180 {
		add(pointer, ReasonValidationFailed, "longitude %v is outside -180..180", p.Lon)
	}
}

func validateModes(requested []string, sensors []SensorCapability, add addFunc) {
	if len(requested) == 0 {
		add("/requested_modes", ReasonValidationFailed, "at least one mode is required")
		return
	}
	for i, mode := range requested {
		supported := false
		for _, s := range sensors {
			if s.Mode == mode {
				supported = true
				break
			}
		}
		if !supported {
			add(fmt.Sprintf("/requested_modes/%d", i), ReasonUnsupportedMode,
				"no configured sensor supports mode %q", mode)
		}
	}
}

// validateConstraints catches narrowing that NO configured sensor could ever
// satisfy.
//
// At ingress rather than after a feasibility sweep. Discovering it by
// propagating the whole constellation costs seconds of compute to learn
// something arithmetic could have told us in microseconds — and the customer
// waits for an answer that was knowable at submission.
func validateConstraints(r SubmitRequest, sensors []SensorCapability, add addFunc) {
	c := r.Constraints

	if c.MinIncidenceDeg != nil && c.MaxIncidenceDeg != nil && *c.MinIncidenceDeg > *c.MaxIncidenceDeg {
		add("/constraints", ReasonConstraintsUnsatisfiable,
			"min_incidence_deg %.1f is above max_incidence_deg %.1f", *c.MinIncidenceDeg, *c.MaxIncidenceDeg)
		return
	}

	// Only consider sensors whose mode the customer actually asked for.
	candidates := make([]SensorCapability, 0, len(sensors))
	for _, s := range sensors {
		for _, m := range r.RequestedModes {
			if s.Mode == m {
				candidates = append(candidates, s)
				break
			}
		}
	}
	if len(candidates) == 0 {
		return // already reported by validateModes
	}

	for _, s := range candidates {
		if satisfiable(c, s) {
			return
		}
	}
	add("/constraints", ReasonConstraintsUnsatisfiable,
		"no configured sensor in the requested modes can satisfy these constraints")
}

func satisfiable(c RequestConstraints, s SensorCapability) bool {
	low, high := s.MinIncidenceDeg, s.MaxIncidenceDeg
	if c.MinIncidenceDeg != nil {
		low = math.Max(low, *c.MinIncidenceDeg)
	}
	if c.MaxIncidenceDeg != nil {
		high = math.Min(high, *c.MaxIncidenceDeg)
	}
	if low >= high {
		return false
	}
	if c.MaxSquintDeg != nil && *c.MaxSquintDeg < 0 {
		return false
	}
	if c.LookSide != "" && c.LookSide != "ANY" {
		permitted := false
		for _, side := range s.LookSides {
			if side == c.LookSide {
				permitted = true
				break
			}
		}
		if !permitted {
			return false
		}
	}
	return true
}

// ringAreaKM2 approximates the area of a small polygon on a sphere.
//
// The shoelace formula on an equirectangular projection about the ring's mean
// latitude. Good to a few percent for the sizes this check cares about, and the
// check is a coarse "is this absurdly large" guard rather than a measurement —
// the honest geodesic area lives in feasibility, which already has pyproj.
func ringAreaKM2(ring []Position) float64 {
	if len(ring) < 4 {
		return 0
	}
	var meanLat float64
	for _, p := range ring[:len(ring)-1] {
		meanLat += p.Lat
	}
	meanLat /= float64(len(ring) - 1)

	const kmPerDegreeLat = 111.32
	kmPerDegreeLon := kmPerDegreeLat * math.Cos(meanLat*math.Pi/180)

	var twiceArea float64
	for i := range len(ring) - 1 {
		x1, y1 := ring[i].Lon*kmPerDegreeLon, ring[i].Lat*kmPerDegreeLat
		x2, y2 := ring[i+1].Lon*kmPerDegreeLon, ring[i+1].Lat*kmPerDegreeLat
		twiceArea += x1*y2 - x2*y1
	}
	return math.Abs(twiceArea) / 2
}

// State is the request lifecycle, from the OpenAPI description.
type State string

const (
	StateReceived         State = "RECEIVED"
	StateAwaitingPlanning State = "AWAITING_PLANNING"
	StatePlanned          State = "PLANNED"
	StateAcquired         State = "ACQUIRED"
	StateInfeasible       State = "INFEASIBLE"
	StateRejected         State = "REJECTED"
	StateExpired          State = "EXPIRED"
	StateCancelled        State = "CANCELLED"
)

// TargetWKT renders a target as WKT for PostGIS.
//
// WKT rather than GeoJSON because the column is geometry(Geometry, 4326) and
// ST_GeomFromText is one call. Longitude first, matching both WKT and GeoJSON —
// the swap that silently relocates a target to another hemisphere is the same
// one CLAUDE.md warns about, so the ordering is stated in one place and used
// everywhere.
func TargetWKT(t Target) string {
	switch t.Kind {
	case TargetPoint:
		return fmt.Sprintf("POINT(%v %v)", t.Point.Lon, t.Point.Lat)
	case TargetPolygon:
		out := "POLYGON(("
		for i, p := range t.Ring {
			if i > 0 {
				out += ", "
			}
			out += fmt.Sprintf("%v %v", p.Lon, p.Lat)
		}
		return out + "))"
	default:
		return ""
	}
}

// ConfiguredSensors is the constellation's capability, as ingress sees it.
//
// Here rather than in configuration for now, and that is a deliberate
// placeholder with a cost: the real source is reference.satellites.sensor_modes,
// and ingress will read it from there once the seeder populates it in M1-19.
// Hardcoding the band today means a sensor added to the database is not
// reflected at ingress until this is replaced — which is why it is one function
// with one caller rather than scattered constants.
func ConfiguredSensors() []SensorCapability {
	return []SensorCapability{
		{Mode: "SPOTLIGHT", MinIncidenceDeg: 20, MaxIncidenceDeg: 50, MaxSquintDeg: 20,
			LookSides: []string{"LEFT", "RIGHT"}},
		{Mode: "STRIPMAP", MinIncidenceDeg: 15, MaxIncidenceDeg: 45, MaxSquintDeg: 5,
			LookSides: []string{"LEFT", "RIGHT"}},
		{Mode: "SCAN", MinIncidenceDeg: 15, MaxIncidenceDeg: 45, MaxSquintDeg: 3,
			LookSides: []string{"LEFT", "RIGHT"}},
	}
}
