package natsmsg

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
)

// Internal tests on purpose. Fetch and convert both need a live broker —
// nats.Msg.Metadata() only works on a message bound to a subscription — but the
// two things most likely to be quietly wrong are pure: which clock EventAt
// comes from, and whether an ack handle can be used twice.

var brokerAt = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func TestEventAtPrefersTheProducersClock(t *testing.T) {
	occurred := time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC)
	header := nats.Header{"Occurred-At": []string{occurred.Format(time.RFC3339Nano)}}

	if got := eventAt(header, brokerAt); !got.Equal(occurred) {
		t.Fatalf("EventAt = %s, want the producer's %s — the broker's store time "+
			"makes a backed-up outbox look fresh", got, occurred)
	}
}

func TestEventAtFallsBackToTheBroker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header nats.Header
	}{
		{"no header", nats.Header{}},
		{"empty header", nats.Header{"Occurred-At": []string{""}}},
		{"unparseable", nats.Header{"Occurred-At": []string{"last tuesday"}}},
		{"wrong format", nats.Header{"Occurred-At": []string{"01/03/2026"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := eventAt(tc.header, brokerAt)
			if got.IsZero() {
				t.Fatal("fell through to a zero time; staleness would report decades")
			}
			if !got.Equal(brokerAt) {
				t.Fatalf("EventAt = %s, want the broker's %s", got, brokerAt)
			}
		})
	}
}

// TestAnAckHandleCannotBeClaimedTwice makes a double ack loud.
//
// The broker ignores the second one, so without this the bug it points at — a
// message handled twice in the same batch — would leave no trace at all.
func TestAnAckHandleCannotBeClaimedTwice(t *testing.T) {
	s := &Source{inflight: map[string]*nats.Msg{}}
	m := port.Message{Stream: "TASKING", Sequence: 7, EventID: "evt-1"}
	s.inflight[handleKey(m)] = &nats.Msg{Subject: "x"}

	if _, err := s.claim(m); err != nil {
		t.Fatalf("first claim failed: %v", err)
	}
	if _, err := s.claim(m); err == nil {
		t.Fatal("the same handle was claimed twice without complaint")
	}
}

func TestClaimingAnUnknownHandleIsAnError(t *testing.T) {
	s := &Source{inflight: map[string]*nats.Msg{}}
	if _, err := s.claim(port.Message{Stream: "TASKING", Sequence: 1}); err == nil {
		t.Fatal("acked a message the source never handed out")
	}
}

// TestHandleKeysAreScopedToTheirStream guards a collision that is guaranteed to
// happen: TASKING 5 and PLANNING 5 are different messages, because sequences
// are per stream.
func TestHandleKeysAreScopedToTheirStream(t *testing.T) {
	a := handleKey(port.Message{Stream: "TASKING", Sequence: 5})
	b := handleKey(port.Message{Stream: "PLANNING", Sequence: 5})
	if a == b {
		t.Fatalf("both streams key to %q; one message's ack would release the other's", a)
	}
}

// TestStreamsMatchTheDeclaredTopology keeps this list from drifting away from
// deploy/nats/init.sh, which is where the streams and consumers are actually
// created. A stream named here that does not exist there fails at Bind with a
// consumer-not-found nobody expects.
func TestStreamsMatchTheDeclaredTopology(t *testing.T) {
	want := map[string]bool{"TASKING": true, "FEASIBILITY": true, "PLANNING": true}
	if len(Streams) != len(want) {
		t.Fatalf("Streams = %v, want exactly %v", Streams, want)
	}
	for _, s := range Streams {
		if !want[s] {
			t.Errorf("%s is not a stream deploy/nats/init.sh creates", s)
		}
	}
	// Tasking before planning: the causal order, so a cold rebuild rarely folds
	// a plan before the request it belongs to.
	if Streams[0] != "TASKING" {
		t.Errorf("Streams starts with %s; draining tasking first is deliberate", Streams[0])
	}
}
