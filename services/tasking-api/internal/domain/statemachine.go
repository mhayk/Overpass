package domain

import (
	"fmt"
	"time"
)

// The request lifecycle, as a table rather than as scattered conditionals.
//
// The table is directly comparable with the diagram in the OpenAPI description,
// so drift between what the contract promises and what the code does is visible
// by reading two things side by side. Twenty `if` statements spread across three
// consumers are not comparable with anything.

// Trigger is what causes a transition. One per event the consumers see.
type Trigger string

const (
	TriggerOpportunitiesFound Trigger = "OPPORTUNITIES_FOUND" // feasibility.opportunities.computed.v1
	TriggerFeasibilityFailed  Trigger = "FEASIBILITY_FAILED"  // feasibility.failed.v1
	TriggerPlanCommitted      Trigger = "PLAN_COMMITTED"      // planning.plan.committed.v1
	TriggerRoundLost          Trigger = "ROUND_LOST"          // planning.request.unfulfilled.v1
	TriggerDeadlinePassed     Trigger = "DEADLINE_PASSED"     // the expiry sweep
	TriggerAcquired           Trigger = "ACQUIRED"            // acquisition.executed.v1
	TriggerCancelled          Trigger = "CANCELLED"           // POST /cancellation
)

// transition is one row of the table.
type transition struct {
	from    State
	trigger Trigger
	to      State
}

// transitions is the whole lifecycle.
//
// Read it against the diagram in contracts/openapi/tasking-api.v1.yaml. If they
// disagree, one of them is wrong and the disagreement is visible.
var transitions = []transition{
	{StateReceived, TriggerOpportunitiesFound, StateAwaitingPlanning},
	{StateReceived, TriggerFeasibilityFailed, StateInfeasible},
	{StateReceived, TriggerDeadlinePassed, StateExpired},
	{StateReceived, TriggerCancelled, StateCancelled},

	{StateAwaitingPlanning, TriggerPlanCommitted, StatePlanned},
	// Losing a round is NOT a failure state. The request ages, gains fairness
	// weight, and competes again — which is only expressible if the machine
	// says so. A terminal LOST state would quietly make the fairness model in
	// M2-09 impossible.
	{StateAwaitingPlanning, TriggerRoundLost, StateAwaitingPlanning},
	{StateAwaitingPlanning, TriggerDeadlinePassed, StateExpired},
	{StateAwaitingPlanning, TriggerCancelled, StateCancelled},

	{StatePlanned, TriggerAcquired, StateAcquired},
	// A committed plan can be superseded, and the request goes back to
	// competing. ADR-0012 keeps the superseded acquisition; this keeps the
	// request alive to win a later round.
	{StatePlanned, TriggerRoundLost, StateAwaitingPlanning},
	{StatePlanned, TriggerDeadlinePassed, StateExpired},
	{StatePlanned, TriggerCancelled, StateCancelled},
}

// terminal states accept no further transitions.
var terminal = map[State]bool{
	StateAcquired:   true,
	StateInfeasible: true,
	StateRejected:   true,
	StateExpired:    true,
	StateCancelled:  true,
}

// IsTerminal reports whether a state is final.
func IsTerminal(s State) bool { return terminal[s] }

// TransitionError describes a refused transition.
//
// An error rather than a silent no-op. An ignored illegal transition is a bug
// that has already happened and left no evidence — the request sits in a state
// nobody can explain, and the event that would have explained it is gone.
type TransitionError struct {
	From    State
	Trigger Trigger
	Reason  string
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("cannot apply %s from %s: %s", e.Trigger, e.From, e.Reason)
}

// Transition is the result of applying a trigger.
type Transition struct {
	From State
	To   State
	// Applied is false for a legal no-op — a stale event that arrived after a
	// newer one. Distinct from an error: nothing is wrong, there is just
	// nothing to do.
	Applied bool
}

// Apply moves a request from one state to another.
//
// `eventAt` and `lastTransitionAt` are what make out-of-order arrival safe.
// JetStream orders per subject, consumers process concurrently, and redelivery
// happens out of band — so arrival order is not causal order and nothing here
// assumes it is.
func Apply(
	current State,
	lastTransitionAt time.Time,
	trigger Trigger,
	eventAt time.Time,
) (Transition, error) {
	if IsTerminal(current) {
		// Terminal means terminal. An ACQUIRED request that receives a stale
		// ROUND_LOST must not reopen — the image was taken.
		return Transition{From: current, To: current}, &TransitionError{
			From: current, Trigger: trigger,
			Reason: "state is terminal",
		}
	}

	// A strictly older event is stale, not illegal. This is the ordering guard:
	// two events racing to move the same request are resolved by when they
	// HAPPENED, not by when they arrived.
	//
	// Equal timestamps apply, deliberately. Clock resolution can collapse two
	// genuinely distinct events onto the same instant, and dropping the second
	// would lose a real transition — the state guard above is what stops that
	// being dangerous.
	if eventAt.Before(lastTransitionAt) {
		return Transition{From: current, To: current, Applied: false}, nil
	}

	for _, t := range transitions {
		if t.from == current && t.trigger == trigger {
			return Transition{From: current, To: t.to, Applied: true}, nil
		}
	}

	return Transition{From: current, To: current}, &TransitionError{
		From: current, Trigger: trigger,
		Reason: "no such transition in the lifecycle",
	}
}

// LegalTriggers lists what may be applied from a state.
//
// Exported for the "why is my request stuck?" answer: a customer support
// question is far easier to answer with "it is PLANNED, and only ACQUIRED,
// ROUND_LOST, DEADLINE_PASSED or CANCELLED move it" than with a reading of the
// source.
func LegalTriggers(from State) []Trigger {
	if IsTerminal(from) {
		return nil
	}
	var out []Trigger
	for _, t := range transitions {
		if t.from == from {
			out = append(out, t.trigger)
		}
	}
	return out
}

// AllStates is every state in the lifecycle, for validation and for tests that
// must cover all of them rather than the ones somebody remembered.
func AllStates() []State {
	return []State{
		StateReceived, StateAwaitingPlanning, StatePlanned, StateAcquired,
		StateInfeasible, StateRejected, StateExpired, StateCancelled,
	}
}

// AllTriggers is every trigger, for the same reason.
func AllTriggers() []Trigger {
	return []Trigger{
		TriggerOpportunitiesFound, TriggerFeasibilityFailed, TriggerPlanCommitted,
		TriggerRoundLost, TriggerDeadlinePassed, TriggerAcquired, TriggerCancelled,
	}
}
