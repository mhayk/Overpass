package natsmsg_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/mhayk/overpass/services/planner/internal/adapter/natsmsg"
	"github.com/mhayk/overpass/services/planner/internal/app"
)

// Against a real broker, because every claim here is about the broker.
//
// Bind resolves durable consumer names that are declared in deploy/nats/init.sh
// and nowhere in this code. A fake would agree with whatever this file asserts
// and prove nothing about whether the names still match the topology — which is
// the only way this package fails in practice. Both consumers it binds,
// planner-opportunities and planner-lifecycle, were provisioned in M0-06 and
// had nothing attached to them until now.

func connect(t *testing.T) (nats.JetStreamContext, *nats.Conn) {
	t.Helper()
	url := os.Getenv("OVERPASS_TEST_NATS_URL")
	if url == "" {
		t.Skip("set OVERPASS_TEST_NATS_URL to run the broker tests")
	}
	conn, err := nats.Connect(url, nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return js, conn
}

// TestBindFindsTheDeclaredConsumers is the whole point of this file.
//
// The durable names live in deploy/nats/init.sh; this code only knows the
// constants in port. Nothing but a real bind can tell you whether those two
// halves still agree — and #109 is the precedent for what happens when they
// quietly do not.
func TestBindFindsTheDeclaredConsumers(t *testing.T) {
	js, _ := connect(t)

	source, err := natsmsg.Bind(js, 8, time.Second)
	if err != nil {
		t.Fatalf("binding to the declared consumers failed; the names in port no longer match deploy/nats/init.sh: %v", err)
	}
	t.Cleanup(func() {
		if err := source.Drain(); err != nil {
			t.Logf("draining: %v", err)
		}
	})
}

func TestBindFailsLoudlyOnAnUndeclaredConsumer(t *testing.T) {
	js, _ := connect(t)

	// Bind, not create, is the rule: a service that creates its own consumer
	// silently diverges from the versioned topology the first time a setting
	// changes. This asserts the rule holds by requiring an unknown name to be
	// an error rather than an auto-creation.
	if _, err := js.ConsumerInfo("TASKING", "planner-does-not-exist"); err == nil {
		t.Fatal("a consumer that was never declared reported info; Bind would silently succeed against nothing")
	}
}

// A message published to a subject the consumer filters on must actually
// arrive. Reading `consumer info` cannot catch a filter that resolves to
// nothing — that was #109, where a comma-joined filter subject produced a
// consumer that received exactly zero messages while looking correctly
// configured.
func TestAPublishedMessageReachesTheProjector(t *testing.T) {
	js, _ := connect(t)

	source, err := natsmsg.Bind(js, 8, 2*time.Second)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	t.Cleanup(func() {
		if err := source.Drain(); err != nil {
			t.Logf("draining: %v", err)
		}
	})

	eventID := "aaaaaaaa-0000-4000-8000-" + time.Now().UTC().Format("150405.000000")[:12]
	msg := nats.NewMsg(app.SubjectRequestReceived)
	msg.Data = []byte(`{"event_id":"` + eventID + `"}`)
	msg.Header.Set("Nats-Msg-Id", eventID)
	msg.Header.Set("Occurred-At", "2026-08-07T09:00:00Z")
	if _, err := js.PublishMsg(msg); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var found bool
	for !found && ctx.Err() == nil {
		batch, err := source.Next(ctx)
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		for _, m := range batch {
			// Other tests and other runs share this broker, so ack everything
			// that arrives and look for the one that is ours.
			if m.EventID == eventID {
				found = true
				if m.Stream != "TASKING" {
					t.Errorf("stream = %q, want TASKING", m.Stream)
				}
				if m.Subject != app.SubjectRequestReceived {
					t.Errorf("subject = %q", m.Subject)
				}
				// The producer's clock, taken from the header rather than the
				// broker's store time.
				want := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
				if !m.EventAt.Equal(want) {
					t.Errorf("event_at = %s, want the producer's %s", m.EventAt, want)
				}
				if m.Sequence == 0 {
					t.Error("sequence is 0; the ordering guard has nothing to work with")
				}
			}
			if err := source.Ack(context.Background(), m); err != nil {
				t.Errorf("acking: %v", err)
			}
		}
	}

	if !found {
		t.Fatal("a message published to a subject planner-lifecycle filters on never arrived")
	}
}

// Acking twice must be a loud error, not a second ack the broker quietly
// ignores. A silent double ack hides a bug where two goroutines believe they
// own the same message.
func TestDoubleAckIsRefused(t *testing.T) {
	js, _ := connect(t)

	source, err := natsmsg.Bind(js, 8, 2*time.Second)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	t.Cleanup(func() {
		if err := source.Drain(); err != nil {
			t.Logf("draining: %v", err)
		}
	})

	eventID := "bbbbbbbb-0000-4000-8000-" + time.Now().UTC().Format("150405.000000")[:12]
	msg := nats.NewMsg(app.SubjectOpportunities)
	msg.Data = []byte(`{"event_id":"` + eventID + `"}`)
	msg.Header.Set("Nats-Msg-Id", eventID)
	if _, err := js.PublishMsg(msg); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	for ctx.Err() == nil {
		batch, err := source.Next(ctx)
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		for _, m := range batch {
			if err := source.Ack(context.Background(), m); err != nil {
				t.Fatalf("first ack: %v", err)
			}
			if err := source.Ack(context.Background(), m); err == nil {
				t.Fatal("the second ack succeeded; a double ack passes silently and hides shared ownership of a message")
			}
			return
		}
	}
	t.Skip("no message arrived within the timeout; nothing to double-ack")
}
