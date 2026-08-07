package domain_test

import (
	"testing"
	"time"

	"github.com/mhayk/overpass/services/tasking-api/internal/domain"
)

var now = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

func sensors() []domain.SensorCapability {
	return []domain.SensorCapability{
		{Mode: "STRIPMAP", MinIncidenceDeg: 15, MaxIncidenceDeg: 45, MaxSquintDeg: 5,
			LookSides: []string{"LEFT", "RIGHT"}},
		{Mode: "SPOTLIGHT", MinIncidenceDeg: 20, MaxIncidenceDeg: 50, MaxSquintDeg: 20,
			LookSides: []string{"RIGHT"}},
	}
}

func valid() domain.SubmitRequest {
	return domain.SubmitRequest{
		CustomerID:  "acme",
		TargetName:  "somewhere",
		Target:      domain.Target{Kind: domain.TargetPoint, Point: domain.Position{Lon: -9.1, Lat: 38.7}},
		WindowStart: now.Add(time.Hour),
		WindowEnd:   now.Add(25 * time.Hour),
		// #nosec G101 -- a tier name, not a credential
		PriorityTier:   "COMMERCIAL",
		BidCredits:     1000,
		RequestedModes: []string{"STRIPMAP"},
	}
}

func ptr(f float64) *float64 { return &f }

func TestAValidRequestPasses(t *testing.T) {
	if got := domain.Validate(valid(), now, sensors(), domain.DefaultValidationPolicy()); !got.OK() {
		t.Fatalf("a valid request was rejected: %+v", got.Errors)
	}
}

// Every rejection reason gets a case. The table is the point: a reason code
// that no test produces is a reason code nobody knows still works.
func TestEveryRejectionReason(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*domain.SubmitRequest)
		want    domain.ReasonCode
		pointer string
	}{
		{
			name:    "inverted window",
			mutate:  func(r *domain.SubmitRequest) { r.WindowStart, r.WindowEnd = r.WindowEnd, r.WindowStart },
			want:    domain.ReasonWindowInverted,
			pointer: "/window",
		},
		{
			name: "window already closed",
			mutate: func(r *domain.SubmitRequest) {
				r.WindowStart = now.Add(-48 * time.Hour)
				r.WindowEnd = now.Add(-time.Hour)
			},
			want:    domain.ReasonDeadlineInPast,
			pointer: "/window/end",
		},
		{
			name:    "horizon beyond the maximum",
			mutate:  func(r *domain.SubmitRequest) { r.WindowEnd = r.WindowStart.Add(30 * 24 * time.Hour) },
			want:    domain.ReasonHorizonTooLong,
			pointer: "/window",
		},
		{
			name: "unsupported geometry type",
			mutate: func(r *domain.SubmitRequest) {
				r.Target = domain.Target{Kind: "LineString"}
			},
			want:    domain.ReasonTargetUnsupportedGeom,
			pointer: "/target/type",
		},
		{
			name: "polygon ring not closed",
			mutate: func(r *domain.SubmitRequest) {
				r.Target = domain.Target{Kind: domain.TargetPolygon, Ring: []domain.Position{
					{Lon: 0, Lat: 0}, {Lon: 0, Lat: 1}, {Lon: 1, Lat: 1}, {Lon: 1, Lat: 0},
				}}
			},
			want:    domain.ReasonTargetUnsupportedGeom,
			pointer: "/target/coordinates",
		},
		{
			name: "polygon ring too short",
			mutate: func(r *domain.SubmitRequest) {
				r.Target = domain.Target{Kind: domain.TargetPolygon, Ring: []domain.Position{
					{Lon: 0, Lat: 0}, {Lon: 0, Lat: 1}, {Lon: 0, Lat: 0},
				}}
			},
			want:    domain.ReasonTargetUnsupportedGeom,
			pointer: "/target/coordinates",
		},
		{
			name: "target too large",
			mutate: func(r *domain.SubmitRequest) {
				r.Target = domain.Target{Kind: domain.TargetPolygon, Ring: []domain.Position{
					{Lon: -30, Lat: -20}, {Lon: -30, Lat: 20}, {Lon: 30, Lat: 20},
					{Lon: 30, Lat: -20}, {Lon: -30, Lat: -20},
				}}
			},
			want:    domain.ReasonTargetTooLarge,
			pointer: "/target",
		},
		{
			name:    "mode no sensor supports",
			mutate:  func(r *domain.SubmitRequest) { r.RequestedModes = []string{"HOLOGRAM"} },
			want:    domain.ReasonUnsupportedMode,
			pointer: "/requested_modes/0",
		},
		{
			name: "incidence band inverted",
			mutate: func(r *domain.SubmitRequest) {
				r.Constraints = domain.RequestConstraints{MinIncidenceDeg: ptr(40), MaxIncidenceDeg: ptr(20)}
			},
			want:    domain.ReasonConstraintsUnsatisfiable,
			pointer: "/constraints",
		},
		{
			name: "incidence band outside every sensor",
			mutate: func(r *domain.SubmitRequest) {
				r.Constraints = domain.RequestConstraints{MinIncidenceDeg: ptr(70), MaxIncidenceDeg: ptr(80)}
			},
			want:    domain.ReasonConstraintsUnsatisfiable,
			pointer: "/constraints",
		},
		{
			name: "look side the requested mode cannot do",
			mutate: func(r *domain.SubmitRequest) {
				r.RequestedModes = []string{"SPOTLIGHT"} // RIGHT only
				r.Constraints = domain.RequestConstraints{LookSide: "LEFT"}
			},
			want:    domain.ReasonConstraintsUnsatisfiable,
			pointer: "/constraints",
		},
		{
			name:    "missing customer",
			mutate:  func(r *domain.SubmitRequest) { r.CustomerID = "" },
			want:    domain.ReasonValidationFailed,
			pointer: "/customer_id",
		},
		{
			name: "latitude out of range",
			mutate: func(r *domain.SubmitRequest) {
				r.Target.Point = domain.Position{Lon: 10, Lat: 100}
			},
			want:    domain.ReasonValidationFailed,
			pointer: "/target/coordinates",
		},
		{
			name:    "no modes at all",
			mutate:  func(r *domain.SubmitRequest) { r.RequestedModes = nil },
			want:    domain.ReasonValidationFailed,
			pointer: "/requested_modes",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := valid()
			tc.mutate(&r)

			result := domain.Validate(r, now, sensors(), domain.DefaultValidationPolicy())
			if result.OK() {
				t.Fatalf("expected %s, but the request was accepted", tc.want)
			}
			found := false
			for _, e := range result.Errors {
				if e.Code == tc.want && e.Pointer == tc.pointer {
					found = true
					if e.Message == "" {
						t.Error("the error carries no message, so a client can show nothing useful")
					}
				}
			}
			if !found {
				t.Fatalf("wanted %s at %s, got %+v", tc.want, tc.pointer, result.Errors)
			}
		})
	}
}

func TestAnInvertedWindowSuppressesItsOwnConsequences(t *testing.T) {
	// A window that ends before it starts also looks like a past deadline and a
	// negative horizon. Reporting three consequences of one mistake sends the
	// customer to fix the wrong thing.
	r := valid()
	r.WindowStart = now.Add(48 * time.Hour)
	r.WindowEnd = now.Add(time.Hour)

	result := domain.Validate(r, now, sensors(), domain.DefaultValidationPolicy())
	windowErrors := 0
	for _, e := range result.Errors {
		if e.Pointer == "/window" || e.Pointer == "/window/end" {
			windowErrors++
		}
	}
	if windowErrors != 1 {
		t.Fatalf("one inverted window produced %d window errors: %+v", windowErrors, result.Errors)
	}
}

func TestEveryProblemIsReportedNotJustTheFirst(t *testing.T) {
	// Three mistakes must not cost three round trips, each one a chance for the
	// customer to give up.
	r := valid()
	r.CustomerID = ""
	r.TargetName = ""
	r.RequestedModes = []string{"HOLOGRAM"}

	result := domain.Validate(r, now, sensors(), domain.DefaultValidationPolicy())
	if len(result.Errors) < 3 {
		t.Fatalf("expected at least 3 errors, got %+v", result.Errors)
	}
}

func TestPrimaryReasonIsTheMostFundamental(t *testing.T) {
	r := valid()
	r.WindowStart, r.WindowEnd = r.WindowEnd, r.WindowStart
	r.RequestedModes = []string{"HOLOGRAM"}

	if got := domain.Validate(r, now, sensors(), domain.DefaultValidationPolicy()).Primary(); got != domain.ReasonWindowInverted {
		t.Fatalf("got %s, want the window problem to lead", got)
	}
}

func TestConstraintsThatNarrowWithinASensorAreFine(t *testing.T) {
	r := valid()
	r.Constraints = domain.RequestConstraints{MinIncidenceDeg: ptr(25), MaxIncidenceDeg: ptr(35)}

	if got := domain.Validate(r, now, sensors(), domain.DefaultValidationPolicy()); !got.OK() {
		t.Fatalf("a reasonable narrowing was rejected: %+v", got.Errors)
	}
}

func TestSatisfiabilityConsidersOnlyTheRequestedModes(t *testing.T) {
	// SPOTLIGHT is RIGHT-only and STRIPMAP does both. Asking for LEFT with only
	// SPOTLIGHT requested must fail even though another configured sensor could
	// have done it — the customer did not ask for that one.
	r := valid()
	r.RequestedModes = []string{"SPOTLIGHT"}
	r.Constraints = domain.RequestConstraints{LookSide: "LEFT"}

	result := domain.Validate(r, now, sensors(), domain.DefaultValidationPolicy())
	if result.OK() {
		t.Fatal("a left-looking SPOTLIGHT request was accepted")
	}

	r.RequestedModes = []string{"SPOTLIGHT", "STRIPMAP"}
	if got := domain.Validate(r, now, sensors(), domain.DefaultValidationPolicy()); !got.OK() {
		t.Fatalf("adding a capable mode should make it satisfiable: %+v", got.Errors)
	}
}

func TestAValidPolygonTargetPasses(t *testing.T) {
	r := valid()
	r.Target = domain.Target{Kind: domain.TargetPolygon, Ring: []domain.Position{
		{Lon: 4.0, Lat: 51.9}, {Lon: 4.0, Lat: 52.0}, {Lon: 4.2, Lat: 52.0},
		{Lon: 4.2, Lat: 51.9}, {Lon: 4.0, Lat: 51.9},
	}}
	if got := domain.Validate(r, now, sensors(), domain.DefaultValidationPolicy()); !got.OK() {
		t.Fatalf("a valid polygon was rejected: %+v", got.Errors)
	}
}
