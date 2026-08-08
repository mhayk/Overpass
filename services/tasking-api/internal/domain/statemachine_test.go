package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/tasking-api/internal/domain"
)

var (
	t0 = time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Hour)
	t2 = t0.Add(2 * time.Hour)
)

func TestEveryLegalTransition(t *testing.T) {
	// The table, asserted row by row. Compare it against the lifecycle diagram
	// in contracts/openapi/tasking-api.v1.yaml — that comparison is the whole
	// point of holding transitions as data.
	cases := []struct {
		from    domain.State
		trigger domain.Trigger
		want    domain.State
	}{
		{domain.StateReceived, domain.TriggerOpportunitiesFound, domain.StateAwaitingPlanning},
		{domain.StateReceived, domain.TriggerFeasibilityFailed, domain.StateInfeasible},
		{domain.StateReceived, domain.TriggerDeadlinePassed, domain.StateExpired},
		{domain.StateReceived, domain.TriggerCancelled, domain.StateCancelled},

		{domain.StateAwaitingPlanning, domain.TriggerPlanCommitted, domain.StatePlanned},
		{domain.StateAwaitingPlanning, domain.TriggerRoundLost, domain.StateAwaitingPlanning},
		{domain.StateAwaitingPlanning, domain.TriggerDeadlinePassed, domain.StateExpired},
		{domain.StateAwaitingPlanning, domain.TriggerCancelled, domain.StateCancelled},

		{domain.StatePlanned, domain.TriggerAcquired, domain.StateAcquired},
		{domain.StatePlanned, domain.TriggerRoundLost, domain.StateAwaitingPlanning},
		{domain.StatePlanned, domain.TriggerDeadlinePassed, domain.StateExpired},
		{domain.StatePlanned, domain.TriggerCancelled, domain.StateCancelled},
	}

	for _, tc := range cases {
		t.Run(string(tc.from)+"/"+string(tc.trigger), func(t *testing.T) {
			got, err := domain.Apply(tc.from, t0, tc.trigger, t1)
			if err != nil {
				t.Fatalf("legal transition refused: %v", err)
			}
			if !got.Applied || got.To != tc.want {
				t.Fatalf("got %s (applied=%v), want %s", got.To, got.Applied, tc.want)
			}
		})
	}
}

func TestEveryIllegalTransitionIsRefused(t *testing.T) {
	// Exhaustive over the whole matrix rather than over the pairs somebody
	// thought of. An illegal transition that is silently ignored is a bug that
	// has already happened and left no evidence.
	legal := map[string]bool{}
	for _, from := range domain.AllStates() {
		for _, trigger := range domain.LegalTriggers(from) {
			legal[string(from)+"/"+string(trigger)] = true
		}
	}

	checked := 0
	for _, from := range domain.AllStates() {
		for _, trigger := range domain.AllTriggers() {
			if legal[string(from)+"/"+string(trigger)] {
				continue
			}
			checked++
			got, err := domain.Apply(from, t0, trigger, t1)
			if err == nil {
				t.Errorf("%s + %s was accepted, moving to %s", from, trigger, got.To)
			}
			var te *domain.TransitionError
			if !errors.As(err, &te) {
				t.Errorf("%s + %s returned %T, want a TransitionError a caller can log", from, trigger, err)
			}
			if got.To != from {
				t.Errorf("a refused transition moved the state from %s to %s", from, got.To)
			}
		}
	}
	// Without this the loop could pass having checked nothing.
	if checked < 20 {
		t.Fatalf("only %d illegal combinations checked; the matrix looks wrong", checked)
	}
}

func TestLosingARoundIsNotAFailure(t *testing.T) {
	// The request ages, gains fairness weight, and competes again. A terminal
	// LOST state would quietly make the fairness model in M2-09 impossible.
	got, err := domain.Apply(domain.StateAwaitingPlanning, t0, domain.TriggerRoundLost, t1)
	if err != nil {
		t.Fatalf("losing a round errored: %v", err)
	}
	if got.To != domain.StateAwaitingPlanning {
		t.Fatalf("a losing request went to %s", got.To)
	}
	if domain.IsTerminal(got.To) {
		t.Fatal("a losing request reached a terminal state")
	}
}

func TestASupersededPlanReturnsTheRequestToCompeting(t *testing.T) {
	// ADR-0012 keeps the superseded acquisition. This keeps the REQUEST alive
	// to win a later round, which is the other half of that decision.
	got, err := domain.Apply(domain.StatePlanned, t0, domain.TriggerRoundLost, t1)
	if err != nil {
		t.Fatalf("supersession errored: %v", err)
	}
	if got.To != domain.StateAwaitingPlanning {
		t.Fatalf("a superseded request went to %s", got.To)
	}
}

func TestTerminalStatesAcceptNothing(t *testing.T) {
	for _, state := range domain.AllStates() {
		if !domain.IsTerminal(state) {
			continue
		}
		for _, trigger := range domain.AllTriggers() {
			if _, err := domain.Apply(state, t0, trigger, t1); err == nil {
				t.Errorf("terminal %s accepted %s", state, trigger)
			}
		}
	}
}

func TestAnAcquiredRequestDoesNotReopenOnAStaleLoss(t *testing.T) {
	// The image was taken. A ROUND_LOST arriving afterwards — which it can,
	// because redelivery happens out of band — must not undo that.
	_, err := domain.Apply(domain.StateAcquired, t2, domain.TriggerRoundLost, t1)
	if err == nil {
		t.Fatal("an acquired request was reopened")
	}
}

func TestAStaleEventIsANoOpAndNotAnError(t *testing.T) {
	// Arrival order is not causal order: JetStream orders per subject,
	// consumers run concurrently, and redelivery is out of band. An older event
	// arriving late is normal, not a fault.
	got, err := domain.Apply(domain.StateAwaitingPlanning, t2, domain.TriggerPlanCommitted, t1)
	if err != nil {
		t.Fatalf("a stale event was treated as an error: %v", err)
	}
	if got.Applied {
		t.Fatal("a stale event was applied")
	}
	if got.To != domain.StateAwaitingPlanning {
		t.Fatalf("a stale event moved the state to %s", got.To)
	}
}

func TestAnEventAtTheSameInstantIsApplied(t *testing.T) {
	// Clock resolution can collapse two genuinely distinct events onto the same
	// instant. Dropping the second would lose a real transition, and the state
	// guard is what stops that being dangerous.
	got, err := domain.Apply(domain.StateReceived, t1, domain.TriggerOpportunitiesFound, t1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Applied {
		t.Fatal("an event at the same instant was dropped")
	}
}

func TestOutOfOrderArrivalConvergesOnTheSameState(t *testing.T) {
	// The property that matters under redelivery: applying the same events in
	// either arrival order lands in the same place, because the decision is
	// made on occurred_at.
	type step struct {
		trigger domain.Trigger
		at      time.Time
	}
	found := step{domain.TriggerOpportunitiesFound, t1}
	planned := step{domain.TriggerPlanCommitted, t2}

	run := func(steps ...step) domain.State {
		state, last := domain.StateReceived, t0
		for _, s := range steps {
			got, err := domain.Apply(state, last, s.trigger, s.at)
			if err != nil {
				continue // an out-of-order arrival can be illegal from here
			}
			if got.Applied {
				state, last = got.To, s.at
			}
		}
		return state
	}

	inOrder := run(found, planned)
	if inOrder != domain.StatePlanned {
		t.Fatalf("in order landed on %s", inOrder)
	}

	// Reversed, the PLAN_COMMITTED arrives first and is illegal from RECEIVED,
	// so it is refused; the redelivery that follows the opportunities event is
	// what completes the sequence. Modelled here as the broker retrying it.
	reversed := run(planned, found, planned)
	if reversed != domain.StatePlanned {
		t.Fatalf("out of order landed on %s, not %s", reversed, domain.StatePlanned)
	}
}

func TestLegalTriggersAnswersTheSupportQuestion(t *testing.T) {
	// "Why is my request stuck?" is far easier to answer with this than with a
	// reading of the source.
	got := domain.LegalTriggers(domain.StatePlanned)
	if len(got) != 4 {
		t.Fatalf("PLANNED has %d legal triggers: %v", len(got), got)
	}
	if domain.LegalTriggers(domain.StateAcquired) != nil {
		t.Fatal("a terminal state reported legal triggers")
	}
}

func TestTheTableCoversEveryNonTerminalState(t *testing.T) {
	// A state with no way out is a state requests get stuck in, and the only
	// states allowed to be dead ends are the terminal ones.
	for _, state := range domain.AllStates() {
		if domain.IsTerminal(state) {
			continue
		}
		if len(domain.LegalTriggers(state)) == 0 {
			t.Errorf("%s is not terminal and has no way out", state)
		}
	}
}

func TestEveryNonTerminalStateCanExpireOrBeCancelled(t *testing.T) {
	// Two escape hatches that must exist everywhere a request can wait: a
	// deadline that passes, and a customer who changes their mind. A state
	// missing either strands requests in it.
	for _, state := range domain.AllStates() {
		if domain.IsTerminal(state) {
			continue
		}
		for _, trigger := range []domain.Trigger{domain.TriggerDeadlinePassed, domain.TriggerCancelled} {
			if _, err := domain.Apply(state, t0, trigger, t1); err != nil {
				t.Errorf("%s cannot handle %s: %v", state, trigger, err)
			}
		}
	}
}
