package consume_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mhayk/overpass/lib/go/consume"
)

// recorder is the publisher the adapter would be. It records rather than
// asserts, so every test below reads what was actually put on the wire.
type recorder struct {
	calls   int
	subject string
	header  map[string][]string
	payload []byte
	err     error
}

func (r *recorder) Publish(_ context.Context, subject string, header map[string][]string, payload []byte) error {
	r.calls++
	r.subject, r.header, r.payload = subject, header, payload
	return r.err
}

func (r *recorder) get(t *testing.T, key string) string {
	t.Helper()
	values := r.header[key]
	if len(values) != 1 {
		t.Fatalf("header %s = %v, want exactly one value", key, values)
	}
	return values[0]
}

func sample() consume.DeadLetter {
	return consume.DeadLetter{
		Subject:     "tasking.request.received.v1",
		EventID:     "9b2f0c0e-7f6f-4f6c-8f0c-2b3a4d5e6f70",
		Payload:     []byte(`{"event_id":"9b2f0c0e-7f6f-4f6c-8f0c-2b3a4d5e6f70"}`),
		Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		Reason:      consume.ReasonContract,
		Delivered:   5,
		Consumer:    "planner-lifecycle",
		FailedAt:    time.Date(2026, 8, 9, 14, 30, 0, 0, time.UTC),
	}
}

// The prefix, not an infix: tasking.dlq.> is already inside the TASKING
// stream's tasking.> wildcard and NATS refuses overlapping streams. The
// contract says dlq.<original>, and the string operation must be exactly that
// so the replay tool can reverse it by trimming.
func TestDeadLetterGoesToThePrefixedSubject(t *testing.T) {
	var pub recorder
	if err := consume.Deadletter(t.Context(), &pub, sample()); err != nil {
		t.Fatalf("dead-lettering: %v", err)
	}
	if pub.subject != "dlq.tasking.request.received.v1" {
		t.Errorf("published to %q, want dlq.tasking.request.received.v1", pub.subject)
	}
}

// The header set is the contract (contracts/nats/topology.md). An inspection
// tool reads these names; a typo here is a tool that silently shows nothing.
func TestDeadLetterCarriesTheContractHeaderSet(t *testing.T) {
	var pub recorder
	if err := consume.Deadletter(t.Context(), &pub, sample()); err != nil {
		t.Fatalf("dead-lettering: %v", err)
	}

	for _, want := range []struct{ header, value string }{
		{"Overpass-Dlq-Reason", "contract"},
		{"Overpass-Dlq-Original-Subject", "tasking.request.received.v1"},
		{"Overpass-Dlq-Delivery-Count", "5"},
		{"Overpass-Dlq-Failed-At", "2026-08-09T14:30:00Z"},
		{"Overpass-Dlq-Consumer", "planner-lifecycle"},
		{"traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
	} {
		if got := pub.get(t, want.header); got != want.value {
			t.Errorf("%s = %q, want %q", want.header, got, want.value)
		}
	}

	if string(pub.payload) != string(sample().Payload) {
		t.Errorf("payload = %q, want it byte-identical to the original", pub.payload)
	}
}

// The dead letter keeps the original id so the DLQ stream's dedup window
// absorbs the duplicate produced by a crash between publish and Term — the one
// consequence ADR-0017 accepts in exchange for never losing the message.
func TestDeadLetterKeepsTheOriginalEventIDAsItsMessageID(t *testing.T) {
	var pub recorder
	if err := consume.Deadletter(t.Context(), &pub, sample()); err != nil {
		t.Fatalf("dead-lettering: %v", err)
	}
	if got := pub.get(t, "Nats-Msg-Id"); got != sample().EventID {
		t.Errorf("Nats-Msg-Id = %q, want the original event id %q", got, sample().EventID)
	}
}

// A message with no id is exactly the kind that dies here — an envelope that
// would not parse. Refusing it would leave the call site Naking forever under
// ADR-0017's ordering, which is the loss this whole issue exists to prevent.
// It lands without a Msg-Id instead: no dedup, still recoverable.
func TestADeadLetterWithoutAnEventIDStillLands(t *testing.T) {
	d := sample()
	d.EventID = ""

	var pub recorder
	if err := consume.Deadletter(t.Context(), &pub, d); err != nil {
		t.Fatalf("dead-lettering an id-less message: %v", err)
	}
	if pub.calls != 1 {
		t.Fatalf("published %d times, want 1 — an id-less message must not be refused", pub.calls)
	}
	if _, present := pub.header["Nats-Msg-Id"]; present {
		t.Errorf("Nats-Msg-Id = %v, want the header absent rather than empty", pub.header["Nats-Msg-Id"])
	}
}

// An empty traceparent is omitted, not written blank: a blank one parses as a
// broken trace context downstream, where absent parses as "no trace".
func TestAnAbsentTraceparentIsOmittedRatherThanBlank(t *testing.T) {
	d := sample()
	d.Traceparent = ""

	var pub recorder
	if err := consume.Deadletter(t.Context(), &pub, d); err != nil {
		t.Fatalf("dead-lettering: %v", err)
	}
	if _, present := pub.header["traceparent"]; present {
		t.Errorf("traceparent = %v, want the header absent", pub.header["traceparent"])
	}
}

// The stamp is the time of the terminal decision (ADR-0017's amendment), and a
// caller that does not supply one gets the clock rather than a zero year.
func TestAnUnsetFailedAtIsStampedFromTheClock(t *testing.T) {
	d := sample()
	d.FailedAt = time.Time{}

	before := time.Now().Add(-time.Second)
	var pub recorder
	if err := consume.Deadletter(t.Context(), &pub, d); err != nil {
		t.Fatalf("dead-lettering: %v", err)
	}

	stamped, err := time.Parse(time.RFC3339, pub.get(t, "Overpass-Dlq-Failed-At"))
	if err != nil {
		t.Fatalf("Overpass-Dlq-Failed-At is not RFC 3339: %v", err)
	}
	if stamped.Before(before) || stamped.After(time.Now().Add(time.Second)) {
		t.Errorf("Overpass-Dlq-Failed-At = %s, want roughly now", stamped)
	}
}

// These four are call-site facts, never message data: every one is a literal or
// a consumer name known at wiring time. An empty one is a programming error,
// and a dead letter that reaches the DLQ without a reason or a consumer is an
// operator staring at a payload with no idea who gave up on it or why.
func TestDeadletterRefusesAnIncompleteCallSite(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*consume.DeadLetter)
		wantErr string
	}{
		{"no subject", func(d *consume.DeadLetter) { d.Subject = "" }, "subject"},
		{"no reason", func(d *consume.DeadLetter) { d.Reason = "" }, "reason"},
		{"no consumer", func(d *consume.DeadLetter) { d.Consumer = "" }, "consumer"},
		{
			// dlq.dlq.tasking.> is a stream nobody declared. A consumer bound to
			// a DLQ stream that dead-letters again is a wiring mistake, and it
			// must be loud rather than create a subject space by accident.
			"already dead", func(d *consume.DeadLetter) { d.Subject = "dlq.tasking.request.received.v1" }, "already",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := sample()
			tc.mutate(&d)

			var pub recorder
			err := consume.Deadletter(t.Context(), &pub, d)
			if err == nil {
				t.Fatalf("dead-lettering with %s succeeded, want a refusal", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
			if pub.calls != 0 {
				t.Errorf("published %d times, want 0 — the refusal must precede the publish", pub.calls)
			}
		})
	}
}

// The caller decides between Term and Nak on this error, so it must arrive
// intact and identifiable rather than swallowed into a generic failure.
func TestAFailedPublishIsReportedSoTheCallerCanNak(t *testing.T) {
	broken := errors.New("no responders available for request")
	pub := recorder{err: broken}

	err := consume.Deadletter(t.Context(), &pub, sample())
	if err == nil {
		t.Fatal("a failed publish reported success; the caller would Term and lose the message")
	}
	if !errors.Is(err, broken) {
		t.Errorf("error %q does not wrap the publish failure", err)
	}
}

func TestMetricsCountDeadLetters(t *testing.T) {
	var m consume.Metrics
	m.Deadlettered()
	m.Deadlettered()

	if got := m.Snapshot().Deadlettered; got != 2 {
		t.Errorf("deadlettered = %d, want 2", got)
	}
}
