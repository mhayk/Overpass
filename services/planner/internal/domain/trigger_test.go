package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/planner/internal/domain"
)

var (
	now    = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	policy = domain.TriggerPolicy{QuietPeriod: 30 * time.Second, StalenessCeiling: 5 * time.Minute}
)

func key() domain.RoundKey {
	return domain.RoundKey{
		SatelliteID: "CAPELLA-14",
		BucketStart: time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC),
	}
}

// ago is written from `now`, so every case reads as "this happened N ago"
// rather than as arithmetic the reader has to do.
func ago(d time.Duration) time.Time { return now.Add(-d) }

func ptr(t time.Time) *time.Time { return &t }

// livePlan stands in for a committed plan the round would supersede.
var livePlan = "11111111-1111-4111-8111-111111111111"

func TestPolicyValidation(t *testing.T) {
	if err := policy.Validate(); err != nil {
		t.Fatalf("the baseline policy must be valid: %v", err)
	}

	tests := []struct {
		name   string
		policy domain.TriggerPolicy
		want   string
	}{
		{"no quiet period", domain.TriggerPolicy{StalenessCeiling: time.Minute}, "quiet period"},
		{"negative quiet period", domain.TriggerPolicy{QuietPeriod: -time.Second, StalenessCeiling: time.Minute}, "quiet period"},
		{"no ceiling", domain.TriggerPolicy{QuietPeriod: time.Second}, "ceiling"},
		// The relation is the design. A ceiling at or below the quiet period
		// means the debounce can never win, so every round reports CADENCE and
		// the burst absorption silently never happens.
		{"ceiling equals quiet period", domain.TriggerPolicy{QuietPeriod: time.Minute, StalenessCeiling: time.Minute}, "must exceed"},
		{"ceiling below quiet period", domain.TriggerPolicy{QuietPeriod: time.Minute, StalenessCeiling: time.Second}, "must exceed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.policy.Validate()
			if err == nil {
				t.Fatalf("accepted %s", tt.name)
			}
			if !errors.Is(err, domain.ErrInvalid) {
				t.Errorf("does not wrap ErrInvalid: %v", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not explain %q", err, tt.want)
			}
		})
	}
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name        string
		state       domain.BucketState
		wantFire    bool
		wantTrigger string
		wantCeiling bool
	}{
		{
			// A clean bucket never fires. The ceiling bounds staleness; it is
			// not a heartbeat, so an idle constellation does no work at all.
			name:  "clean bucket never fires, however long since the last round",
			state: domain.BucketState{Key: key(), LastRoundAt: ptr(ago(72 * time.Hour)), PendingCandidates: 0},
		},
		{
			name: "arrivals still coming: the debounce is re-armed, so nothing fires",
			state: domain.BucketState{
				Key: key(), PendingCandidates: 3,
				NewestCandidateAt: ago(5 * time.Second),
				OldestPendingAt:   ago(20 * time.Second),
			},
		},
		{
			name: "quiet for the period: debounce fires",
			state: domain.BucketState{
				Key: key(), PendingCandidates: 3,
				NewestCandidateAt: ago(30 * time.Second),
				OldestPendingAt:   ago(45 * time.Second),
			},
			wantFire: true, wantTrigger: domain.TriggerDebounce,
		},
		{
			// The boundary is inclusive. An exclusive one would leave a bucket
			// that went quiet exactly on the period waiting for the next tick,
			// which is a latency bug nobody would ever reproduce.
			name: "exactly the quiet period counts as quiet",
			state: domain.BucketState{
				Key: key(), PendingCandidates: 1,
				NewestCandidateAt: ago(policy.QuietPeriod),
				OldestPendingAt:   ago(policy.QuietPeriod),
			},
			wantFire: true, wantTrigger: domain.TriggerDebounce,
		},
		{
			// The starvation case, and the reason the ceiling exists. Arrivals
			// never stop, so the debounce alone would never fire and the bucket
			// would wait forever.
			name: "sustained arrivals hit the ceiling: cadence fires",
			state: domain.BucketState{
				Key: key(), PendingCandidates: 900,
				NewestCandidateAt: ago(time.Second),
				OldestPendingAt:   ago(6 * time.Minute),
			},
			wantFire: true, wantTrigger: domain.TriggerCadence, wantCeiling: true,
		},
		{
			name: "exactly the ceiling counts as stale",
			state: domain.BucketState{
				Key: key(), PendingCandidates: 5,
				NewestCandidateAt: ago(time.Second),
				OldestPendingAt:   ago(policy.StalenessCeiling),
			},
			wantFire: true, wantTrigger: domain.TriggerCadence, wantCeiling: true,
		},
		{
			// REPLAN wins over the cause, per ADR-0014's resolution of the
			// contract's non-orthogonal enum.
			name: "debounce over a bucket with a live plan reports REPLAN",
			state: domain.BucketState{
				Key: key(), PendingCandidates: 2, LivePlanID: &livePlan,
				NewestCandidateAt: ago(time.Minute),
				OldestPendingAt:   ago(2 * time.Minute),
			},
			wantFire: true, wantTrigger: domain.TriggerReplan,
		},
		{
			// And the case that made ByCeiling a field rather than a
			// derivation: this reports REPLAN too, and is a DIFFERENT round
			// from the one above.
			name: "ceiling over a bucket with a live plan also reports REPLAN",
			state: domain.BucketState{
				Key: key(), PendingCandidates: 400, LivePlanID: &livePlan,
				NewestCandidateAt: ago(time.Second),
				OldestPendingAt:   ago(10 * time.Minute),
			},
			wantFire: true, wantTrigger: domain.TriggerReplan, wantCeiling: true,
		},
		{
			// The guard against re-planning the same input forever.
			name: "candidates predating the last round are not dirt",
			state: domain.BucketState{
				Key: key(), PendingCandidates: 4,
				LastRoundAt:       ptr(ago(time.Minute)),
				NewestCandidateAt: ago(10 * time.Minute),
				OldestPendingAt:   ago(20 * time.Minute),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := policy.Decide(tt.state, now)

			if got.Fire != tt.wantFire {
				t.Fatalf("Fire = %v, want %v (%s)", got.Fire, tt.wantFire, got.Reason)
			}
			if got.Trigger != tt.wantTrigger {
				t.Errorf("Trigger = %q, want %q", got.Trigger, tt.wantTrigger)
			}
			if got.ByCeiling != tt.wantCeiling {
				t.Errorf("ByCeiling = %v, want %v", got.ByCeiling, tt.wantCeiling)
			}
			if got.Reason == "" {
				t.Error("no Reason; an operator asking why would have to re-derive it")
			}
		})
	}
}

// The quiet period must WIN when both conditions hold.
//
// Not arbitrary: the quiet period firing means arrivals have settled, so the
// round sees a complete burst. Letting the ceiling win there would open a round
// mid-burst for no reason, and — because causation_id would then be null — the
// audit record would claim nothing tipped it when something had.
func TestQuietPeriodWinsWhenBothConditionsHold(t *testing.T) {
	got := policy.Decide(domain.BucketState{
		Key: key(), PendingCandidates: 10,
		NewestCandidateAt: ago(time.Minute),      // quiet, >= 30s
		OldestPendingAt:   ago(10 * time.Minute), // also past the 5m ceiling
	}, now)

	if !got.Fire {
		t.Fatal("did not fire with both conditions met")
	}
	if got.ByCeiling {
		t.Error("the ceiling won over a satisfied quiet period; the round opens mid-burst and reports no causation")
	}
	if got.Trigger != domain.TriggerDebounce {
		t.Errorf("Trigger = %q, want %q", got.Trigger, domain.TriggerDebounce)
	}
}

// causation_id is the ONLY thing separating a cadence replan from a debounce
// replan, because REPLAN overloads the trigger field.
func TestCausationTracksTheCeilingNotTheTrigger(t *testing.T) {
	debounceReplan := policy.Decide(domain.BucketState{
		Key: key(), PendingCandidates: 1, LivePlanID: &livePlan,
		NewestCandidateAt: ago(time.Minute), OldestPendingAt: ago(2 * time.Minute),
	}, now)
	ceilingReplan := policy.Decide(domain.BucketState{
		Key: key(), PendingCandidates: 99, LivePlanID: &livePlan,
		NewestCandidateAt: ago(time.Second), OldestPendingAt: ago(time.Hour),
	}, now)

	if debounceReplan.Trigger != ceilingReplan.Trigger {
		t.Fatal("the two REPLAN rounds differ in Trigger; this test's premise is gone")
	}
	if !debounceReplan.CarriesCausation() {
		t.Error("a debounce-fired replan reported no causation; it would be indistinguishable from a cadence one")
	}
	if ceilingReplan.CarriesCausation() {
		t.Error("a ceiling-fired replan claimed a causing event; nothing tipped it")
	}
}

// A permanently held candidate must not re-fire its bucket forever.
//
// ADR-0014 clears dirtiness when a round RUNS, never when candidates are
// allocated, precisely so a candidate whose snapshot never arrives cannot
// hot-loop the advisory lock. This asserts the domain half of that: once
// LastRoundAt moves past the candidate, the bucket is clean even though the
// candidate is still sitting there.
func TestAHeldCandidateDoesNotReFireForever(t *testing.T) {
	arrived := ago(time.Hour)

	before := policy.Decide(domain.BucketState{
		Key: key(), PendingCandidates: 1,
		NewestCandidateAt: arrived, OldestPendingAt: arrived,
	}, now)
	if !before.Fire {
		t.Fatal("a bucket with a long-pending candidate did not fire once")
	}

	// The round ran. It allocated nothing — the candidate is held — but the
	// bucket is no longer dirty.
	after := policy.Decide(domain.BucketState{
		Key: key(), PendingCandidates: 0,
		LastRoundAt:       ptr(now),
		NewestCandidateAt: arrived, OldestPendingAt: arrived,
	}, now.Add(time.Hour))

	if after.Fire {
		t.Error("the bucket fired again with nothing new; a held candidate would hot-loop the advisory lock forever")
	}
}
