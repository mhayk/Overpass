package natsmsg_test

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/mhayk/overpass/lib/go/consume"

	"github.com/mhayk/overpass/services/plan-gateway/internal/adapter/natsmsg"
	"github.com/mhayk/overpass/services/plan-gateway/internal/port"
)

// Against a real broker, because the claim being tested is about the broker.
//
// Bind resolves consumer names that are declared in deploy/nats/init.sh and
// nowhere in this code. A fake would agree with whatever this file asserts and
// prove nothing about whether the names match the topology — which is the only
// way this package fails in practice.

func connect(t *testing.T) (nats.JetStreamContext, *nats.Conn) {
	t.Helper()
	url := os.Getenv("OVERPASS_TEST_NATS_URL")
	if url == "" {
		t.Skip("set OVERPASS_TEST_NATS_URL to run the broker tests")
	}
	conn, err := nats.Connect(url, nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return js, conn
}

// TestBindFindsTheDeclaredConsumers is the whole point of this file.
//
// The durable names live in deploy/nats/init.sh; this code only knows a prefix
// and a lowercase stream name. Nothing but a real bind can tell you whether
// those two halves still agree.
func TestBindFindsTheDeclaredConsumers(t *testing.T) {
	js, _ := connect(t)

	source, err := natsmsg.Bind(js, "gateway-projector", 8, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("bind against the declared topology failed: %v\n"+
			"  the consumer names here must match deploy/nats/init.sh", err)
	}
	t.Cleanup(func() {
		if drainErr := source.Drain(); drainErr != nil {
			t.Errorf("drain: %v", drainErr)
		}
	})
}

// TestBindRefusesAConsumerThatDoesNotExist is the guard on the guard. If Bind
// succeeded against a nonsense prefix, the test above would prove nothing.
func TestBindRefusesAConsumerThatDoesNotExist(t *testing.T) {
	js, _ := connect(t)

	if _, err := natsmsg.Bind(js, "no-such-projector", 8, 200*time.Millisecond); err == nil {
		t.Fatal("bound to a consumer that was never declared; Bind proves nothing")
	}
}

// TestAPublishedEventComesBackWithItsMetadata covers the round trip: publish,
// fetch, convert, ack.
func TestAPublishedEventComesBackWithItsMetadata(t *testing.T) {
	js, _ := connect(t)
	source, err := natsmsg.Bind(js, "gateway-projector", 8, 2*time.Second)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	t.Cleanup(func() { _ = source.Drain() }) //nolint:errcheck // teardown

	occurred := time.Date(2026, 3, 1, 11, 30, 0, 0, time.UTC)
	eventID := "itest-" + time.Now().UTC().Format("150405.000000000")
	msg := &nats.Msg{
		Subject: "tasking.request.received.v1",
		Data:    []byte(`{"request_id":"itest-1"}`),
		Header: nats.Header{
			"Nats-Msg-Id": []string{eventID},
			"Occurred-At": []string{occurred.Format(time.RFC3339Nano)},
		},
	}
	if _, err := js.PublishMsg(msg); err != nil {
		t.Fatalf("publish: %v", err)
	}

	got, ok := drainFor(t, source, eventID)
	if !ok {
		t.Fatal("the published event never came back from the consumer")
	}
	if got.Stream != "TASKING" {
		t.Errorf("Stream = %q, want TASKING", got.Stream)
	}
	if got.Sequence == 0 {
		t.Error("Sequence is zero; the ordering guard would have nothing to compare")
	}
	if !got.EventAt.Equal(occurred) {
		t.Errorf("EventAt = %s, want the producer's %s", got.EventAt, occurred)
	}
	if err := source.Ack(t.Context(), got); err != nil {
		t.Fatalf("ack: %v", err)
	}
	// The handle is gone after one ack, so a double ack is loud rather than
	// silently absorbed by the broker.
	if err := source.Ack(t.Context(), got); err == nil {
		t.Error("acked the same message twice without complaint")
	}
}

// drainFor pulls until it sees the event it is looking for, acking everything
// else. The streams are shared with the rest of the stack, so a fetch can
// return anyone's messages.
func drainFor(t *testing.T, source *natsmsg.Source, eventID string) (port.Message, bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		batch, err := source.Next(t.Context())
		if err != nil && !errors.Is(err, nats.ErrTimeout) {
			t.Fatalf("next: %v", err)
		}
		for _, m := range batch {
			if m.EventID == eventID {
				return m, true
			}
			if ackErr := source.Ack(t.Context(), m); ackErr != nil {
				t.Logf("acking an unrelated message: %v", ackErr)
			}
		}
	}
	return port.Message{}, false
}

// The claim #49 actually makes: a terminated message is still there afterwards.
//
// The subject mapping and the DLQ streams' subject filters are declared in two
// different files, and the consumer name in the header is built here from a
// prefix and a stream name. Only a real broker can say all three agree — and a
// misattributed dead letter sends an operator to the wrong service.
func TestADeadLetteredMessageLandsInTheDlqStreamWithItsHeaders(t *testing.T) {
	js, _ := connect(t)

	source, bindErr := natsmsg.Bind(js, "gateway-projector", 8, 200*time.Millisecond)
	if bindErr != nil {
		t.Fatalf("bind: %v", bindErr)
	}
	t.Cleanup(func() {
		if drainErr := source.Drain(); drainErr != nil {
			t.Errorf("drain: %v", drainErr)
		}
	})

	const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	eventID := "dddddddd-0000-4000-8000-" + time.Now().UTC().Format("150405.000000")[:12]
	payload := []byte(`{"event_id":"` + eventID + `","poison":true}`)

	msg := nats.NewMsg("planning.plan.committed.v1")
	msg.Data = payload
	msg.Header.Set("Nats-Msg-Id", eventID)
	msg.Header.Set("traceparent", traceparent)
	if _, err := js.PublishMsg(msg); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	got, found := drainFor(t, source, eventID)
	if !found {
		t.Fatal("the poison message never arrived; nothing was dead-lettered")
	}
	// The projector's sequence, exactly: publish, then Term.
	if err := source.Deadletter(t.Context(), got, consume.ReasonContract); err != nil {
		t.Fatalf("dead-lettering: %v", err)
	}
	if err := source.Term(t.Context(), got); err != nil {
		t.Fatalf("terming after the dead letter: %v", err)
	}

	stored, err := js.GetLastMsg("DLQ_PLANNING", consume.SubjectPrefix+"planning.plan.committed.v1")
	if err != nil {
		t.Fatalf("reading DLQ_PLANNING: %v — the dlq. subject and the stream's filter disagree", err)
	}
	if id := stored.Header.Get(consume.HeaderMsgID); id != eventID {
		t.Fatalf("last dead letter is %q, not the one this test made (%q)", id, eventID)
	}
	if string(stored.Data) != string(payload) {
		t.Errorf("payload = %q, want it byte-identical — replay republishes these bytes", stored.Data)
	}
	// Built here from a prefix and a stream name, so it is the header most
	// likely to be quietly wrong.
	if got := stored.Header.Get(consume.HeaderConsumer); got != "gateway-projector-planning" {
		t.Errorf("%s = %q, want gateway-projector-planning", consume.HeaderConsumer, got)
	}
	if got := stored.Header.Get(consume.HeaderTraceparent); got != traceparent {
		t.Errorf("traceparent = %q, want it preserved so the failure stays inspectable", got)
	}
	if got := stored.Header.Get(consume.HeaderReason); got != consume.ReasonContract {
		t.Errorf("%s = %q, want %q", consume.HeaderReason, got, consume.ReasonContract)
	}
}
