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

// Next must give every stream a turn, whatever the depth of the others.
//
// THE BUG THIS CATCHES WAS OBSERVED, NOT IMAGINED. Next returns as soon as one
// stream yields a batch; starting the scan at index 0 every time meant a
// stream with a backlog was fetched forever and the streams after it were
// never fetched at all. A load test left 358,045 messages on TASKING and this
// service then consumed none of the 782,870 waiting on PLANNING — while
// request_views kept advancing, so nothing looked broken.
//
// The rotation is tested rather than the fetching: the starvation is entirely
// in which index the scan begins at, and asserting that needs no broker.
//
// WHAT THIS DOES NOT COVER, stated rather than implied: it exercises
// nextStart, not Next's use of it. Watched failing against a nextStart pinned
// to 0 — "first pass started with [TASKING TASKING TASKING]" — but a change
// that stopped CALLING nextStart would pass. Covering that needs a live broker
// with two backlogged streams, which is source_integration_test.go's territory
// and a heavier test than this defect warrants on its own.
func TestTheStartingStreamRotates(t *testing.T) {
	source := &Source{order: []string{"TASKING", "FEASIBILITY", "PLANNING"}}

	seen := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		seen = append(seen, source.order[source.nextStart()])
	}

	// Every stream leads at least once in two passes. Under the old behaviour
	// this is ["TASKING", "TASKING", ...] and the last two never lead.
	if seen[0] != "TASKING" || seen[1] != "FEASIBILITY" || seen[2] != "PLANNING" {
		t.Errorf("first pass started with %v, want each stream in turn", seen[:3])
	}
	if seen[3] != "TASKING" {
		t.Errorf("second pass started with %q, want the rotation to wrap", seen[3])
	}
}

// A single stream must not divide by zero or spin.
func TestRotationHandlesOneStream(t *testing.T) {
	source := &Source{order: []string{"TASKING"}}
	for i := 0; i < 3; i++ {
		if got := source.nextStart(); got != 0 {
			t.Errorf("nextStart() = %d, want 0 for a single stream", got)
		}
	}
}
