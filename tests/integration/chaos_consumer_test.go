package integration_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// A consumer killed INSIDE its transaction.
//
// TestAProjectorKilledMidStreamLosesNothing already kills a projector between
// messages and proves nothing is lost. This is the other half, and it is a
// different claim: that no delivery can leave HALF of itself behind.
//
// The whole idempotency design rests on one sentence — the dedup claim and the
// state change commit together or not at all. If they could come apart, the two
// halves fail in opposite directions and both are silent: a claim without a
// projection is an event permanently marked done that never happened, and a
// projection without a claim is an event that will be applied again. Neither
// raises anything. Only killing the process at the wrong moment, repeatedly,
// and then checking both tables against each other, can tell you which world
// you are in.

// chaosDelivery is one published event, identified the way the system
// identifies it.
//
// The ENVELOPE's event_id, not the broker's Nats-Msg-Id — checked against the
// code rather than assumed, because the first version of this test assumed the
// header and reported a false defect. contracts/nats/topology.md states the
// rule: "the contract puts event_id in the payload, and that is the identity
// the dedup keys on". The header is the BROKER's dedup key, a different
// mechanism at a different layer, and the outbox relay sets it to the same
// value — which is why this test publishes them equal.
type chaosDelivery struct {
	eventID string
}

// withEnvelopeID rewrites the envelope's event_id so a published event can be
// found in the projection afterwards. The examples carry a fixed id, and a
// burst of events sharing one would be indistinguishable exactly where this
// test needs to tell them apart.
func withEnvelopeID(t *testing.T, payload []byte, eventID string) []byte {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("decoding the envelope: %v", err)
	}
	envelope["event_id"] = eventID
	return encode(t, envelope)
}

// seedChaosSatellite inserts one satellite the planner's candidate rows can
// reference, and returns its id.
func seedChaosSatellite(t *testing.T) string {
	t.Helper()
	satelliteID := "CHAOS-" + uuidUpper()

	// norad_id from the table, not from a hash of the name: it carries its own
	// UNIQUE constraint, which ON CONFLICT (satellite_id) does not absorb.
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO reference.satellites
			(satellite_id, norad_id, display_name, sensor_modes, duty_cycle_budget_s)
		VALUES ($1, (SELECT COALESCE(MAX(norad_id), 900000) + 1 FROM reference.satellites),
		        $1, '{}'::jsonb, 600000)
		ON CONFLICT (satellite_id) DO NOTHING`, satelliteID); err != nil {
		t.Fatalf("seeding the satellite: %v", err)
	}
	return satelliteID
}

// claimedAndProjected reports the two halves for one delivery.
func claimedAndProjected(t *testing.T, d chaosDelivery) (claimed, projected bool) {
	t.Helper()
	return rowCount(t, `SELECT count(*) FROM planning.processed_events
	                     WHERE consumer = 'planner-opportunities' AND event_id = $1`, d.eventID) > 0,
		rowCount(t, `SELECT count(*) FROM planning.candidate_opportunities
		              WHERE source_event_id = $1`, d.eventID) > 0
}

// completed counts claimed deliveries in ONE query.
//
// Separate from the pairwise check because it runs in a polling loop: two
// queries per delivery per poll would put the test's own load between the
// consumer and the kill it is trying to aim.
func completed(t *testing.T, deliveries []chaosDelivery) int {
	t.Helper()
	ids := make([]string, 0, len(deliveries))
	for _, d := range deliveries {
		ids = append(ids, d.eventID)
	}
	return rowCount(t, `SELECT count(*) FROM planning.processed_events
	                     WHERE consumer = 'planner-opportunities' AND event_id = ANY($1::uuid[])`, ids)
}

// requireNoHalfDeliveries is the assertion this whole file exists for.
func requireNoHalfDeliveries(t *testing.T, when string, deliveries []chaosDelivery) int {
	t.Helper()
	done := 0
	for i, d := range deliveries {
		claimed, projected := claimedAndProjected(t, d)
		switch {
		case claimed && !projected:
			t.Fatalf("%s: delivery %d is claimed in the ledger with nothing projected — "+
				"the event is permanently marked done and never happened", when, i)
		case projected && !claimed:
			t.Fatalf("%s: delivery %d is projected with no ledger claim — "+
				"it will be applied again on redelivery, and nothing will notice", when, i)
		case claimed:
			done++
		}
	}
	return done
}

func TestAConsumerKilledMidTransactionLeavesNoPartialState(t *testing.T) {
	satelliteID := seedChaosSatellite(t)
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	// A backlog, published before anything is consuming it. The planner then
	// meets all of it at once, which is what keeps a transaction open when the
	// kill lands — a trickle would be idle most of the time and the kills would
	// hit nothing.
	// Big enough that the consumer is still working when the kills land. The
	// first version used 24 and drained them all before the first kill — the
	// test passed, having exercised a restart, and said so in its log. That is
	// the number this one is chosen against.
	const deliveries = 200
	all := make([]chaosDelivery, 0, deliveries)
	for range deliveries {
		f := newFixture(t)
		f.satelliteID = satelliteID

		subject, payload := f.opportunitiesEvent(t, at, 5)
		eventID := uuid.NewString()
		publishAs(t, subject, withEnvelopeID(t, payload, eventID), at, eventID)
		all = append(all, chaosDelivery{eventID: eventID})
	}
	t.Cleanup(func() {
		for _, d := range all {
			if _, err := env.pool.Exec(context.Background(),
				`DELETE FROM planning.candidate_opportunities WHERE source_event_id = $1`,
				d.eventID); err != nil {
				t.Logf("cleanup: %v", err)
			}
		}
	})

	// Three kills, because one is an anecdote. Each lands somewhere different
	// in the backlog, and the invariant is checked in the window each one
	// opens — while nothing is running, which is the only moment a half-applied
	// delivery would still be visible rather than repaired by a retry.
	for kill := 1; kill <= 3; kill++ {
		planner, err := start(env.plannerBin, "planner", plannerEnv())
		if err != nil {
			t.Fatalf("starting the planner for kill %d: %v", kill, err)
		}

		// Aimed at a moving consumer: wait until it has finished a share of the
		// backlog, then kill immediately. Killing at the same point every time
		// would probe one instant of the fold over and over.
		target := kill * deliveries / 6
		progressed := waitFor(60*time.Second, func() bool { return completed(t, all) >= target })
		inFlight := completed(t, all) < deliveries
		if killErr := planner.Kill(); killErr != nil {
			t.Fatalf("killing the planner: %v", killErr)
		}
		if !progressed || !inFlight {
			t.Logf("kill %d: the backlog was already drained; this kill landed on an "+
				"idle consumer rather than a working one", kill)
		}

		done := requireNoHalfDeliveries(t, "after a kill", all)
		t.Logf("kill %d: %d of %d deliveries complete, none half-applied", kill, done, deliveries)
		if done == deliveries {
			break // nothing left to kill inside
		}
	}

	// Nothing was lost either: a killed consumer redelivers, and the backlog
	// finishes. Without this the test would pass on a projector that simply
	// stopped after the first kill.
	last, err := start(env.plannerBin, "planner", plannerEnv())
	if err != nil {
		t.Fatalf("restarting the planner: %v", err)
	}
	t.Cleanup(func() {
		if killErr := last.Kill(); killErr != nil {
			t.Errorf("killing the last planner: %v", killErr)
		}
	})

	eventually(t, "the backlog to drain", 180*time.Second, func() bool {
		return completed(t, all) == deliveries
	})
	requireNoHalfDeliveries(t, "at rest", all)

	// Leave the consumer drained, not merely "all claimed".
	//
	// A claim happens at COMMIT and the ack follows it, so a killed consumer
	// leaves messages that redeliver for up to ack_wait afterwards. The next
	// test in the package then starts a planner that spends its first minute
	// chewing through this test's leftovers instead of doing what it was
	// started for — which is exactly how the planner chaos tests failed in the
	// full suite while passing alone. Tidying up here is cheaper than making
	// every later test patient.
	eventually(t, "the consumer to settle everything it fetched", 120*time.Second,
		drained("FEASIBILITY", "planner-opportunities"))
}

// publishAs publishes with the Nats-Msg-Id the outbox relay would set: the
// envelope's own event_id. The shared `publish` helper invents a fresh one,
// which is right for the tests that need the broker's dedup and the ledger's to
// be told apart, and wrong here.
func publishAs(t *testing.T, subject string, payload []byte, occurredAt time.Time, eventID string) {
	t.Helper()
	msg := &nats.Msg{
		Subject: subject,
		Data:    payload,
		Header: nats.Header{
			"Nats-Msg-Id": []string{eventID},
			"Occurred-At": []string{occurredAt.UTC().Format(time.RFC3339Nano)},
		},
	}
	if _, err := env.js.PublishMsg(msg); err != nil {
		t.Fatalf("publishing %s: %v", subject, err)
	}
}

// uuidUpper is a satellite-id-safe random suffix: reference.satellites enforces
// ^[A-Z0-9][A-Z0-9_-]{0,31}$, the same rule the contract's pattern states.
func uuidUpper() string {
	out := make([]byte, 0, 8)
	for _, c := range uuid.NewString()[:8] {
		if c >= 'a' && c <= 'f' {
			c -= 32
		}
		out = append(out, byte(c))
	}
	return string(out)
}
