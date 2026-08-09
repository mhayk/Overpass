package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mhayk/overpass/services/planner/internal/adapter/postgres"
	"github.com/mhayk/overpass/services/planner/internal/domain"
	"github.com/mhayk/overpass/services/planner/internal/port"
)

// The plan commit is one transaction under the advisory lock: the round row,
// the plan, its acquisitions, the demotion of anything superseded, and every
// outbox row. All of it or none of it.
//
// Against a real Postgres because the claims are about a transaction and about
// constraints — including ADR-0012's deferred exclusion constraint, which no
// fake can be wrong about the way the real one can.

func acquisition(requestID, opportunityID, customerID string, start time.Time, d time.Duration) domain.ScheduledAcquisition {
	return domain.ScheduledAcquisition{
		// App-side now, matching buildPlan: the event carries the id, so the
		// row cannot invent its own.
		AcquisitionID:       uuid.NewString(),
		OpportunityID:       opportunityID,
		RequestID:           requestID,
		CustomerID:          customerID,
		Mode:                "STRIPMAP",
		OrbitNumber:         47110,
		Start:               start,
		End:                 start.Add(d),
		Attitude:            domain.Attitude{RollDeg: 20, Mode: "STRIPMAP"},
		DutyCycleCostS:      d.Seconds(),
		AwardedValueCredits: 500,
		FootprintGeoJSON:    []byte(`{"type":"Polygon","coordinates":[[[0,0],[0,1],[1,1],[1,0],[0,0]]]}`),
		GeometryJSON:        []byte(`{"incidence_angle_deg":32.5,"look_side":"RIGHT"}`),
		QualityScore:        0.87,
	}
}

// commitOnce opens a round over a seeded bucket and commits the plan the caller
// builds from what the round read under the lock.
func commitOnce(
	t *testing.T,
	p *pgxpool.Pool,
	key domain.RoundKey,
	bucketEnd time.Time,
	build func(port.RoundInputs) port.PlanCommit,
) (string, error) {
	t.Helper()
	rounds := postgres.NewRounds(p)
	planID := ""

	_, err := rounds.OpenRound(context.Background(), key, bucketEnd,
		func(inputs port.RoundInputs) (port.RoundOutcome, error) {
			plan := build(inputs)
			planID = plan.PlanID
			roundID := uuid.NewString()
			plan.RoundID = roundID
			return port.RoundOutcome{
				Round: port.Round{
					RoundID: roundID, EventID: uuid.NewString(), CorrelationID: uuid.NewString(),
					Key: key, BucketEnd: bucketEnd,
					Trigger: triggerFor(inputs), Policy: plan.Policy,
					CandidateOpportunityCount: inputs.CandidateOpportunityCount,
					CandidateRequestIDs:       inputs.CandidateRequestIDs,
					DutyCycleBudgetS:          inputs.DutyCycleBudgetS,
					SupersedesPlanID:          inputs.LivePlanID,
					TriggeredAt:               time.Now().UTC(),
				},
				RoundPayload: []byte(`{"event_id":"x"}`),
				Plan:         &plan,
			}, nil
		})
	return planID, err
}

func triggerFor(inputs port.RoundInputs) string {
	if inputs.LivePlanID != nil {
		return domain.TriggerReplan
	}
	return domain.TriggerCadence
}

func basePlan(inputs port.RoundInputs, customer string, acquisitions ...domain.ScheduledAcquisition) port.PlanCommit {
	return port.PlanCommit{
		PlanID:               uuid.NewString(),
		SupersedesPlanID:     inputs.LivePlanID,
		SupersededRowVersion: inputs.LivePlanRowVersion,
		PlanVersion:          inputs.NextPlanVersion,
		Policy:               "GREEDY_BY_BID",
		MetricsJSON:          []byte(`{"plan_value_credits":500}`),
		CommittedAt:          time.Now().UTC(),
		Acquisitions:         acquisitions,
		PlanEventID:          uuid.NewString(),
		PlanPayload:          []byte(`{"event_id":"plan"}`),
	}
}

func TestCommittingAPlanWritesEverythingAtomically(t *testing.T) {
	p := pool(t)
	ctx := context.Background()

	bucketStart := domain.BucketStart(time.Now().UTC().Add(900*time.Hour), bucketDuration)
	bucketEnd := bucketStart.Add(bucketDuration)
	satellite := fmt.Sprintf("SAT-PA%d", time.Now().UnixNano()%100000)
	customer, requestID := seedBucketWithRequest(t, p, satellite, bucketStart)
	key := domain.RoundKey{SatelliteID: satellite, BucketStart: bucketStart}

	planID, err := commitOnce(t, p, key, bucketEnd, func(inputs port.RoundInputs) port.PlanCommit {
		plan := basePlan(inputs, customer,
			acquisition(requestID, uuid.NewString(), customer, bucketStart.Add(time.Minute), 30*time.Second))
		plan.UnfulfilledEvents = []port.OutboxEvent{{
			EventID: uuid.NewString(), EventType: "planning.request.unfulfilled.v1",
			Subject: "planning.request.unfulfilled.v1", Payload: []byte(`{"event_id":"u"}`),
		}}
		return plan
	})
	if err != nil {
		t.Fatalf("committing: %v", err)
	}

	var version int
	var policy string
	if err := p.QueryRow(ctx,
		`SELECT plan_version, policy FROM planning.collection_plans WHERE plan_id = $1`,
		planID).Scan(&version, &policy); err != nil {
		t.Fatalf("reading the plan: %v", err)
	}
	if version != 1 || policy != "GREEDY_BY_BID" {
		t.Errorf("plan version=%d policy=%q, want 1 / GREEDY_BY_BID", version, policy)
	}

	var acquisitions int
	if err := p.QueryRow(ctx,
		`SELECT count(*) FROM planning.acquisitions WHERE plan_id = $1 AND status = 'ACTIVE'`,
		planID).Scan(&acquisitions); err != nil {
		t.Fatalf("counting acquisitions: %v", err)
	}
	if acquisitions != 1 {
		t.Errorf("%d active acquisitions, want 1", acquisitions)
	}

	// The round event, the plan event and the unfulfilment event all queued in
	// the same transaction.
	var queued int
	if err := p.QueryRow(ctx, `
		SELECT count(*) FROM planning.outbox
		WHERE published_at IS NULL
		  AND event_type IN ('planning.round.triggered.v1','planning.plan.committed.v1','planning.request.unfulfilled.v1')
		  AND occurred_at > now() - interval '1 minute'
	`).Scan(&queued); err != nil {
		t.Fatalf("counting outbox rows: %v", err)
	}
	if queued < 3 {
		t.Errorf("%d outbox rows, want at least 3 — the plan is committed and somebody downstream never hears", queued)
	}
}

// Supersession retains the old acquisitions with a status rather than deleting
// them (ADR-0012). The SUPERSEDED reason code promises the customer an account
// of what replaced them, and deleting the evidence in the transaction that
// creates the need for it is not an explanation.
func TestSupersedingRetainsTheOldAcquisitions(t *testing.T) {
	p := pool(t)
	ctx := context.Background()

	bucketStart := domain.BucketStart(time.Now().UTC().Add(1000*time.Hour), bucketDuration)
	bucketEnd := bucketStart.Add(bucketDuration)
	satellite := fmt.Sprintf("SAT-PB%d", time.Now().UnixNano()%100000)
	customer, requestID := seedBucketWithRequest(t, p, satellite, bucketStart)
	key := domain.RoundKey{SatelliteID: satellite, BucketStart: bucketStart}

	firstPlan, err := commitOnce(t, p, key, bucketEnd, func(inputs port.RoundInputs) port.PlanCommit {
		return basePlan(inputs, customer,
			acquisition(requestID, uuid.NewString(), customer, bucketStart.Add(time.Minute), 30*time.Second))
	})
	if err != nil {
		t.Fatalf("first commit: %v", err)
	}

	secondPlan, err := commitOnce(t, p, key, bucketEnd, func(inputs port.RoundInputs) port.PlanCommit {
		if inputs.LivePlanID == nil || *inputs.LivePlanID != firstPlan {
			t.Errorf("the round did not see the live plan; got %v want %s", inputs.LivePlanID, firstPlan)
		}
		if inputs.NextPlanVersion != 2 {
			t.Errorf("next plan version = %d, want 2", inputs.NextPlanVersion)
		}
		// The SAME time window, which is the case that makes the exclusion
		// constraint fire against the plan being replaced unless it is partial
		// and deferred.
		return basePlan(inputs, customer,
			acquisition(requestID, uuid.NewString(), customer, bucketStart.Add(time.Minute), 30*time.Second))
	})
	if err != nil {
		t.Fatalf("superseding commit: %v — ADR-0012's partial deferred constraint is not doing its job", err)
	}

	var supersededCount, activeCount int
	if err := p.QueryRow(ctx,
		`SELECT count(*) FROM planning.acquisitions WHERE plan_id = $1 AND status = 'SUPERSEDED' AND superseded_at IS NOT NULL`,
		firstPlan).Scan(&supersededCount); err != nil {
		t.Fatalf("counting superseded: %v", err)
	}
	if supersededCount != 1 {
		t.Errorf("%d superseded acquisitions on the replaced plan, want 1 — the evidence the SUPERSEDED reason code promises is gone", supersededCount)
	}
	if err := p.QueryRow(ctx,
		`SELECT count(*) FROM planning.acquisitions WHERE plan_id = $1 AND status = 'ACTIVE'`,
		secondPlan).Scan(&activeCount); err != nil {
		t.Fatalf("counting active: %v", err)
	}
	if activeCount != 1 {
		t.Errorf("%d active acquisitions on the new plan, want 1", activeCount)
	}

	var supersedes *string
	if err := p.QueryRow(ctx,
		`SELECT supersedes_plan_id::text FROM planning.collection_plans WHERE plan_id = $1`,
		secondPlan).Scan(&supersedes); err != nil {
		t.Fatalf("reading supersedes: %v", err)
	}
	if supersedes == nil || *supersedes != firstPlan {
		t.Errorf("supersedes_plan_id = %v, want %s", supersedes, firstPlan)
	}
}

// Optimistic concurrency on the plan being replaced. If anybody touched it
// since this round read it, the round aborts rather than superseding a plan it
// never saw.
func TestAStalePlanVersionAbortsTheRound(t *testing.T) {
	p := pool(t)
	ctx := context.Background()

	bucketStart := domain.BucketStart(time.Now().UTC().Add(1100*time.Hour), bucketDuration)
	bucketEnd := bucketStart.Add(bucketDuration)
	satellite := fmt.Sprintf("SAT-PC%d", time.Now().UnixNano()%100000)
	customer, requestID := seedBucketWithRequest(t, p, satellite, bucketStart)
	key := domain.RoundKey{SatelliteID: satellite, BucketStart: bucketStart}

	firstPlan, err := commitOnce(t, p, key, bucketEnd, func(inputs port.RoundInputs) port.PlanCommit {
		return basePlan(inputs, customer,
			acquisition(requestID, uuid.NewString(), customer, bucketStart.Add(time.Minute), 30*time.Second))
	})
	if err != nil {
		t.Fatalf("first commit: %v", err)
	}

	_, err = commitOnce(t, p, key, bucketEnd, func(inputs port.RoundInputs) port.PlanCommit {
		plan := basePlan(inputs, customer,
			acquisition(requestID, uuid.NewString(), customer, bucketStart.Add(2*time.Minute), 30*time.Second))
		// Somebody else moved the plan between the read and the write.
		plan.SupersededRowVersion = inputs.LivePlanRowVersion + 7
		return plan
	})
	if err == nil {
		t.Fatal("a round superseding a plan at a row_version it never read was accepted")
	}
	if !errors.Is(err, port.ErrConcurrentPlan) {
		t.Errorf("error does not wrap ErrConcurrentPlan, so a caller cannot tell it from a real failure: %v", err)
	}

	// And it aborted cleanly: no version 2 exists.
	var versions int
	if err := p.QueryRow(ctx,
		`SELECT count(*) FROM planning.collection_plans WHERE satellite_id = $1 AND lower(bucket) = $2`,
		satellite, bucketStart).Scan(&versions); err != nil {
		t.Fatalf("counting plans: %v", err)
	}
	if versions != 1 {
		t.Errorf("%d plans exist, want only the first — the aborted round left something behind", versions)
	}
	_ = firstPlan
}

// A constraint violation aborts the WHOLE round. The exclusion constraint is a
// backstop, not the primary mechanism: if it fires the policy has a bug, and
// committing the rows that happened to be legal would leave a plan silently
// missing whatever the policy got wrong.
func TestAConstraintViolationAbortsTheWholeRound(t *testing.T) {
	p := pool(t)
	ctx := context.Background()

	bucketStart := domain.BucketStart(time.Now().UTC().Add(1200*time.Hour), bucketDuration)
	bucketEnd := bucketStart.Add(bucketDuration)
	satellite := fmt.Sprintf("SAT-PD%d", time.Now().UnixNano()%100000)
	customer, requestID := seedBucketWithRequest(t, p, satellite, bucketStart)
	key := domain.RoundKey{SatelliteID: satellite, BucketStart: bucketStart}

	roundEventID := uuid.NewString()
	rounds := postgres.NewRounds(p)

	_, err := rounds.OpenRound(ctx, key, bucketEnd, func(inputs port.RoundInputs) (port.RoundOutcome, error) {
		// TWO OVERLAPPING acquisitions on one satellite — a policy bug the
		// exclusion constraint exists to catch.
		start := bucketStart.Add(time.Minute)
		plan := basePlan(inputs, customer,
			acquisition(requestID, uuid.NewString(), customer, start, 5*time.Minute),
			acquisition(uuid.NewString(), uuid.NewString(), customer, start.Add(time.Minute), 5*time.Minute),
		)
		roundID := uuid.NewString()
		plan.RoundID = roundID
		return port.RoundOutcome{
			Round: port.Round{
				RoundID: roundID, EventID: roundEventID, CorrelationID: uuid.NewString(),
				Key: key, BucketEnd: bucketEnd,
				Trigger: domain.TriggerCadence, Policy: "GREEDY_BY_BID",
				CandidateOpportunityCount: inputs.CandidateOpportunityCount,
				CandidateRequestIDs:       inputs.CandidateRequestIDs,
				DutyCycleBudgetS:          inputs.DutyCycleBudgetS,
				TriggeredAt:               time.Now().UTC(),
			},
			RoundPayload: []byte(`{"event_id":"x"}`),
			Plan:         &plan,
		}, nil
	})

	if err == nil {
		t.Fatal("two overlapping acquisitions on one satellite were committed; the central invariant is not holding")
	}

	// Nothing survives — not the plan, not the legal acquisition, and above all
	// not the round event, which would have announced a decision boundary whose
	// plan does not exist.
	var plans, acquisitions, events int
	if err := p.QueryRow(ctx,
		`SELECT count(*) FROM planning.collection_plans WHERE satellite_id = $1 AND lower(bucket) = $2`,
		satellite, bucketStart).Scan(&plans); err != nil {
		t.Fatalf("counting plans: %v", err)
	}
	if err := p.QueryRow(ctx,
		`SELECT count(*) FROM planning.acquisitions WHERE request_id = $1`, requestID).Scan(&acquisitions); err != nil {
		t.Fatalf("counting acquisitions: %v", err)
	}
	if err := p.QueryRow(ctx,
		`SELECT count(*) FROM planning.outbox WHERE event_id = $1`, roundEventID).Scan(&events); err != nil {
		t.Fatalf("counting outbox rows: %v", err)
	}

	if plans != 0 || acquisitions != 0 || events != 0 {
		t.Errorf("the aborted round left %d plans, %d acquisitions and %d outbox rows behind",
			plans, acquisitions, events)
	}
}

// seedBucketWithRequest seeds a satellite, a customer, a request snapshot and
// one candidate, returning the customer and request ids the plan must reference.
func seedBucketWithRequest(t *testing.T, p *pgxpool.Pool, satellite string, bucketStart time.Time) (string, string) {
	t.Helper()
	seedSatellite(t, p, satellite)
	customer := fmt.Sprintf("cust-%d", time.Now().UnixNano())
	seedCustomer(t, p, customer)

	requestID := uuid.NewString()
	projections := postgres.NewProjections(p)
	if _, err := projections.ProjectSnapshot(context.Background(), port.ConsumerLifecycle,
		snapshotEvent(uuid.NewString(), requestID, customer)); err != nil {
		t.Fatalf("seeding the snapshot: %v", err)
	}

	event := candidateEvent(uuid.NewString(), requestID, satellite, uuid.NewString())
	event.Candidates[0].AccessStart = bucketStart.Add(time.Minute)
	event.Candidates[0].AccessEnd = bucketStart.Add(10 * time.Minute)
	if _, err := projections.ProjectCandidates(context.Background(), port.ConsumerOpportunities, event); err != nil {
		t.Fatalf("seeding candidates: %v", err)
	}
	return customer, requestID
}
