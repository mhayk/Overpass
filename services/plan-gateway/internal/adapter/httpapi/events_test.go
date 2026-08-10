package httpapi_test

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/plan-gateway/internal/adapter/httpapi"
	"github.com/mhayk/overpass/services/plan-gateway/internal/app"
)

// streamServer takes the read timeout explicitly, because one test's whole
// point is that the stream OUTLIVES it — and proving that with the 30s default
// would mean a 30-second test.
func streamServer(t *testing.T, hub *app.Hub, readTimeout time.Duration) http.Handler {
	t.Helper()
	return httpapi.New(&fakeReads{}, func() error { return nil },
		func() time.Time { return pinnedNow }, readTimeout,
		slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithHub(hub).
		Routes()
}

// readFrames collects SSE frames until the deadline, then returns what arrived.
func readFrames(t *testing.T, body io.Reader, want int, within time.Duration) []string {
	t.Helper()
	frames := make(chan string, 64)
	go func() {
		scanner := bufio.NewScanner(body)
		var current strings.Builder
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if current.Len() > 0 {
					frames <- current.String()
					current.Reset()
				}
				continue
			}
			current.WriteString(line)
			current.WriteString("\n")
		}
	}()

	var got []string
	deadline := time.After(within)
	for len(got) < want {
		select {
		case frame := <-frames:
			got = append(got, frame)
		case <-deadline:
			return got
		}
	}
	return got
}

// THE STREAM MUST OUTLIVE THE READ DEADLINE.
//
// Deadline (#51) exists so a stalled query cannot hold a request open forever.
// A stream is supposed to be held open, so the same middleware would close it
// after READ_TIMEOUT and keep doing so forever — a feature that looks broken
// rather than slow. This asserts the exemption.
func TestTheStreamIsNotClosedByTheReadDeadline(t *testing.T) {
	// A DELIBERATELY TINY read timeout. If the stream inherited it, the
	// connection would be closed 200ms in — and the publish below, which
	// happens well after that, would never arrive.
	const readTimeout = 200 * time.Millisecond

	hub := app.NewHub()
	server := httptest.NewServer(streamServer(t, hub, readTimeout))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/events", http.NoBody)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("content-type = %q, want text/event-stream", got)
	}
	// A cached event stream is not a stream.
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("cache-control = %q, want no-store", got)
	}

	// Publish well AFTER the read timeout would have fired, and require
	// delivery anyway.
	time.Sleep(5 * readTimeout)
	hub.Publish(app.Change{Kind: "request", RequestID: "r1", State: "PLANNED"})

	frames := readFrames(t, resp.Body, 1, 3*time.Second)
	if len(frames) == 0 {
		t.Fatal("no frame arrived; the deadline middleware is closing the stream")
	}
	if !strings.Contains(frames[0], "event: request") {
		t.Errorf("frame = %q, want a request event", frames[0])
	}
}

// An unresumable Last-Event-ID gets a `reset`, not silence.
//
// Silence is indistinguishable from "nothing has happened", and a UI that
// believes that goes quietly stale — the failure this mechanism exists to
// avoid.
func TestAnUnresumableIdGetsAResetFrame(t *testing.T) {
	hub := app.NewHub()
	for i := 0; i < 1000; i++ {
		hub.Publish(app.Change{Kind: "request", RequestID: "r1"})
	}

	server := httptest.NewServer(streamServer(t, hub, testReadTimeout))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/events", http.NoBody)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Last-Event-ID", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test

	frames := readFrames(t, resp.Body, 1, 3*time.Second)
	if len(frames) == 0 || !strings.Contains(frames[0], "event: reset") {
		t.Fatalf("frames = %v, want a reset", frames)
	}
}

// Events carry an id, because Last-Event-ID is meaningless without one.
func TestEventsCarryAnId(t *testing.T) {
	hub := app.NewHub()
	server := httptest.NewServer(streamServer(t, hub, testReadTimeout))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/events", http.NoBody)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test

	hub.Publish(app.Change{Kind: "plan", PlanID: "p1", SatelliteID: "SENTINEL-1A"})

	frames := readFrames(t, resp.Body, 1, 3*time.Second)
	if len(frames) == 0 {
		t.Fatal("no frame arrived")
	}
	if !strings.Contains(frames[0], "id: ") {
		t.Errorf("frame carries no id, so a client cannot resume:\n%s", frames[0])
	}
}

// A backwards window is refused, like every other endpoint's.
func TestAStreamRefusesABackwardsWindow(t *testing.T) {
	hub := app.NewHub()
	recorder := httptest.NewRecorder()
	streamServer(t, hub, testReadTimeout).ServeHTTP(recorder, httptest.NewRequestWithContext(
		context.Background(), http.MethodGet,
		"/v1/events?window_start=2026-03-02T00:00:00Z&window_end=2026-03-01T00:00:00Z",
		http.NoBody))

	if recorder.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", recorder.Code)
	}
}

// Without a hub there is no route at all, rather than a route that hangs.
func TestNoHubMeansNoStreamRoute(t *testing.T) {
	server := httpapi.New(&fakeReads{}, func() error { return nil },
		func() time.Time { return pinnedNow }, testReadTimeout,
		slog.New(slog.NewTextHandler(io.Discard, nil))).Routes()

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequestWithContext(
		context.Background(), http.MethodGet, "/v1/events", http.NoBody))

	if recorder.Code == http.StatusOK {
		t.Error("served a stream with no hub behind it")
	}
}
