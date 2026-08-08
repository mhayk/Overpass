package natsmsg

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/mhayk/overpass/services/planner/internal/port"
)

// eventAt is the part of this package with a decision in it, and the only part
// testable without a broker: nats.Msg.Metadata() works only on a message bound
// to a live subscription, so everything around it needs one.

func TestEventAtPrefersTheProducersClock(t *testing.T) {
	brokerAt := time.Date(2026, 8, 7, 9, 5, 0, 0, time.UTC)
	header := nats.Header{}
	header.Set("Occurred-At", "2026-08-07T09:00:00Z")

	got := eventAt(header, brokerAt)

	want := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("eventAt = %s, want the producer's %s", got, want)
	}
	// The broker's timestamp is when the message was STORED, which is later
	// than when the thing happened and drifts further the longer the outbox
	// backs up. Preferring it would make a busy system report itself as
	// fresher than it is — the wrong direction for a staleness number.
	if got.Equal(brokerAt) {
		t.Error("eventAt used the broker's store time while the producer's was available")
	}
}

func TestEventAtFallsBackToTheBroker(t *testing.T) {
	brokerAt := time.Date(2026, 8, 7, 9, 5, 0, 0, time.UTC)

	if got := eventAt(nats.Header{}, brokerAt); !got.Equal(brokerAt) {
		t.Errorf("with no Occurred-At, eventAt = %s, want the broker's %s", got, brokerAt)
	}
}

// A malformed header must NOT propagate as a zero time. Zero would report the
// projection as decades behind, which is far worse than a few seconds of skew.
func TestEventAtSurvivesAMalformedHeader(t *testing.T) {
	brokerAt := time.Date(2026, 8, 7, 9, 5, 0, 0, time.UTC)
	header := nats.Header{}
	header.Set("Occurred-At", "yesterday afternoon")

	got := eventAt(header, brokerAt)
	if got.IsZero() {
		t.Fatal("a malformed timestamp produced the zero time; staleness would read as decades")
	}
	if !got.Equal(brokerAt) {
		t.Errorf("eventAt = %s, want the broker's %s", got, brokerAt)
	}
}

// The handle key must separate streams. Both consumers see sequence numbers
// starting at 1, so keying on sequence alone would let a TASKING ack release a
// FEASIBILITY message's handle — acking one event by acknowledging another.
func TestHandleKeySeparatesStreams(t *testing.T) {
	tasking := port.Message{Stream: "TASKING", Sequence: 1}
	feasibility := port.Message{Stream: "FEASIBILITY", Sequence: 1}

	if handleKey(tasking) == handleKey(feasibility) {
		t.Error("two streams at sequence 1 share a handle key; an ack on one would release the other")
	}
}

func TestClaimRefusesAnUnknownHandle(t *testing.T) {
	s := &Source{inflight: map[string]*nats.Msg{}}

	if _, err := s.claim(port.Message{Stream: "TASKING", Sequence: 7, EventID: "e1"}); err == nil {
		t.Fatal("claimed a handle that was never registered; a double ack would pass silently")
	}
}
