package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/mhayk/overpass/services/plan-gateway/internal/app"
)

// eventFlushInterval coalesces a burst before it reaches the wire.
//
// A round of planning commits many changes at once, and writing each one
// immediately turns one logical event into dozens of frames a browser must
// parse and a React tree must reconcile. Batching over a short window makes a
// burst cost one render instead of thirty, which is the difference between a
// plan appearing and a page stuttering while it appears.
//
// 100ms because it is below the threshold where a UI feels delayed and well
// above the cost of a write.
const eventFlushInterval = 100 * time.Millisecond

// eventPingInterval keeps intermediaries from closing an idle stream.
//
// Proxies and load balancers commonly drop a connection that has been silent
// for a minute. A comment frame costs nothing and turns "the demo broke after
// lunch" into a non-event.
const eventPingInterval = 25 * time.Second

// streamEvents serves the SSE stream.
//
// The deadline middleware is bypassed for this route — see Routes. A stream is
// supposed to stay open, and a five-second read timeout would close it five
// seconds in, forever, which reads as a broken feature rather than a timeout.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// No flusher means every write is buffered until the handler returns,
		// which for a stream is never. Refusing is better than hanging.
		s.problem(w, r, problemDoc{
			Type:   ProblemBase + "streaming-unsupported",
			Title:  "Streaming unsupported",
			Status: http.StatusInternalServerError,
			Detail: "This server cannot flush; the event stream would never be delivered.",
		})
		return
	}

	filter, ok := s.eventFilter(w, r)
	if !ok {
		return
	}

	since := lastEventID(r)
	sub, backlog, resumed := s.hub.Subscribe(filter, since)
	defer s.hub.Unsubscribe(sub)

	w.Header().Set("Content-Type", "text/event-stream")
	// A cached event stream is not a stream.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	// Nginx and friends buffer by default, which holds every frame until the
	// buffer fills — a stream that arrives in lumps minutes late.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	if !resumed {
		// The client asked to resume from an id we can no longer vouch for.
		// Telling it so is the whole point: silence is indistinguishable from
		// "nothing has happened", and a UI that believes that goes quietly
		// stale.
		writeEvent(w, 0, "reset", map[string]string{
			"detail": "resume window exceeded; refetch before trusting this stream",
		})
		flusher.Flush()
	}

	for _, change := range backlog {
		writeChange(w, change)
	}
	flusher.Flush()

	ping := time.NewTicker(eventPingInterval)
	defer ping.Stop()
	flush := time.NewTicker(eventFlushInterval)
	defer flush.Stop()

	pending := make([]app.Change, 0, 32)
	writePending := func() {
		if len(pending) == 0 {
			return
		}
		for _, change := range pending {
			writeChange(w, change)
		}
		pending = pending[:0]
		flusher.Flush()
	}

	for {
		select {
		case <-r.Context().Done():
			return

		case change, open := <-sub.C:
			if !open {
				// The hub dropped us for falling behind. Say so rather than
				// closing silently: the client should reconnect, and a bare
				// EOF looks like a network blip it might not retry from.
				if sub.Dropped() {
					writeEvent(w, 0, "reset", map[string]string{
						"detail": "client fell behind and was disconnected; reconnect to resume",
					})
					flusher.Flush()
				}
				return
			}
			pending = append(pending, change)
			// A full batch goes immediately rather than waiting for the tick.
			// Holding it would trade the memory saving for latency on exactly
			// the burst the batching exists to smooth.
			if len(pending) >= cap(pending) {
				writePending()
			}

		case <-flush.C:
			writePending()

		case <-ping.C:
			writePending()
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// eventFilter parses the subscription's narrowing, refusing what it cannot
// honour rather than silently widening it.
func (s *Server) eventFilter(w http.ResponseWriter, r *http.Request) (app.Filter, bool) {
	filter := app.Filter{
		SatelliteID: r.URL.Query().Get("satellite_id"),
		RequestID:   r.URL.Query().Get("request_id"),
	}

	if start, err := timeParam(r, "window_start"); err != nil {
		s.problem(w, r, badRequest("window_start must be RFC 3339"))
		return app.Filter{}, false
	} else if start != nil {
		filter.WindowStart = *start
	}

	if end, err := timeParam(r, "window_end"); err != nil {
		s.problem(w, r, badRequest("window_end must be RFC 3339"))
		return app.Filter{}, false
	} else if end != nil {
		filter.WindowEnd = *end
	}

	if !filter.WindowStart.IsZero() && !filter.WindowEnd.IsZero() &&
		!filter.WindowEnd.After(filter.WindowStart) {
		s.problem(w, r, badRequest("window_end must be after window_start"))
		return app.Filter{}, false
	}

	return filter, true
}

// lastEventID reads the resumption point.
//
// The header is what a browser sends automatically on reconnect. The query
// parameter exists for clients that are not browsers — curl, a test — and is
// the same value by another route.
func lastEventID(r *http.Request) uint64 {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("last_event_id")
	}
	// An unparseable id is treated as "no id" rather than an error. It costs
	// the client a full refetch, which is what it would get from a reset
	// anyway, and refusing the connection over a malformed header would make a
	// stale proxy value permanently fatal.
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

func writeChange(w http.ResponseWriter, change app.Change) {
	switch change.Kind {
	case "request":
		writeEvent(w, change.ID, "request", map[string]any{
			"request_id": change.RequestID,
			"state":      change.State,
		})
	case "plan":
		writeEvent(w, change.ID, "plan", map[string]any{
			"plan_id":      change.PlanID,
			"satellite_id": change.SatelliteID,
			"bucket_start": change.BucketStart.UTC().Format(time.RFC3339),
			"plan_version": change.PlanVersion,
		})
	}
}

// writeEvent frames one SSE event.
//
// Errors are ignored deliberately: a failed write means the client is gone,
// and the loop discovers that through the request context on the next
// iteration. Handling it here would mean two places deciding the connection is
// over.
func writeEvent(w http.ResponseWriter, id uint64, event string, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if id > 0 {
		fmt.Fprintf(w, "id: %d\n", id) //nolint:errcheck // see above
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body) //nolint:errcheck // see above
}
