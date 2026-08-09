package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The broker, restarted while a consumer is working through a backlog.
//
// Two things are claimed and neither is obvious. A durable pull consumer must
// SURVIVE the broker going away — the client reconnects, the subscription is
// still bound, and fetching resumes without anyone restarting a service. And no
// message may be lost: file storage plus explicit acks means everything
// unacked at the moment of the restart comes back.
//
// The failure this rules out is the quiet one: a consumer that reconnects to
// the socket but never fetches again. The service is up, the broker is up, the
// connection is established, and the backlog simply stops moving.

func TestABrokerRestartUnderLoadLosesNothing(t *testing.T) {
	satelliteID := seedChaosSatellite(t)
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	const deliveries = 120
	all := make([]chaosDelivery, 0, deliveries)
	for range deliveries {
		f := newFixture(t)
		f.satelliteID = satelliteID

		subject, payload := f.opportunitiesEvent(t, at, 4)
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

	planner, err := start(env.plannerBin, "planner", plannerEnv())
	if err != nil {
		t.Fatalf("starting the planner: %v", err)
	}
	t.Cleanup(func() {
		if killErr := planner.Kill(); killErr != nil {
			t.Errorf("killing the planner: %v", killErr)
		}
	})

	// Restart once it is demonstrably working. Restarting an idle broker would
	// prove reconnection and nothing about what happens to work in flight.
	if !waitFor(60*time.Second, func() bool { return completed(t, all) > 0 }) {
		t.Fatal("the planner never started consuming; there is no load to restart under")
	}
	before := completed(t, all)
	if before == deliveries {
		t.Skip("the backlog drained before the restart could land; nothing to test")
	}

	restartBroker(t)
	t.Logf("broker restarted with %d of %d deliveries done", before, deliveries)

	// The consumer comes back on its own. No restart of the planner, no
	// intervention — the whole point of a durable consumer plus a reconnecting
	// client.
	eventually(t, "the backlog to finish after the broker restart", 180*time.Second, func() bool {
		return completed(t, all) == deliveries
	})

	// Nothing lost and nothing half-applied: redeliveries after a restart are
	// ordinary, and the ledger absorbs them.
	requireNoHalfDeliveries(t, "after the broker restart", all)

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

// restartBroker stops and starts the NATS container, keeping its published port
// so everything reconnects to the address it already knows.
//
// Stop-and-start rather than terminate-and-recreate: the streams and their
// messages live on the container's filesystem, and a fresh container would be
// an empty broker — which would make "no message loss" trivially false for
// reasons that have nothing to do with the system under test.
func restartBroker(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	timeout := 10 * time.Second
	if err := env.natsContainer.Stop(ctx, &timeout); err != nil {
		t.Fatalf("stopping the broker: %v", err)
	}
	if err := env.natsContainer.Start(ctx); err != nil {
		t.Fatalf("starting the broker: %v", err)
	}

	// Wait for the broker to answer again before handing control back, so a
	// failure downstream is about the consumer rather than about a server that
	// had not finished booting.
	if !waitFor(60*time.Second, func() bool {
		_, err := env.js.StreamInfo("FEASIBILITY")
		return err == nil
	}) {
		t.Fatal("the broker never became usable again after the restart")
	}
}
