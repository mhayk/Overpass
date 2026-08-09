package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mhayk/overpass/lib/go/consume"

	"github.com/mhayk/overpass/services/planner/internal/app"
	"github.com/mhayk/overpass/services/planner/internal/domain"
	"github.com/mhayk/overpass/services/planner/internal/port"
)

// fakeSource hands over one batch, then nothing, and records every ack and nak.
//
// Recording BOTH is the point. Most of these tests are really about which of
// the two happened: acking a failure drops an event silently, and a silently
// dropped opportunity is a customer whose request never competes in any round.
type fakeSource struct {
	batches    [][]port.Message
	acked      []string
	naked      []string
	terminated []string

	// deadlettered records what was handed to the DLQ, and settled records the
	// ORDER of every settle call — because #49 is a statement about order.
	// "Dead-lettered and terminated" is true of both the correct sequence and
	// the one that drops the payload first, and only one of those is safe.
	deadlettered []deadLetter
	settled      []string
	dlqErr       error
}

type deadLetter struct {
	eventID string
	reason  string
}

func (f *fakeSource) Next(context.Context) ([]port.Message, error) {
	if len(f.batches) == 0 {
		return nil, nil
	}
	batch := f.batches[0]
	f.batches = f.batches[1:]
	return batch, nil
}

func (f *fakeSource) Ack(_ context.Context, m port.Message) error {
	f.acked = append(f.acked, m.EventID)
	f.settled = append(f.settled, "ack")
	return nil
}

func (f *fakeSource) Nak(_ context.Context, m port.Message) error {
	f.naked = append(f.naked, m.EventID)
	f.settled = append(f.settled, "nak")
	return nil
}

func (f *fakeSource) Term(_ context.Context, m port.Message) error {
	f.terminated = append(f.terminated, m.EventID)
	f.settled = append(f.settled, "term")
	return nil
}

func (f *fakeSource) Deadletter(_ context.Context, m port.Message, reason string) error {
	if f.dlqErr != nil {
		return f.dlqErr
	}
	f.deadlettered = append(f.deadlettered, deadLetter{eventID: m.EventID, reason: reason})
	f.settled = append(f.settled, "deadletter")
	return nil
}

type fakeDecoder struct {
	snapshot      port.RequestReceived
	opportunities port.OpportunitiesComputed
	snapshotErr   error
	opsErr        error
}

func (f fakeDecoder) RequestReceived([]byte) (port.RequestReceived, error) {
	return f.snapshot, f.snapshotErr
}

func (f fakeDecoder) Opportunities([]byte) (port.OpportunitiesComputed, error) {
	return f.opportunities, f.opsErr
}

type fakeProjections struct {
	snapshotCalls  int
	candidateCalls int
	applied        bool
	err            error
	sawConsumer    []string
}

func (f *fakeProjections) ProjectSnapshot(_ context.Context, consumer string, _ port.RequestReceived) (bool, error) {
	f.snapshotCalls++
	f.sawConsumer = append(f.sawConsumer, consumer)
	return f.applied, f.err
}

func (f *fakeProjections) ProjectCandidates(_ context.Context, consumer string, _ port.OpportunitiesComputed) (bool, error) {
	f.candidateCalls++
	f.sawConsumer = append(f.sawConsumer, consumer)
	return f.applied, f.err
}

func discard() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func message(stream, subject, eventID string) port.Message {
	return port.Message{
		Stream:    stream,
		Sequence:  1,
		Subject:   subject,
		EventID:   eventID,
		EventAt:   time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC),
		Payload:   []byte(`{}`),
		Delivered: 1,
	}
}

func validSnapshotEvent() port.RequestReceived {
	return port.RequestReceived{
		EventID: "e1",
		Snapshot: domain.Snapshot{
			RequestID:    "r1",
			CustomerID:   "acme",
			PriorityTier: "COMMERCIAL",
			BidCredits:   10,
			WindowStart:  time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC),
			WindowEnd:    time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC),
			SubmittedAt:  time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC),
		},
	}
}

func validCandidateEvent() port.OpportunitiesComputed {
	return port.OpportunitiesComputed{
		EventID:   "e2",
		RequestID: "r1",
		Candidates: []domain.Candidate{{
			OpportunityID:        "o1",
			RequestID:            "r1",
			SatelliteID:          "CAPELLA-14",
			Mode:                 "STRIPMAP",
			AccessStart:          time.Date(2026, 8, 7, 10, 14, 0, 0, time.UTC),
			AccessEnd:            time.Date(2026, 8, 7, 10, 16, 30, 0, time.UTC),
			AcquisitionDurationS: 18.5,
			DutyCycleCostS:       18.5,
			QualityScore:         0.8,
			GeometryJSON:         []byte(`{}`),
			FootprintGeoJSON:     []byte(`{}`),
		}},
	}
}

func TestSnapshotIsProjectedAndAcked(t *testing.T) {
	source := &fakeSource{batches: [][]port.Message{{
		message("TASKING", app.SubjectRequestReceived, "e1"),
	}}}
	projections := &fakeProjections{applied: true}

	stats, err := app.NewProjector(source, fakeDecoder{snapshot: validSnapshotEvent()}, projections, discard()).
		DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	if projections.snapshotCalls != 1 {
		t.Errorf("ProjectSnapshot called %d times, want 1", projections.snapshotCalls)
	}
	if stats.Applied != 1 {
		t.Errorf("applied = %d, want 1", stats.Applied)
	}
	if len(source.acked) != 1 || len(source.naked) != 0 {
		t.Errorf("acked %v, naked %v — want exactly one ack", source.acked, source.naked)
	}
	// The dedup ledger is partitioned by consumer. Writing the wrong partition
	// would let the same event be processed twice, once per stream.
	if len(projections.sawConsumer) != 1 || projections.sawConsumer[0] != port.ConsumerLifecycle {
		t.Errorf("wrote ledger partition %v, want %s", projections.sawConsumer, port.ConsumerLifecycle)
	}
}

func TestCandidatesAreProjectedAndAcked(t *testing.T) {
	source := &fakeSource{batches: [][]port.Message{{
		message("FEASIBILITY", app.SubjectOpportunities, "e2"),
	}}}
	projections := &fakeProjections{applied: true}

	stats, err := app.NewProjector(source, fakeDecoder{opportunities: validCandidateEvent()}, projections, discard()).
		DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	if projections.candidateCalls != 1 {
		t.Errorf("ProjectCandidates called %d times, want 1", projections.candidateCalls)
	}
	if stats.Applied != 1 {
		t.Errorf("applied = %d, want 1", stats.Applied)
	}
	if len(projections.sawConsumer) != 1 || projections.sawConsumer[0] != port.ConsumerOpportunities {
		t.Errorf("wrote ledger partition %v, want %s", projections.sawConsumer, port.ConsumerOpportunities)
	}
}

// A redelivery is a NORMAL outcome, not a failure. It must still be acked, or
// the consumer redelivers forever and the message eventually reaches the DLQ
// having been handled correctly every single time.
func TestRedeliveryIsAckedNotRetried(t *testing.T) {
	source := &fakeSource{batches: [][]port.Message{{
		message("TASKING", app.SubjectRequestReceived, "e1"),
	}}}
	projections := &fakeProjections{applied: false} // already in the ledger

	stats, err := app.NewProjector(source, fakeDecoder{snapshot: validSnapshotEvent()}, projections, discard()).
		DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	if stats.Duplicate != 1 || stats.Applied != 0 {
		t.Errorf("stats = %+v, want one duplicate and no applications", stats)
	}
	if len(source.acked) != 1 || len(source.naked) != 0 {
		t.Errorf("acked %v, naked %v — a duplicate must be acked", source.acked, source.naked)
	}
}

// planner-lifecycle filters on tasking.request.> — broader than what is
// projected. A sibling must be acked and forgotten, not treated as an error,
// or every rejected request would poison the consumer.
func TestSiblingSubjectIsIgnoredAndAcked(t *testing.T) {
	source := &fakeSource{batches: [][]port.Message{{
		message("TASKING", "tasking.request.rejected.v1", "e9"),
	}}}
	projections := &fakeProjections{applied: true}

	stats, err := app.NewProjector(source, fakeDecoder{}, projections, discard()).
		DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	if projections.snapshotCalls != 0 || projections.candidateCalls != 0 {
		t.Error("a sibling subject reached the projections")
	}
	if stats.Ignored != 1 {
		t.Errorf("ignored = %d, want 1", stats.Ignored)
	}
	if len(source.acked) != 1 {
		t.Errorf("acked %v — an ignored message must still be acked", source.acked)
	}
}

// A payload that does not decode will not decode on redelivery either:
// PERMANENT, terminated on the FIRST delivery. The M2 behaviour — Nak
// everything and let max_deliver bound the cost — burned the whole delivery
// budget rerunning a deterministic failure; the demo's poison "hello" messages
// showed exactly that, redelivering for an hour.
func TestDecodeFailureIsTerminatedOnFirstDelivery(t *testing.T) {
	source := &fakeSource{batches: [][]port.Message{{
		message("TASKING", app.SubjectRequestReceived, "e1"),
	}}}
	projections := &fakeProjections{applied: true}

	projector := app.NewProjector(source,
		fakeDecoder{snapshotErr: errors.New("not an enveloped contract event")},
		projections, discard())
	stats, err := projector.DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	if stats.Failed != 1 {
		t.Errorf("failed = %d, want 1", stats.Failed)
	}
	if len(source.terminated) != 1 || len(source.acked) != 0 || len(source.naked) != 0 {
		t.Errorf("acked %v, naked %v, terminated %v — a deterministic failure must Term, once",
			source.acked, source.naked, source.terminated)
	}
	if projections.snapshotCalls != 0 {
		t.Error("an undecodable payload reached the database")
	}
	if projector.Metrics.Snapshot().Terminated != 1 {
		t.Error("the Term left no metric; the drop would be invisible to M3-06")
	}
}

// A TRANSIENT failure still retries — and on the LAST delivery terminates
// deliberately rather than letting max_deliver lapse into a silent drop.
func TestTransientFailureNaksUntilTheLastDeliveryThenTerms(t *testing.T) {
	early := message("FEASIBILITY", app.SubjectOpportunities, "e2")
	last := message("FEASIBILITY", app.SubjectOpportunities, "e2")
	last.Delivered = 5 // deploy/nats/init.sh: max_deliver 5

	source := &fakeSource{batches: [][]port.Message{{early}, {last}}}
	projections := &fakeProjections{err: errors.New("connection refused")}
	projector := app.NewProjector(source, fakeDecoder{opportunities: validCandidateEvent()}, projections, discard())

	if _, err := projector.DrainOnce(context.Background()); err != nil {
		t.Fatalf("drain 1: %v", err)
	}
	if len(source.naked) != 1 || len(source.terminated) != 0 {
		t.Fatalf("first delivery: naked %v terminated %v, want one Nak", source.naked, source.terminated)
	}

	if _, err := projector.DrainOnce(context.Background()); err != nil {
		t.Fatalf("drain 2: %v", err)
	}
	if len(source.terminated) != 1 {
		t.Fatalf("final delivery: terminated %v, want the deliberate Term", source.terminated)
	}
	if projector.Metrics.Snapshot().Redeliveries != 1 {
		t.Error("the redelivery left no metric; the early-warning line is dark")
	}
}

// One bad candidate fails the WHOLE batch, and nothing is written.
//
// A partially projected event would let a round allocate over some of a
// request's options while the rest were never offered, then report the
// difference as unfulfilment — telling a customer they lost a competition that
// was never run.
func TestOneInvalidCandidateRejectsTheWholeBatch(t *testing.T) {
	event := validCandidateEvent()
	good := event.Candidates[0]
	bad := good
	bad.OpportunityID = "o2"
	bad.Mode = "PANCHROMATIC" // not one of the three
	event.Candidates = []domain.Candidate{good, bad}

	source := &fakeSource{batches: [][]port.Message{{
		message("FEASIBILITY", app.SubjectOpportunities, "e2"),
	}}}
	projections := &fakeProjections{applied: true}

	stats, err := app.NewProjector(source, fakeDecoder{opportunities: event}, projections, discard()).
		DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	if projections.candidateCalls != 0 {
		t.Error("a batch containing an invalid candidate was partially written")
	}
	if stats.Failed != 1 {
		t.Errorf("failed = %d, want 1", stats.Failed)
	}
	// An invalid candidate is invalid on every redelivery: permanent, Term.
	if len(source.terminated) != 1 {
		t.Errorf("terminated %v, want the deliberate Term", source.terminated)
	}
}

// The ordering invariant, tested as an ordering: publish, THEN Term.
//
// A Term that lands before the dead letter is a drop with extra steps, and it
// looks identical to the correct sequence in every counter — which is why this
// asserts the sequence rather than the pair of facts (ADR-0017).
func TestAPoisonMessageIsDeadLetteredBeforeItIsTermed(t *testing.T) {
	source := &fakeSource{batches: [][]port.Message{{
		message("TASKING", app.SubjectRequestReceived, "e1"),
	}}}

	projector := app.NewProjector(source,
		fakeDecoder{snapshotErr: errors.New("not an enveloped contract event")},
		&fakeProjections{applied: true}, discard())
	if _, err := projector.DrainOnce(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if got := strings.Join(source.settled, ","); got != "deadletter,term" {
		t.Fatalf("settled %q, want deadletter,term — a Term before the dead letter is the loss this prevents", got)
	}
	if len(source.deadlettered) != 1 || source.deadlettered[0].eventID != "e1" {
		t.Fatalf("dead-lettered %+v, want the poison event once", source.deadlettered)
	}
	// The planner's permanent class is "the payload is one this service
	// refuses" — a decode failure or a contract violation, indistinguishable
	// here because both arrive wrapped in domain.ErrInvalid.
	if source.deadlettered[0].reason != consume.ReasonContract {
		t.Errorf("reason = %q, want %q", source.deadlettered[0].reason, consume.ReasonContract)
	}
	if got := projector.Metrics.Snapshot().Deadlettered; got != 1 {
		t.Errorf("deadlettered metric = %d, want 1", got)
	}
}

// The exhausted retry says so, rather than borrowing the poison reason. An
// operator triages on this header: "the payload is bad" and "the dependency was
// down for five deliveries" are different incidents with different fixes.
func TestAnExhaustedRetryIsDeadLetteredAsExhausted(t *testing.T) {
	last := message("FEASIBILITY", app.SubjectOpportunities, "e2")
	last.Delivered = 5 // deploy/nats/init.sh: max_deliver 5

	source := &fakeSource{batches: [][]port.Message{{last}}}
	projector := app.NewProjector(source, fakeDecoder{opportunities: validCandidateEvent()},
		&fakeProjections{err: errors.New("connection refused")}, discard())
	if _, err := projector.DrainOnce(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if len(source.deadlettered) != 1 || source.deadlettered[0].reason != consume.ReasonExhausted {
		t.Fatalf("dead-lettered %+v, want one %q", source.deadlettered, consume.ReasonExhausted)
	}
}

// If the dead letter cannot be published, NAK. The delivery retries in full —
// the handling and the dead-lettering — and both are idempotent, so the retry
// is boring. Terminating here would be the silent drop with a metric attached.
func TestAFailedDeadLetterNaksRatherThanDropping(t *testing.T) {
	source := &fakeSource{
		batches: [][]port.Message{{message("TASKING", app.SubjectRequestReceived, "e1")}},
		dlqErr:  errors.New("no responders available for request"),
	}

	projector := app.NewProjector(source,
		fakeDecoder{snapshotErr: errors.New("not an enveloped contract event")},
		&fakeProjections{applied: true}, discard())
	if _, err := projector.DrainOnce(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if len(source.terminated) != 0 {
		t.Errorf("terminated %v after a failed dead letter — the payload is gone", source.terminated)
	}
	if len(source.naked) != 1 {
		t.Errorf("naked %v, want the one retry that re-runs both halves", source.naked)
	}
	snapshot := projector.Metrics.Snapshot()
	if snapshot.Deadlettered != 0 || snapshot.Terminated != 0 {
		t.Errorf("metrics %+v claim a drop that did not happen", snapshot)
	}
}

func TestProjectionErrorIsNaked(t *testing.T) {
	source := &fakeSource{batches: [][]port.Message{{
		message("FEASIBILITY", app.SubjectOpportunities, "e2"),
	}}}
	projections := &fakeProjections{err: errors.New("connection refused")}

	stats, err := app.NewProjector(source, fakeDecoder{opportunities: validCandidateEvent()}, projections, discard()).
		DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	if stats.Failed != 1 {
		t.Errorf("failed = %d, want 1", stats.Failed)
	}
	if len(source.acked) != 0 {
		t.Errorf("acked %v after a database failure — the event would be lost", source.acked)
	}
	if len(source.naked) != 1 {
		t.Errorf("naked %v — a database failure is transient and must retry, not Term", source.naked)
	}
}

// A stream this projector never bound has no ledger partition, so it cannot be
// deduplicated. Ignoring it is the only safe answer; failing would poison a
// consumer over a topology change that does not concern it.
func TestUnknownStreamIsIgnored(t *testing.T) {
	source := &fakeSource{batches: [][]port.Message{{
		message("PLANNING", app.SubjectOpportunities, "e5"),
	}}}
	projections := &fakeProjections{applied: true}

	stats, err := app.NewProjector(source, fakeDecoder{opportunities: validCandidateEvent()}, projections, discard()).
		DrainOnce(context.Background())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	if projections.candidateCalls != 0 {
		t.Error("a message from an unbound stream was projected")
	}
	if stats.Ignored != 1 {
		t.Errorf("ignored = %d, want 1", stats.Ignored)
	}
}

// Run must return on cancellation rather than spin, and it must be exercised
// by running to completion rather than only by being cancelled — a shutdown
// path only ever reached by cancellation is a path never tested.
func TestRunStopsAfterMaxIterations(t *testing.T) {
	source := &fakeSource{batches: [][]port.Message{{
		message("TASKING", app.SubjectRequestReceived, "e1"),
	}}}
	projector := app.NewProjector(source, fakeDecoder{snapshot: validSnapshotEvent()},
		&fakeProjections{applied: true}, discard())

	done := make(chan error, 1)
	go func() { done <- projector.Run(context.Background(), 2, time.Millisecond) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its iteration budget")
	}
}

func TestRunReturnsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	projector := app.NewProjector(&fakeSource{}, fakeDecoder{}, &fakeProjections{}, discard())

	done := make(chan error, 1)
	go func() { done <- projector.Run(ctx, 0, time.Hour) }()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancellation is a clean stop, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run ignored cancellation")
	}
}
