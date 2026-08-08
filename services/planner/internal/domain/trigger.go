package domain

import (
	"fmt"
	"time"
)

// The firing rule from ADR-0014, as a pure function.
//
// Pure on purpose. This is the decision that governs how often the system's one
// serialisation point is taken, and every interesting case about it — a burst
// that keeps re-arming, a bucket dirty for hours, a candidate held forever — is
// a statement about clocks. Testing those against a real database would mean
// sleeping, and a rule verified by sleeping is a rule verified slowly and
// flakily.

// Trigger values, mirroring the enum in
// contracts/events/planning.round.triggered.v1.schema.json.
const (
	TriggerCadence  = "CADENCE"
	TriggerDebounce = "OPPORTUNITY_DEBOUNCE"
	TriggerManual   = "MANUAL"
	TriggerReplan   = "REPLAN"
)

// TriggerPolicy is the configured firing rule.
type TriggerPolicy struct {
	// QuietPeriod is how long candidate arrivals must stop before a round
	// fires. Each arrival re-arms it.
	QuietPeriod time.Duration
	// StalenessCeiling fires a bucket that has been dirty this long regardless
	// of arrivals, so a sustained stream cannot starve it and a bucket left
	// dirty by a lost event is still recovered.
	StalenessCeiling time.Duration
}

// Validate refuses a policy that cannot behave as ADR-0014 describes.
func (p TriggerPolicy) Validate() error {
	if p.QuietPeriod <= 0 {
		return fmt.Errorf("%w: quiet period must be positive, got %s", ErrInvalid, p.QuietPeriod)
	}
	if p.StalenessCeiling <= 0 {
		return fmt.Errorf("%w: staleness ceiling must be positive, got %s", ErrInvalid, p.StalenessCeiling)
	}
	// The relation is the whole design. With the ceiling at or below the quiet
	// period the debounce can never win, so every round reports CADENCE and the
	// burst absorption the quiet period exists for silently never happens —
	// a system that still works, still plans, and has lost half its rule.
	if p.StalenessCeiling <= p.QuietPeriod {
		return fmt.Errorf(
			"%w: staleness ceiling %s must exceed the quiet period %s, or the debounce can never fire",
			ErrInvalid, p.StalenessCeiling, p.QuietPeriod)
	}
	return nil
}

// BucketState is what the planner knows about one candidate bucket.
//
// Every field is derived, not stored. ADR-0014 keeps no is_dirty column and no
// timer state on disk, so a planner restart loses nothing and two planners
// cannot disagree about what is pending.
type BucketState struct {
	Key RoundKey

	// LastRoundAt is when a round last opened over this bucket. Nil means the
	// bucket has never been planned.
	LastRoundAt *time.Time

	// NewestCandidateAt is the most recent arrival for this bucket. The quiet
	// period is measured from it.
	NewestCandidateAt time.Time

	// OldestPendingAt is the earliest arrival NOT yet covered by a round — the
	// instant the bucket became dirty. The ceiling is measured from it.
	OldestPendingAt time.Time

	// PendingCandidates counts arrivals since the last round.
	PendingCandidates int

	// TippingEventID is the event that delivered the newest candidate.
	//
	// It becomes causation_id when an arrival fired the round. Carried from the
	// sweep rather than re-derived under the lock: causation names WHAT TIPPED
	// THE DECISION, and the decision was made on the sweep's reading. Re-reading
	// it under the lock would name whatever arrived last instead, which is a
	// different and untrue claim.
	TippingEventID string

	// LivePlanID is the plan this round would supersede, or nil if the bucket
	// has never been planned.
	//
	// The ID rather than a boolean, because the database will not accept the
	// boolean's worth of information on its own: rounds_supersedes_iff_replan
	// requires supersedes_plan_id to be non-null exactly when trigger is
	// REPLAN. Carrying "there is a live plan" without carrying WHICH would mean
	// deciding REPLAN here and discovering at INSERT time that the round cannot
	// be written.
	LivePlanID *string
}

// Replanning reports whether this round replaces a live plan.
func (s BucketState) Replanning() bool { return s.LivePlanID != nil }

// Decision is the outcome of applying the rule to one bucket.
type Decision struct {
	Fire bool

	// Trigger is the contract enum value for the round. Empty when Fire is
	// false.
	Trigger string

	// ByCeiling records that the staleness ceiling fired this round rather than
	// an arrival tipping it.
	//
	// Carried explicitly rather than derived from Trigger, and that is not
	// defensive coding — deriving it is WRONG. REPLAN overloads the trigger
	// field, so a ceiling-fired round over a bucket with a live plan reports
	// REPLAN, indistinguishable from a debounce-fired one. Since causation_id
	// is the only thing that separates those two on the wire, inferring this
	// from Trigger would make a cadence replan and a debounce replan identical
	// forever.
	ByCeiling bool

	// Reason is for logs, not for the wire. An operator asking "why did that
	// fire?" should not have to re-derive it.
	Reason string
}

// Decide applies ADR-0014's rule to one bucket.
//
// A bucket is dirty when a candidate arrived after the most recent round over
// it. A dirty bucket fires when arrivals have stopped for the quiet period, or
// when it has been dirty for the ceiling, whichever comes first. A clean bucket
// never fires — the ceiling is a bound on staleness, not a heartbeat, so an
// idle constellation does no work at all.
func (p TriggerPolicy) Decide(state BucketState, now time.Time) Decision {
	if state.PendingCandidates == 0 {
		return Decision{Reason: "clean: no candidates have arrived since the last round"}
	}

	// Belt and braces. PendingCandidates is computed by the query as "arrived
	// after LastRoundAt", so these should agree — but if a clock skewed or the
	// query changed, firing on a bucket whose candidates predate its last round
	// would re-plan the same input forever.
	if state.LastRoundAt != nil && !state.NewestCandidateAt.After(*state.LastRoundAt) {
		return Decision{Reason: "clean: every candidate predates the last round"}
	}

	trigger := TriggerDebounce
	if state.Replanning() {
		// REPLAN is not orthogonal to CADENCE and OPPORTUNITY_DEBOUNCE, and
		// ADR-0014 resolved the overlap using the contract's own wording:
		// supersedes_plan_id is set "when trigger is REPLAN". So REPLAN wins
		// whenever a live plan is being replaced, and WHAT FIRED THE ROUND is
		// recovered from causation_id instead — non-null means an opportunities
		// event tipped it, null means the ceiling did.
		trigger = TriggerReplan
	}

	if quiet := now.Sub(state.NewestCandidateAt); quiet >= p.QuietPeriod {
		return Decision{
			Fire:    true,
			Trigger: trigger,
			Reason: fmt.Sprintf("quiet for %s (>= %s) with %d candidates pending",
				quiet.Round(time.Millisecond), p.QuietPeriod, state.PendingCandidates),
		}
	}

	if dirty := now.Sub(state.OldestPendingAt); dirty >= p.StalenessCeiling {
		// The ceiling fired, so nothing tipped it. On a bucket with no live
		// plan that is a CADENCE round; with one, REPLAN still wins per above,
		// and causation_id stays null to say the ceiling did it.
		ceilingTrigger := TriggerCadence
		if state.Replanning() {
			ceilingTrigger = TriggerReplan
		}
		return Decision{
			Fire:      true,
			Trigger:   ceilingTrigger,
			ByCeiling: true,
			Reason: fmt.Sprintf("dirty for %s (>= ceiling %s) with %d candidates pending",
				dirty.Round(time.Millisecond), p.StalenessCeiling, state.PendingCandidates),
		}
	}

	return Decision{
		Reason: fmt.Sprintf("waiting: last arrival %s ago, dirty for %s",
			now.Sub(state.NewestCandidateAt).Round(time.Millisecond),
			now.Sub(state.OldestPendingAt).Round(time.Millisecond)),
	}
}

// CarriesCausation reports whether the round should name the event that tipped
// it.
//
// causation_id is null when the ceiling fired and the tipping event's id
// otherwise — the contract says "Null for CADENCE", and ADR-0014 extends that
// to any ceiling-fired round, including the ones REPLAN renames.
func (d Decision) CarriesCausation() bool {
	return d.Fire && !d.ByCeiling
}
