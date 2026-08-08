package domain_test

import (
	"testing"
	"time"

	"github.com/mhayk/overpass/services/plan-gateway/internal/domain"
)

var (
	base  = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	later = base.Add(time.Minute)
)

func TestStalenessIsMeasuredFromTheLastEvent(t *testing.T) {
	got := domain.StalenessAt(base, base.Add(30*time.Second))
	if got.LagSeconds != 30 || !got.AsOf.Equal(base) {
		t.Fatalf("got %+v", got)
	}
}

func TestANegativeLagIsClampedRatherThanReported(t *testing.T) {
	// An event stamped in the future is clock skew between services, not a bug
	// here. Reporting a negative lag would claim the projection is ahead of
	// reality, which no reader could act on.
	if got := domain.StalenessAt(later, base); got.LagSeconds != 0 {
		t.Fatalf("lag is %v", got.LagSeconds)
	}
}

func TestPlanVersionDecisions(t *testing.T) {
	cases := []struct {
		name              string
		arriving, current int
		want              domain.VersionDecision
	}{
		{"first ever", 1, 0, domain.ApplyAsCurrent},
		{"newer", 3, 2, domain.ApplyAsCurrent},
		{"already folded", 2, 2, domain.Ignore},
		{"late arrival of an older version", 1, 3, domain.ApplyAsSuperseded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := domain.DecidePlanVersion(tc.arriving, tc.current); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestALateOlderVersionIsKeptRatherThanDiscarded(t *testing.T) {
	// "Drop the stale version" means "do not make it current", not "throw it
	// away". ADR-0012 retains the history, and a history nothing can address
	// was not worth keeping.
	if domain.DecidePlanVersion(1, 5) == domain.Ignore {
		t.Fatal("a late older version was discarded rather than stored as history")
	}
}

func TestEventOrderingGuard(t *testing.T) {
	if !domain.ShouldApplyEvent(later, base) {
		t.Fatal("a newer event was refused")
	}
	if domain.ShouldApplyEvent(base, later) {
		t.Fatal("a stale event was applied")
	}
	// Equal applies, for the same reason the request state machine accepts it:
	// clock resolution can collapse two distinct events onto one instant.
	if !domain.ShouldApplyEvent(base, base) {
		t.Fatal("an event at the same instant was dropped")
	}
}
