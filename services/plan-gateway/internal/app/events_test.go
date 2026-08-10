package app_test

import (
	"testing"
	"time"

	"github.com/mhayk/overpass/services/plan-gateway/internal/app"
)

func drain(sub *app.Subscriber) []app.Change {
	var got []app.Change
	for {
		select {
		case change, open := <-sub.C:
			if !open {
				return got
			}
			got = append(got, change)
		default:
			return got
		}
	}
}

// A slow client is DROPPED, not buffered.
//
// Unbounded per-client buffering turns one bad connection into a server-wide
// memory problem. A dropped client reconnects and resumes; a server that has
// run out of memory does not.
func TestASlowSubscriberIsDroppedRatherThanBuffered(t *testing.T) {
	hub := app.NewHub()
	sub, _, _ := hub.Subscribe(app.Filter{}, 0)

	// Never read from sub.C. Publish far past the queue depth.
	for i := 0; i < 5000; i++ {
		hub.Publish(app.Change{Kind: "request", RequestID: "r1", State: "PLANNED"})
	}

	if !sub.Dropped() {
		t.Fatal("subscriber was not dropped; a slow reader would grow unboundedly")
	}
	if hub.Subscribers() != 0 {
		t.Errorf("hub still holds %d subscribers", hub.Subscribers())
	}
}

// Publishing must not block on a slow client.
//
// A blocking send would let one slow reader stall the projector itself — the
// fold, the ack, and every other subscriber — turning a client's problem into
// the server's.
func TestPublishDoesNotBlockOnASlowSubscriber(t *testing.T) {
	hub := app.NewHub()
	hub.Subscribe(app.Filter{}, 0) //nolint:errcheck // never read from, deliberately

	done := make(chan struct{})
	go func() {
		for i := 0; i < 10_000; i++ {
			hub.Publish(app.Change{Kind: "request", RequestID: "r1"})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked; one slow client can stall the projector")
	}
}

// A resumable id replays what came after it.
func TestResumptionReplaysWhatCameAfterTheId(t *testing.T) {
	hub := app.NewHub()
	for i := 0; i < 5; i++ {
		hub.Publish(app.Change{Kind: "request", RequestID: "r1"})
	}

	_, backlog, resumed := hub.Subscribe(app.Filter{}, 2)
	if !resumed {
		t.Fatal("id 2 is inside the buffer and should have resumed")
	}
	if len(backlog) != 3 {
		t.Fatalf("backlog = %d, want 3 (ids 3,4,5)", len(backlog))
	}
	if backlog[0].ID != 3 {
		t.Errorf("backlog starts at %d, want 3", backlog[0].ID)
	}
}

// AN UNRESUMABLE ID MUST SAY SO, so the handler can send `reset`.
//
// Returning an empty backlog and claiming success would leave a UI silently
// stale, and stale-but-quiet is indistinguishable from up-to-date. That is the
// failure this whole mechanism exists to avoid.
func TestAnIdOlderThanTheBufferReportsAFailedResume(t *testing.T) {
	hub := app.NewHub()
	for i := 0; i < 1000; i++ {
		hub.Publish(app.Change{Kind: "request", RequestID: "r1"})
	}

	_, backlog, resumed := hub.Subscribe(app.Filter{}, 1)
	if resumed {
		t.Error("claimed to resume from an id long since evicted")
	}
	if backlog != nil {
		t.Error("returned a backlog it cannot vouch for")
	}
}

// A fresh connection is not a failed resume.
func TestAFreshConnectionIsNotAReset(t *testing.T) {
	hub := app.NewHub()
	hub.Publish(app.Change{Kind: "request", RequestID: "r1"})

	_, backlog, resumed := hub.Subscribe(app.Filter{}, 0)
	if !resumed {
		t.Error("a client with no Last-Event-ID has missed nothing and must not be reset")
	}
	if len(backlog) != 0 {
		t.Error("replayed history to a client that never asked for it")
	}
}

func TestFiltersNarrowBySatellite(t *testing.T) {
	hub := app.NewHub()
	sub, _, _ := hub.Subscribe(app.Filter{SatelliteID: "SENTINEL-1A"}, 0)

	hub.Publish(app.Change{Kind: "plan", SatelliteID: "SENTINEL-1A", PlanID: "p1"})
	hub.Publish(app.Change{Kind: "plan", SatelliteID: "ICEYE-X2", PlanID: "p2"})

	got := drain(sub)
	if len(got) != 1 || got[0].PlanID != "p1" {
		t.Errorf("got %+v, want only the SENTINEL-1A plan", got)
	}
}

// A WINDOW FILTER MUST NOT HIDE REQUEST EVENTS.
//
// A request change has no bucket. A client watching a time range still wants
// to know its request was refused, and filtering it out by a window it does
// not have would hide the one event the user is waiting for.
func TestAWindowDoesNotSuppressRequestChanges(t *testing.T) {
	hub := app.NewHub()
	sub, _, _ := hub.Subscribe(app.Filter{
		WindowStart: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
	}, 0)

	hub.Publish(app.Change{Kind: "request", RequestID: "r1", State: "AWAITING_PLANNING"})

	if len(drain(sub)) != 1 {
		t.Error("a window filter suppressed a request change, which carries no bucket")
	}
}

func TestAWindowFiltersPlansByBucket(t *testing.T) {
	hub := app.NewHub()
	sub, _, _ := hub.Subscribe(app.Filter{
		WindowStart: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
	}, 0)

	hub.Publish(app.Change{
		Kind: "plan", PlanID: "inside",
		BucketStart: time.Date(2026, 8, 10, 6, 0, 0, 0, time.UTC),
	})
	hub.Publish(app.Change{
		Kind: "plan", PlanID: "outside",
		BucketStart: time.Date(2026, 8, 12, 6, 0, 0, 0, time.UTC),
	})

	got := drain(sub)
	if len(got) != 1 || got[0].PlanID != "inside" {
		t.Errorf("got %+v, want only the in-window plan", got)
	}
}

// Ids are monotonic, because Last-Event-ID is only meaningful if they are.
func TestIdsAreMonotonic(t *testing.T) {
	hub := app.NewHub()
	sub, _, _ := hub.Subscribe(app.Filter{}, 0)

	for i := 0; i < 10; i++ {
		hub.Publish(app.Change{Kind: "request", RequestID: "r1"})
	}

	var previous uint64
	for _, change := range drain(sub) {
		if change.ID <= previous {
			t.Fatalf("id %d followed %d; resumption needs a total order", change.ID, previous)
		}
		previous = change.ID
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	hub := app.NewHub()
	sub, _, _ := hub.Subscribe(app.Filter{}, 0)
	hub.Unsubscribe(sub)

	hub.Publish(app.Change{Kind: "request", RequestID: "r1"})
	if hub.Subscribers() != 0 {
		t.Error("unsubscribe left the client registered")
	}
}
