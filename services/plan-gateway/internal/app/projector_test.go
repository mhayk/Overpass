package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/plan-gateway/internal/app"
	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
)

var eventAt = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// recordingProjection notes what it was asked to fold and can be told to fail.
type recordingProjection struct {
	folded  []string
	cursors map[string]port.Cursor
	advance []port.Cursor
	failOn  string
}

func newRecording() *recordingProjection {
	return &recordingProjection{cursors: map[string]port.Cursor{}}
}

func (p *recordingProjection) note(kind string) error {
	if p.failOn == kind {
		return errors.New("fold blew up")
	}
	p.folded = append(p.folded, kind)
	return nil
}

func (p *recordingProjection) ProjectRequestReceived(_ context.Context, _ port.RequestReceived) error {
	return p.note("request")
}

func (p *recordingProjection) ProjectOpportunities(_ context.Context, _ port.OpportunitiesComputed) error {
	return p.note("opportunities")
}

func (p *recordingProjection) ProjectPlanCommitted(_ context.Context, _ port.PlanCommitted) error {
	return p.note("plan")
}

func (p *recordingProjection) ProjectUnfulfilled(_ context.Context, _ port.RequestUnfulfilled) error {
	return p.note("unfulfilled")
}

func (p *recordingProjection) Cursor(_ context.Context, stream string) (port.Cursor, error) {
	return p.cursors[stream], nil
}

func (p *recordingProjection) Advance(_ context.Context, stream string, seq uint64, at time.Time) error {
	p.cursors[stream] = port.Cursor{Sequence: seq, LastEventAt: at}
	p.advance = append(p.advance, port.Cursor{Sequence: seq, LastEventAt: at})
	return nil
}

func (p *recordingProjection) Reset(context.Context) error { return nil }

// countingSource records acks and naks and hands out scripted batches.
type countingSource struct {
	batches [][]port.Message
	fetchAt int
	failNth int // 1-based; the nth Next call returns an error

	acked []uint64
	naked []uint64

	cancel func()
}

func (s *countingSource) Next(context.Context) ([]port.Message, error) {
	s.fetchAt++
	if s.fetchAt == s.failNth {
		return nil, errors.New("broker reconnecting")
	}
	i := s.fetchAt - 1
	if s.failNth > 0 && s.fetchAt > s.failNth {
		i-- // the failed call consumed no batch
	}
	if i >= len(s.batches) {
		// Out of script. Stop the loop rather than spin.
		if s.cancel != nil {
			s.cancel()
		}
		return nil, nil
	}
	return s.batches[i], nil
}

func (s *countingSource) Ack(_ context.Context, m port.Message) error {
	s.acked = append(s.acked, m.Sequence)
	return nil
}

func (s *countingSource) Nak(_ context.Context, m port.Message) error {
	s.naked = append(s.naked, m.Sequence)
	return nil
}

func msg(stream, subject string, seq uint64, payload string) port.Message {
	return port.Message{
		Stream:   stream,
		Sequence: seq,
		Subject:  subject,
		EventID:  "evt-" + subject,
		EventAt:  eventAt,
		Payload:  []byte(payload),
	}
}

func newProjector(source port.MessageSource, projection port.Projection) *app.Projector {
	return app.NewProjector(source, projection, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestEachSubjectReachesItsFold catches a routing table that compiles and is
// wrong. A misrouted subject produces no error at all — it just silently never
// projects, which surfaces weeks later as an empty page.
func TestEachSubjectReachesItsFold(t *testing.T) {
	for _, tc := range []struct {
		subject string
		payload string
		want    string
	}{
		{app.SubjectRequestReceived, `{"request_id":"r1"}`, "request"},
		{app.SubjectOpportunitiesComputed, `{"request_id":"r1"}`, "opportunities"},
		{app.SubjectPlanCommitted, `{"plan_id":"p1"}`, "plan"},
		{app.SubjectRequestUnfulfilled, `{"request_id":"r1"}`, "unfulfilled"},
	} {
		t.Run(tc.subject, func(t *testing.T) {
			projection := newRecording()
			p := newProjector(&countingSource{}, projection)
			if err := p.Apply(t.Context(), msg("S", tc.subject, 1, tc.payload)); err != nil {
				t.Fatalf("apply: %v", err)
			}
			if len(projection.folded) != 1 || projection.folded[0] != tc.want {
				t.Fatalf("folded %v, want [%s]", projection.folded, tc.want)
			}
		})
	}
}

// TestAReplayedSequenceIsIgnored is the idempotency guard.
//
// Redelivery is normal, not exceptional: an ack that does not reach the broker
// before ack_wait expires produces one. Folding twice would double an
// opportunity_count.
func TestAReplayedSequenceIsIgnored(t *testing.T) {
	projection := newRecording()
	p := newProjector(&countingSource{}, projection)
	m := msg("TASKING", app.SubjectRequestReceived, 7, `{"request_id":"r1"}`)

	for i := range 3 {
		if err := p.Apply(t.Context(), m); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	if len(projection.folded) != 1 {
		t.Fatalf("folded %d times, want 1 — redelivery is being folded again", len(projection.folded))
	}
}

// TestTheGuardIsPerStream stops a shared cursor swallowing real events.
//
// Sequences are per stream and they collide constantly: TASKING 5 and PLANNING
// 5 are unrelated messages. A single global cursor would drop whichever arrived
// second, and nothing would report it.
func TestTheGuardIsPerStream(t *testing.T) {
	projection := newRecording()
	p := newProjector(&countingSource{}, projection)

	if err := p.Apply(t.Context(), msg("TASKING", app.SubjectRequestReceived, 5, `{}`)); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(t.Context(), msg("PLANNING", app.SubjectPlanCommitted, 5, `{}`)); err != nil {
		t.Fatal(err)
	}
	if len(projection.folded) != 2 {
		t.Fatalf("folded %v, want both — the cursor is not per stream", projection.folded)
	}
}

// TestAFailedFoldDoesNotAdvanceTheCursor is the one that matters most.
//
// Advancing past an event that did not fold loses it permanently: the guard
// above will ignore the redelivery, and the read model is silently missing a
// row with no error anywhere to explain it.
func TestAFailedFoldDoesNotAdvanceTheCursor(t *testing.T) {
	projection := newRecording()
	projection.failOn = "plan"
	p := newProjector(&countingSource{}, projection)

	err := p.Apply(t.Context(), msg("PLANNING", app.SubjectPlanCommitted, 3, `{}`))
	if err == nil {
		t.Fatal("a failing fold reported success")
	}
	if len(projection.advance) != 0 {
		t.Fatalf("cursor advanced past a fold that failed: %+v", projection.advance)
	}
}

// TestAnUnknownSubjectIsSkippedNotFailed keeps one new event type from wedging
// every stream it shares.
func TestAnUnknownSubjectIsSkippedNotFailed(t *testing.T) {
	projection := newRecording()
	p := newProjector(&countingSource{}, projection)

	if err := p.Apply(t.Context(), msg("TASKING", "tasking.something.new.v1", 1, `{}`)); err != nil {
		t.Fatalf("an unrecognised subject failed the fold: %v", err)
	}
	if len(projection.folded) != 0 {
		t.Fatalf("an unrecognised subject was folded as something: %v", projection.folded)
	}
	// Still advances. Leaving the cursor behind on a subject this service will
	// never handle would stall it forever.
	if len(projection.advance) != 1 {
		t.Fatal("the cursor did not advance past a subject that will never be handled")
	}
}

// TestAMalformedPayloadFailsRatherThanFoldingAnEmptyEvent guards against the
// worst outcome: json.Unmarshal on garbage leaving a zero-valued struct that
// folds cleanly and writes an empty row.
func TestAMalformedPayloadFailsRatherThanFoldingAnEmptyEvent(t *testing.T) {
	projection := newRecording()
	p := newProjector(&countingSource{}, projection)

	err := p.Apply(t.Context(), msg("TASKING", app.SubjectRequestReceived, 1, `not json`))
	if !errors.Is(err, app.ErrMalformed) {
		t.Fatalf("want ErrMalformed, got %v", err)
	}
	if len(projection.folded) != 0 {
		t.Fatalf("a malformed payload was folded: %v", projection.folded)
	}
}

// TestAnEventIsAckedOnlyAfterItIsFolded is the effectively-once guarantee.
//
// Acking on receipt would make a crash mid-fold lose the event permanently:
// the broker considers it delivered, the read model never got it, and nothing
// anywhere reports a problem.
func TestAnEventIsAckedOnlyAfterItIsFolded(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	source := &countingSource{
		batches: [][]port.Message{{
			msg("TASKING", app.SubjectRequestReceived, 1, `{}`),
			msg("TASKING", app.SubjectRequestReceived, 2, `{}`),
		}},
		cancel: cancel,
	}
	projection := newRecording()

	if err := newProjector(source, projection).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(projection.folded) != 2 {
		t.Fatalf("folded %v, want both", projection.folded)
	}
	if len(source.acked) != 2 {
		t.Fatalf("acked %v, want both after folding", source.acked)
	}
	if len(source.naked) != 0 {
		t.Fatalf("naked %v on a clean run", source.naked)
	}
}

// TestAFailedFoldIsNakedAndNotAcked keeps the event on the broker so it comes
// back. The alternative is a silent hole in the read model.
func TestAFailedFoldIsNakedAndNotAcked(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	source := &countingSource{
		batches: [][]port.Message{{msg("PLANNING", app.SubjectPlanCommitted, 9, `{}`)}},
		cancel:  cancel,
	}
	projection := newRecording()
	projection.failOn = "plan"

	if err := newProjector(source, projection).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(source.acked) != 0 {
		t.Fatalf("acked %v despite the fold failing", source.acked)
	}
	if len(source.naked) != 1 || source.naked[0] != 9 {
		t.Fatalf("naked %v, want [9] so the broker redelivers", source.naked)
	}
}

// TestAFetchFailureDoesNotStopTheLoop matters because the common cause is the
// broker reconnecting. Exiting would turn a blip into a projection that
// silently stops advancing and never resumes.
func TestAFetchFailureDoesNotStopTheLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	source := &countingSource{
		failNth: 1,
		batches: [][]port.Message{{msg("TASKING", app.SubjectRequestReceived, 1, `{}`)}},
		cancel:  cancel,
	}
	projection := newRecording()

	if err := newProjector(source, projection).Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(projection.folded) != 1 {
		t.Fatalf("folded %v — the loop gave up after one fetch error", projection.folded)
	}
}

// TestRunReturnsOnCancellation is the shutdown path. A loop that ignores its
// context makes every deploy wait for a SIGKILL.
func TestRunReturnsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	done := make(chan error, 1)
	go func() { done <- newProjector(&countingSource{}, newRecording()).Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancellation reported as a failure: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// TestEventAtComesFromTheEnvelope pins which clock staleness is measured
// against. A payload field disagreeing with the envelope would make lag a
// function of two different clocks.
func TestEventAtComesFromTheEnvelope(t *testing.T) {
	projection := newRecording()
	p := newProjector(&countingSource{}, projection)

	// The payload claims an hour earlier than the envelope.
	payload := `{"request_id":"r1","event_at":"2026-03-01T11:00:00Z"}`
	if err := p.Apply(t.Context(), msg("TASKING", app.SubjectRequestReceived, 1, payload)); err != nil {
		t.Fatal(err)
	}
	if got := projection.advance[0].LastEventAt; !got.Equal(eventAt) {
		t.Fatalf("cursor stamped %s, want the envelope's %s", got, eventAt)
	}
}
