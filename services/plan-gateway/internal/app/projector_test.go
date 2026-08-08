package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/plan-gateway/internal/adapter/wire"
	"github.com/mhayk/overpass/services/plan-gateway/internal/app"
	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
)

var eventAt = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// Real contract envelopes, not hand-shaped fragments.
//
// Every payload below is one the schemas would accept. That is not fussiness:
// the tests these replace passed `{"request_id":"r1"}` and `{}`, which decode to
// nothing under the real contracts, and the suite could not tell the difference.

const (
	sampleRequestID = "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"
	sampleEventID   = "11111111-2222-4333-8444-555555555555"
	sampleCorrID    = "66666666-7777-4888-8999-aaaaaaaaaaaa"
	samplePlanID    = "33333333-4444-4555-8666-777777777777"
	sampleRoundID   = "44444444-5555-4666-8777-888888888888"
	sampleOppID     = "55555555-6666-4777-8888-999999999999"
	sampleAcqID     = "66666666-7777-4888-8999-000000000000"
)

func stamp(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }

func envelopeFor(t *testing.T, subject string, at time.Time) string {
	t.Helper()
	when := stamp(at)
	base := map[string]any{
		"event_id":       sampleEventID,
		"event_type":     subject,
		"schema_version": "1.0.0",
		"occurred_at":    when,
		"correlation_id": sampleCorrID,
		"causation_id":   nil,
	}

	switch subject {
	case app.SubjectRequestReceived:
		base["producer"] = "tasking-api"
		base["data"] = map[string]any{
			"request_id":      sampleRequestID,
			"customer_id":     "acme-imaging",
			"target":          map[string]any{"type": "Point", "coordinates": []float64{0, 0}},
			"window":          map[string]any{"start": when, "end": stamp(at.Add(24 * time.Hour))},
			"priority_tier":   "BEST_EFFORT",
			"bid_credits":     0,
			"requested_modes": []string{"SCAN"},
			"submitted_at":    when,
		}
	case app.SubjectOpportunitiesComputed:
		base["producer"] = "feasibility-service"
		base["data"] = map[string]any{
			"request_id":           sampleRequestID,
			"computed_at":          when,
			"horizon":              map[string]any{"start": when, "end": stamp(at.Add(24 * time.Hour))},
			"tle_references":       []any{},
			"satellites_evaluated": 1,
			"opportunity_count":    1,
			"truncated":            false,
			"compute_duration_ms":  12,
			"opportunities": []any{map[string]any{
				"opportunity_id":         sampleOppID,
				"satellite_id":           "CAPELLA-14",
				"mode":                   "STRIPMAP",
				"access_window":          map[string]any{"start": when, "end": stamp(at.Add(10 * time.Minute))},
				"acquisition_duration_s": 8,
				"geometry":               accessGeometry(),
				"footprint":              polygon(),
				"duty_cycle_cost_s":      8,
				"quality_score":          0.9,
			}},
		}
	case app.SubjectPlanCommitted:
		base["producer"] = "planner-service"
		base["data"] = map[string]any{
			"plan_id":      samplePlanID,
			"round_id":     sampleRoundID,
			"satellite_id": "CAPELLA-14",
			"bucket":       map[string]any{"start": when, "end": stamp(at.Add(3 * time.Hour))},
			"plan_version": 1,
			"policy":       "GreedyByBid",
			"committed_at": when,
			"acquisitions": []any{map[string]any{
				"acquisition_id":        sampleAcqID,
				"request_id":            sampleRequestID,
				"opportunity_id":        sampleOppID,
				"mode":                  "STRIPMAP",
				"window":                map[string]any{"start": when, "end": stamp(at.Add(8 * time.Second))},
				"geometry":              accessGeometry(),
				"awarded_value_credits": 500,
			}},
			"metrics": map[string]any{
				"total_plan_value_credits":    500,
				"requests_fulfilled":          1,
				"requests_unfulfilled":        0,
				"candidate_opportunity_count": 1,
				"satellite_utilisation_ratio": 0.1,
				"duty_cycle_used_s":           8,
				"duty_cycle_budget_s":         600,
				"total_slew_time_s":           0,
				"allocation_duration_ms":      5,
			},
		}
	case app.SubjectRequestUnfulfilled:
		base["producer"] = "planner-service"
		base["data"] = map[string]any{
			"request_id":         sampleRequestID,
			"round_id":           sampleRoundID,
			"reason_code":        "OUTBID",
			"decided_at":         when,
			"eligible_for_retry": true,
		}
	default:
		t.Fatalf("no envelope builder for %s", subject)
	}

	out, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("building %s: %v", subject, err)
	}
	return string(out)
}

func accessGeometry() map[string]any {
	return map[string]any{
		"incidence_angle_deg": 30.0,
		"look_side":           "RIGHT",
		"squint_angle_deg":    0.0,
		"elevation_angle_deg": 60.0,
		"slant_range_km":      700.0,
	}
}

func polygon() map[string]any {
	return map[string]any{
		"type": "Polygon",
		"coordinates": [][][]float64{{
			{4, 51}, {4, 52}, {5, 52}, {5, 51}, {4, 51},
		}},
	}
}

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

// newProjector uses the REAL decoder, not a stub.
//
// A stub decoder here would accept whatever these tests hand it and recreate
// exactly the blind spot that let #112 through: every input built from the
// same shape it decodes into.
func newProjector(source port.MessageSource, projection port.Projection) *app.Projector {
	return app.NewProjector(source, projection, wire.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestEachSubjectReachesItsFold catches a routing table that compiles and is
// wrong. A misrouted subject produces no error at all — it just silently never
// projects, which surfaces weeks later as an empty page.
func TestEachSubjectReachesItsFold(t *testing.T) {
	for _, tc := range []struct {
		subject string
		want    string
	}{
		{app.SubjectRequestReceived, "request"},
		{app.SubjectOpportunitiesComputed, "opportunities"},
		{app.SubjectPlanCommitted, "plan"},
		{app.SubjectRequestUnfulfilled, "unfulfilled"},
	} {
		t.Run(tc.subject, func(t *testing.T) {
			projection := newRecording()
			p := newProjector(&countingSource{}, projection)
			payload := envelopeFor(t, tc.subject, eventAt)
			if err := p.Apply(t.Context(), msg("S", tc.subject, 1, payload)); err != nil {
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
	m := msg("TASKING", app.SubjectRequestReceived, 7, envelopeFor(t, app.SubjectRequestReceived, eventAt))

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

	if err := p.Apply(t.Context(), msg("TASKING", app.SubjectRequestReceived, 5, envelopeFor(t, app.SubjectRequestReceived, eventAt))); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(t.Context(), msg("PLANNING", app.SubjectPlanCommitted, 5, envelopeFor(t, app.SubjectPlanCommitted, eventAt))); err != nil {
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

	err := p.Apply(t.Context(), msg("PLANNING", app.SubjectPlanCommitted, 3, envelopeFor(t, app.SubjectPlanCommitted, eventAt)))
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

	if err := p.Apply(t.Context(), msg("TASKING", "tasking.something.new.v1", 1, `{"anything":true}`)); err != nil {
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

// TestAPayloadThatDoesNotMatchTheContractIsRefused is the #112 regression at
// the projector level.
//
// The original version of this test passed `not json` and was satisfied. That
// left the far more dangerous case wide open: VALID json whose field names the
// contract does not know decodes to a zero struct, returns no error, and folds
// an empty row. Both shapes are covered here now, and the second one is the
// one that actually shipped.
func TestAPayloadThatDoesNotMatchTheContractIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, payload string }{
		{"not json at all", `not json`},
		{"valid json, wrong shape", `{"request_id":"r1","customer_id":"acme"}`},
		{"the envelope with no data", `{"event_id":"x"}`},
		{"an empty object", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projection := newRecording()
			p := newProjector(&countingSource{}, projection)

			err := p.Apply(t.Context(), msg("TASKING", app.SubjectRequestReceived, 1, tc.payload))
			if err == nil {
				t.Fatal("accepted a payload the contract would reject")
			}
			if !errors.Is(err, wire.ErrMalformed) {
				t.Fatalf("want ErrMalformed, got %v", err)
			}
			if len(projection.folded) != 0 {
				t.Fatalf("it was folded anyway: %v", projection.folded)
			}
			if len(projection.advance) != 0 {
				t.Fatal("the cursor advanced past a payload that never folded")
			}
		})
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
			msg("TASKING", app.SubjectRequestReceived, 1, envelopeFor(t, app.SubjectRequestReceived, eventAt)),
			msg("TASKING", app.SubjectRequestReceived, 2, envelopeFor(t, app.SubjectRequestReceived, eventAt)),
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
		batches: [][]port.Message{{msg("PLANNING", app.SubjectPlanCommitted, 9, envelopeFor(t, app.SubjectPlanCommitted, eventAt))}},
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
		batches: [][]port.Message{{msg("TASKING", app.SubjectRequestReceived, 1, envelopeFor(t, app.SubjectRequestReceived, eventAt))}},
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

// TestEventAtComesFromTheEnvelopesOccurredAt pins which clock staleness is
// measured against.
//
// The event's own occurred_at, stamped by the producing outbox — not the
// broker's store time, which is later and drifts further the longer the outbox
// backs up. Here the two are deliberately an hour apart.
func TestEventAtComesFromTheEnvelopesOccurredAt(t *testing.T) {
	projection := newRecording()
	p := newProjector(&countingSource{}, projection)

	occurred := eventAt.Add(-time.Hour)
	m := msg("TASKING", app.SubjectRequestReceived, 1,
		envelopeFor(t, app.SubjectRequestReceived, occurred))
	m.EventAt = eventAt // what the broker would report

	if err := p.Apply(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if got := projection.advance[0].LastEventAt; !got.Equal(occurred) {
		t.Fatalf("cursor stamped %s, want the envelope's occurred_at %s", got, occurred)
	}
}
