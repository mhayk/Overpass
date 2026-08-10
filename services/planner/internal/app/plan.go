package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mhayk/overpass/gen/go/events"

	"github.com/mhayk/overpass/services/planner/internal/domain"
	"github.com/mhayk/overpass/services/planner/internal/port"
)

// buildPlan turns what the round read under the lock into a committed plan and
// its events.
//
// This is where the interface earns itself: the policy is a pure function of
// the Problem, and everything around it — scoring, event assembly, metrics — is
// the same regardless of which policy ran. The benchmark swaps the policy; the
// plumbing stays identical.
func (t *Trigger) buildPlan(
	ctx context.Context,
	round port.Round,
	inputs port.RoundInputs,
	now time.Time,
	log *slog.Logger,
) (*port.PlanCommit, error) {
	problem := domain.Problem{
		Key:       round.Key,
		BucketEnd: round.BucketEnd,
		Profile:   inputs.Profile,
		Now:       now,
	}

	// Score the joinable rows. The fairness model runs HERE, once, so no policy
	// ever sees a priority tier — ADR-0007's separation, enforced by the data
	// each layer is given rather than by convention.
	for _, jc := range inputs.Joinable {
		attitude, err := domain.AttitudeFor(nil, incidence(jc), slantRange(jc), lookSide(jc), jc.Mode)
		if err != nil {
			// Defensive: feasibility publishes the required geometry fields, so
			// an underivable roll means an inconsistent payload. The candidate
			// is skipped rather than guessed at — a zero roll is nadir, which
			// is a perfectly plausible-looking answer and completely wrong. Its
			// request still gets an outcome through conservation below.
			log.Warn("candidate skipped: roll underivable",
				slog.String("opportunity_id", jc.OpportunityID),
				slog.Any("error", err))
			continue
		}
		if supplied := suppliedRoll(jc); supplied != nil {
			attitude.RollDeg = *supplied
		}

		snapshot := domain.Snapshot{
			RequestID:    jc.RequestID,
			CustomerID:   jc.CustomerID,
			PriorityTier: jc.PriorityTier,
			BidCredits:   jc.BidCredits,
			SubmittedAt:  jc.SubmittedAt,
		}
		problem.Candidates = append(problem.Candidates, domain.ScoredCandidate{
			Candidate:      jc.Candidate,
			CustomerID:     jc.CustomerID,
			EffectiveValue: t.fairness.EffectiveValue(snapshot, now),
			Deadline:       jc.Deadline,
			Attitude:       attitude,
		})
	}

	started := time.Now()
	plan := t.allocator.Allocate(problem)
	allocationMs := time.Since(started).Milliseconds()

	// Conservation, checked BEFORE anything reaches the database — and against
	// the round's ANNOUNCED ledger, not the scored subset. The difference is
	// real and a test caught it: a request whose every candidate was skipped
	// before scoring never enters the Problem, so validating against the
	// Problem would let it vanish while candidate_request_ids promises it an
	// outcome.
	competed := inputs.CandidateRequestIDs
	ensureConservation(&plan, problem, competed)
	if err := plan.Validate(competed); err != nil {
		return nil, fmt.Errorf("policy %s broke conservation: %w", t.allocator.Name(), err)
	}

	for i := range plan.Acquisitions {
		plan.Acquisitions[i].AcquisitionID = uuid.NewString()
	}

	planID := uuid.NewString()
	markSuperseded(&plan, inputs, planID)

	commit := &port.PlanCommit{
		PlanID:               planID,
		RoundID:              round.RoundID,
		SupersedesPlanID:     inputs.LivePlanID,
		SupersededRowVersion: inputs.LivePlanRowVersion,
		PlanVersion:          inputs.NextPlanVersion,
		Policy:               t.allocator.Name(),
		CommittedAt:          now,
		Acquisitions:         plan.Acquisitions,
		Unfulfilled:          plan.Unfulfilled,
		PlanEventID:          uuid.NewString(),
	}

	metrics := planMetrics(plan, inputs, allocationMs)

	// Export the same numbers the contract puts on the event. They were
	// already computed and already required; until #53 the only consumer was
	// the event payload, so an operator could not see plan value, allocation
	// latency or utilisation without reading a message off the wire.
	t.instruments.RecordPlan(ctx, t.allocator.Name(), round.Key.SatelliteID, metrics)
	for _, u := range plan.Unfulfilled {
		t.instruments.RecordUnfulfilled(ctx, u.ReasonCode)
	}

	metricsJSON, err := json.Marshal(metrics)
	if err != nil {
		return nil, fmt.Errorf("encoding plan metrics: %w", err)
	}
	commit.MetricsJSON = metricsJSON

	payload, err := buildPlanCommittedEvent(round, *commit, metrics)
	if err != nil {
		return nil, err
	}
	commit.PlanPayload = payload

	for _, u := range plan.Unfulfilled {
		event, err := buildUnfulfilledEvent(round, u, inputs, now)
		if err != nil {
			return nil, err
		}
		commit.UnfulfilledEvents = append(commit.UnfulfilledEvents, event)
	}
	return commit, nil
}

// ensureConservation adds an unfulfilment for any request whose every candidate
// was skipped before the policy saw it — the underivable-roll path. The policy
// cannot report what it was never given, and the contract does not care whose
// fault the gap is.
func ensureConservation(plan *domain.Plan, problem domain.Problem, competed []string) {
	seen := map[string]bool{}
	for _, a := range plan.Acquisitions {
		seen[a.RequestID] = true
	}
	for _, u := range plan.Unfulfilled {
		seen[u.RequestID] = true
	}
	for _, requestID := range competed {
		if !seen[requestID] {
			plan.Unfulfilled = append(plan.Unfulfilled, domain.Unfulfilment{
				RequestID:  requestID,
				ReasonCode: domain.ReasonNoOpportunity,
				Explanation: "every candidate for this request was unusable in this round; " +
					"it stays in contention for later ones",
			})
		}
	}
}

// markSuperseded rewrites the outcome for every request that held a slot in
// the plan being replaced and is absent from this one.
//
// SUPERSEDED wins over whatever competitive reason the policy produced, because
// it is the truthful description of what the customer experiences: they HAD a
// slot and the re-plan dropped them. The competitive detail — the shortfall,
// the budget numbers — stays in the explanation, so "why did the re-plan drop
// me?" is still answerable; the code names the event, the numbers name the
// cause.
//
// A holder that is not even in the candidate ledger any more — its options
// expired since the plan it won a place in — gets an event APPENDED. That is
// beyond the round's announced conservation set, deliberately: the ledger
// promises outcomes for who competed, and this promise is older, made when the
// plan committed.
func markSuperseded(plan *domain.Plan, inputs port.RoundInputs, planID string) {
	if inputs.LivePlanID == nil || len(inputs.LivePlanHolders) == 0 {
		return
	}
	winners := map[string]bool{}
	for _, a := range plan.Acquisitions {
		winners[a.RequestID] = true
	}
	reported := map[string]int{}
	for i, u := range plan.Unfulfilled {
		reported[u.RequestID] = i
	}

	for _, holder := range inputs.LivePlanHolders {
		if winners[holder] {
			continue // still flying; nothing was lost
		}
		if i, ok := reported[holder]; ok {
			plan.Unfulfilled[i].ReasonCode = domain.ReasonSuperseded
			plan.Unfulfilled[i].Detail.SupersededByPlanID = planID
			continue
		}
		plan.Unfulfilled = append(plan.Unfulfilled, domain.Unfulfilment{
			RequestID:  holder,
			ReasonCode: domain.ReasonSuperseded,
			Explanation: "held an acquisition in the replaced plan; the re-plan did not include it " +
				"and no current candidates remain",
			Detail: domain.RefusalDetail{SupersededByPlanID: planID},
		})
	}
}

// planMetrics builds the metrics object the contract requires on every
// committed plan.
func planMetrics(plan domain.Plan, inputs port.RoundInputs, allocationMs int64) events.PlanCommittedDataMetrics {
	var dutyUsed, totalSlew float64
	for _, a := range plan.Acquisitions {
		dutyUsed += a.DutyCycleCostS
		if a.SlewFromPreviousS != nil {
			totalSlew += *a.SlewFromPreviousS
		}
	}

	metrics := events.PlanCommittedDataMetrics{
		TotalPlanValueCredits:     events.Credits(plan.Value()),
		RequestsFulfilled:         len(plan.Acquisitions),
		CandidateOpportunityCount: inputs.CandidateOpportunityCount,
		DutyCycleUsedS:            events.DurationSeconds(dutyUsed),
		DutyCycleBudgetS:          events.DurationSeconds(inputs.Profile.DutyCycleBudgetS),
		AllocationDurationMs:      events.DurationMillis(allocationMs),
	}
	unfulfilled := len(plan.Unfulfilled)
	metrics.RequestsUnfulfilled = &unfulfilled
	if inputs.Profile.DutyCycleBudgetS > 0 {
		utilisation := events.Ratio(dutyUsed / inputs.Profile.DutyCycleBudgetS)
		metrics.SatelliteUtilisationRatio = &utilisation
	}
	slew := events.DurationSeconds(totalSlew)
	metrics.TotalSlewTimeS = &slew
	return metrics
}

// The geometry accessors below read the verbatim AccessGeometry blob. The
// contract requires these fields on every opportunity, so a missing one is an
// inconsistent payload and surfaces as an underivable roll, not a zero.

func incidence(jc port.JoinableCandidate) float64 {
	return geometryField(jc.GeometryJSON, "incidence_angle_deg")
}

func slantRange(jc port.JoinableCandidate) float64 {
	return geometryField(jc.GeometryJSON, "slant_range_km")
}

func lookSide(jc port.JoinableCandidate) string {
	var g struct {
		LookSide string `json:"look_side"`
	}
	_ = json.Unmarshal(jc.GeometryJSON, &g) //nolint:errcheck // a bad blob yields "", which AttitudeFor refuses loudly
	return g.LookSide
}

func suppliedRoll(jc port.JoinableCandidate) *float64 {
	var g struct {
		RollAngleDeg *float64 `json:"roll_angle_deg"`
	}
	_ = json.Unmarshal(jc.GeometryJSON, &g) //nolint:errcheck // absent is a legal state; the derivation covers it
	return g.RollAngleDeg
}

func geometryField(blob []byte, field string) float64 {
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		return 0 // AttitudeFor refuses zero incidence, so the failure is loud
	}
	if v, ok := m[field].(float64); ok {
		return v
	}
	return 0
}
