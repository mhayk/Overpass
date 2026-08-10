package app

import (
	"sync"
	"time"
)

// Change is what the stream carries: WHAT changed, never the thing itself.
//
// Thin on purpose. A client refetches the resource it already knows how to
// read; a stream carrying full entities would be a second API to keep in step
// with the first, and the two would disagree the first time one gained a
// field.
type Change struct {
	// ID is monotonic within this process and is what a client sends back as
	// Last-Event-ID.
	ID uint64
	// Kind is "request" or "plan".
	Kind string
	// The identifiers a subscriber filters on. Empty means not applicable —
	// a request change has no satellite.
	RequestID   string
	SatelliteID string
	PlanID      string
	BucketStart time.Time
	State       string
	PlanVersion int
}

// Filter narrows a subscription. A zero Filter matches everything, which is
// what a client that asked for nothing should get.
type Filter struct {
	SatelliteID string
	RequestID   string
	WindowStart time.Time
	WindowEnd   time.Time
}

func (f Filter) matches(change Change) bool {
	if f.RequestID != "" && change.RequestID != f.RequestID {
		return false
	}
	if f.SatelliteID != "" && change.SatelliteID != "" && change.SatelliteID != f.SatelliteID {
		return false
	}
	// The window applies to plans, which have a bucket. A request change has
	// no bucket and is NOT filtered out by a window — a client watching a time
	// range still wants to know its request was refused, and dropping that
	// would make the filter hide the one event the user is waiting for.
	if !change.BucketStart.IsZero() {
		if !f.WindowStart.IsZero() && change.BucketStart.Before(f.WindowStart) {
			return false
		}
		if !f.WindowEnd.IsZero() && !change.BucketStart.Before(f.WindowEnd) {
			return false
		}
	}
	return true
}

// Subscriber is one connected client.
type Subscriber struct {
	C      chan Change
	filter Filter
	// dropped is set when the hub gave up on this client, so the handler can
	// tell a disconnect apart from a shutdown.
	dropped bool
}

// Dropped reports whether the hub disconnected this subscriber for being slow.
func (s *Subscriber) Dropped() bool { return s.dropped }

// Hub fans changes out to connected clients and remembers the recent past.
//
// In-process and in-memory, deliberately. The alternative — a durable per-
// client cursor — is a second delivery system beside JetStream, and the read
// model it is announcing is itself rebuildable. Best-effort resumption over a
// bounded window is the honest shape for a live view, and the `reset` event is
// what keeps "I lost your place" from being indistinguishable from "nothing
// happened".
type Hub struct {
	mu          sync.Mutex
	nextID      uint64
	subscribers map[*Subscriber]struct{}

	// recent is a ring of the last bufferSize changes, for Last-Event-ID.
	recent []Change
}

// bufferSize bounds both the replay window and the memory one client can cost.
//
// 256 is roughly a minute of a busy demo. Large enough that a reconnect after
// a dropped Wi-Fi packet resumes cleanly; small enough that the memory is a
// rounding error, which is the trade a bounded buffer exists to make.
const bufferSize = 256

// subscriberQueue is how far behind a client may fall before it is dropped.
//
// Small on purpose. A big queue does not fix a slow client, it only delays the
// decision while holding more memory — and by the time it fills, the client is
// receiving minutes-old news it will refetch anyway.
const subscriberQueue = 32

func NewHub() *Hub {
	return &Hub{subscribers: map[*Subscriber]struct{}{}}
}

// Subscribe registers a client and returns everything after `since`.
//
// A `since` the hub can no longer serve returns resumed=false, and the caller
// is expected to tell the client to refetch. Returning nothing and pretending
// it resumed would leave a UI silently stale — the failure this whole
// mechanism exists to avoid.
func (h *Hub) Subscribe(filter Filter, since uint64) (*Subscriber, []Change, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	sub := &Subscriber{C: make(chan Change, subscriberQueue), filter: filter}
	h.subscribers[sub] = struct{}{}

	if since == 0 {
		// A fresh connection, not a resumption. Nothing to replay and nothing
		// lost, so this is not a reset.
		return sub, nil, true
	}

	var backlog []Change
	resumed := false
	for _, change := range h.recent {
		if change.ID == since {
			resumed = true
			continue
		}
		if change.ID > since {
			if resumed && filter.matches(change) {
				backlog = append(backlog, change)
			}
		}
	}
	// The id was not in the ring: either it is older than the buffer or this
	// process restarted and the ids mean nothing. Both are "I cannot prove you
	// have not missed something".
	if !resumed {
		return sub, nil, false
	}
	return sub, backlog, true
}

func (h *Hub) Unsubscribe(sub *Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subscribers[sub]; ok {
		delete(h.subscribers, sub)
		close(sub.C)
	}
}

// Publish fans a change out, dropping clients that cannot keep up.
//
// The send is non-blocking. A blocking send would let one slow reader stall
// the projector itself — the fold, the ack, and every other subscriber — which
// turns a client's problem into the server's.
func (h *Hub) Publish(change Change) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.nextID++
	change.ID = h.nextID

	h.recent = append(h.recent, change)
	if len(h.recent) > bufferSize {
		h.recent = h.recent[len(h.recent)-bufferSize:]
	}

	for sub := range h.subscribers {
		if !sub.filter.matches(change) {
			continue
		}
		select {
		case sub.C <- change:
		default:
			// Full. Drop rather than buffer: unbounded per-client buffering
			// turns one bad connection into a server-wide memory problem, and
			// a dropped client reconnects and resumes.
			sub.dropped = true
			delete(h.subscribers, sub)
			close(sub.C)
		}
	}
}

// Subscribers reports how many clients are connected, for the metric.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subscribers)
}
