package app_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mhayk/overpass/services/plan-gateway/internal/app"
)

// failureEnvelope builds a feasibility.failed.v1 with the retryable flag set
// either way, which is the only thing these tests vary.
func failureEnvelope(t *testing.T, retryable bool, at time.Time) string {
	t.Helper()
	when := stamp(at)
	// The same envelope shape envelopeFor builds — schema_version is a semantic
	// version string, not a number, and the decoder rejects unknown fields.
	body := map[string]any{
		"event_id":       sampleEventID,
		"event_type":     app.SubjectFeasibilityFailed,
		"schema_version": "1.0.0",
		"occurred_at":    when,
		"correlation_id": sampleCorrID,
		"causation_id":   nil,
		"producer":       "feasibility-service",
		"data": map[string]any{
			"request_id":  sampleRequestID,
			"reason_code": "NO_ACCESS_IN_HORIZON",
			"retryable":   retryable,
			"failed_at":   when,
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// A DEFINITIVE REFUSAL IS RECORDED. The defect this fixes, stated as a test.
//
// feasibility.failed.v1 was published, delivered, and consumed by a projector
// that folded five subjects and did not include it — so it fell through to the
// default branch, which ignores unknown subjects and advances the cursor. The
// request stayed at RECEIVED with no error anywhere. See #207.
func TestANonRetryableFailureIsProjected(t *testing.T) {
	projection := newRecording()
	p := newProjector(&countingSource{}, projection)

	payload := failureEnvelope(t, false, eventAt)
	if err := p.Apply(t.Context(), msg("FEASIBILITY", app.SubjectFeasibilityFailed, 1, payload)); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if len(projection.folded) != 1 || projection.folded[0] != "feasibility-failed" {
		t.Fatalf("folded %v; a request the system refused is not being recorded", projection.folded)
	}
}

// A RETRYABLE FAILURE IS NOT AN ANSWER, and must never be recorded as one.
//
// It says something about our ability to compute — a TLE we could not fetch, a
// propagation that errored — not about the physics. The message goes back to
// the stream with backoff and will be answered properly later. Recording it
// would publish our transient problem to the customer as their permanent
// verdict, and INFEASIBLE is terminal: there is no event that walks it back.
func TestARetryableFailureIsNotRecordedAsAVerdict(t *testing.T) {
	projection := newRecording()
	p := newProjector(&countingSource{}, projection)

	payload := failureEnvelope(t, true, eventAt)
	if err := p.Apply(t.Context(), msg("FEASIBILITY", app.SubjectFeasibilityFailed, 1, payload)); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if len(projection.folded) != 0 {
		t.Errorf("folded %v; a retryable failure was recorded as a terminal verdict", projection.folded)
	}
}

// THE CURSOR STILL ADVANCES PAST A RETRYABLE FAILURE.
//
// Skipping the projection must not skip the fold. Leaving the cursor behind on
// a message this projector has genuinely finished with would stall the stream
// and re-deliver it forever — trading a wrong row for a stopped pipeline.
func TestARetryableFailureStillAdvancesTheCursor(t *testing.T) {
	projection := newRecording()
	p := newProjector(&countingSource{}, projection)

	payload := failureEnvelope(t, true, eventAt)
	if err := p.Apply(t.Context(), msg("FEASIBILITY", app.SubjectFeasibilityFailed, 42, payload)); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if len(projection.advance) != 1 || projection.advance[0].Sequence != 42 {
		t.Fatalf("advance = %+v, want sequence 42 — the stream would redeliver forever", projection.advance)
	}
}
