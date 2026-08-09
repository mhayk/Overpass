package integration_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/mhayk/overpass/lib/go/consume"
)

// The second half of the operational loop, against a real broker and the real
// script an operator would run.
//
// The first half — a terminal failure landing in a DLQ stream with its headers
// — is covered per service in each natsmsg package. What only this level can
// check is the tooling: scripts/dlq-replay.sh reads the headers this repository
// writes, republishes to the subject they name, and deletes the entry only
// after seeing the message land. A replay tool that half works turns an
// incident into data loss, and it is used exactly when nobody is calm.

// jsPublisher is consume.Publisher over the harness's JetStream context.
type jsPublisher struct{ js nats.JetStreamContext }

func (p jsPublisher) Publish(ctx context.Context, subject string, header map[string][]string, payload []byte) error {
	_, err := p.js.PublishMsg(&nats.Msg{
		Subject: subject,
		Header:  nats.Header(header),
		Data:    payload,
	}, nats.Context(ctx))
	return err
}

func TestADeadLetterIsReplayedBackOntoItsOriginalSubject(t *testing.T) {
	ctx := t.Context()
	eventID := fmt.Sprintf("dlq-replay-%d", time.Now().UnixNano())

	// A payload containing a Go template delimiter, deliberately.
	//
	// `nats pub` renders its body as a Go template — measured, not assumed:
	// {{Count}} comes back as "1", through stdin as well as the argument. The
	// script escapes it, and this payload is what proves the escaping is still
	// there. Byte-identical is the whole promise of a replay; a payload quietly
	// rewritten on the way back is worse than one left in the queue.
	payload := []byte(`{"event_id":"` + eventID + `","note":"{{Count}} braces }} survive"}`)
	const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	err := consume.Deadletter(ctx, jsPublisher{js: env.js}, consume.DeadLetter{
		Subject:     "tasking.request.received.v1",
		EventID:     eventID,
		Payload:     payload,
		Traceparent: traceparent,
		Reason:      consume.ReasonContract,
		Delivered:   5,
		Consumer:    "planner-lifecycle",
	})
	if err != nil {
		t.Fatalf("seeding a dead letter: %v", err)
	}

	dead, err := env.js.GetLastMsg("DLQ_TASKING", "dlq.tasking.request.received.v1")
	if err != nil {
		t.Fatalf("the seeded dead letter is not in DLQ_TASKING: %v", err)
	}
	if got := dead.Header.Get(consume.HeaderMsgID); got != eventID {
		t.Fatalf("newest dead letter is %q, not this test's %q", got, eventID)
	}

	replay(t, "DLQ_TASKING", "--event-id", eventID)

	// Back on the original subject, byte for byte, under the original id — the
	// id is what lets the consumer ledgers arbitrate the replay as a duplicate
	// or as new work without anyone coordinating.
	replayed, err := env.js.GetLastMsg("TASKING", "tasking.request.received.v1")
	if err != nil {
		t.Fatalf("nothing was republished to tasking.request.received.v1: %v", err)
	}
	if got := replayed.Header.Get(consume.HeaderMsgID); got != eventID {
		t.Fatalf("newest message on the original subject is %q, want the replayed %q", got, eventID)
	}
	if string(replayed.Data) != string(payload) {
		t.Errorf("replayed payload = %s\n            want %s", replayed.Data, payload)
	}
	if got := replayed.Header.Get(consume.HeaderTraceparent); got != traceparent {
		t.Errorf("traceparent = %q, want it carried through so the replay ties back to the failure", got)
	}

	// And gone from the queue, because depth is what the alert fires on: a
	// replay that leaves the entry behind means depth never returns to zero and
	// the alert becomes noise an operator learns to ignore.
	if _, err := env.js.GetMsg("DLQ_TASKING", dead.Sequence); err == nil {
		t.Errorf("DLQ_TASKING#%d is still there after a successful replay", dead.Sequence)
	}
}

// A replay that cannot confirm the message landed must keep the dead letter.
//
// `nats pub` is a core publish: it reports success whether or not a stream
// stored anything. Deleting on that alone would turn a topology mistake — a
// subject no stream covers any more — into exactly the loss the DLQ exists to
// prevent, and it would do it silently, with a success message on screen.
func TestAReplayThatCannotConfirmDeliveryKeepsTheDeadLetter(t *testing.T) {
	ctx := t.Context()
	eventID := fmt.Sprintf("dlq-unstored-%d", time.Now().UnixNano())

	// tasking.> reaches DLQ_TASKING's sibling; nowhere.at.all.v1 reaches no
	// stream at all, which is what a decommissioned subject looks like.
	err := consume.Deadletter(ctx, jsPublisher{js: env.js}, consume.DeadLetter{
		Subject:   "tasking.gone.v1",
		EventID:   eventID,
		Payload:   []byte(`{"event_id":"` + eventID + `"}`),
		Reason:    consume.ReasonContract,
		Delivered: 5,
		Consumer:  "planner-lifecycle",
	})
	if err != nil {
		t.Fatalf("seeding a dead letter: %v", err)
	}
	dead, err := env.js.GetLastMsg("DLQ_TASKING", "dlq.tasking.gone.v1")
	if err != nil {
		t.Fatalf("the seeded dead letter is not in DLQ_TASKING: %v", err)
	}

	// Rewrite the original subject to one no stream covers. Seeding it that way
	// directly is not possible: Deadletter would publish to dlq.nowhere.>,
	// which no DLQ stream captures either.
	stripped := nats.NewMsg("dlq.tasking.gone.v1")
	stripped.Data = dead.Data
	stripped.Header = nats.Header{}
	for key, values := range dead.Header {
		stripped.Header[key] = append([]string(nil), values...)
	}
	stripped.Header.Set(consume.HeaderOriginalSubject, "nowhere.at.all.v1")
	stripped.Header.Set(consume.HeaderMsgID, eventID+"-unroutable")
	if _, err := env.js.PublishMsg(stripped, nats.Context(ctx)); err != nil {
		t.Fatalf("seeding the unroutable dead letter: %v", err)
	}
	unroutable, err := env.js.GetLastMsg("DLQ_TASKING", "dlq.tasking.gone.v1")
	if err != nil {
		t.Fatalf("reading back the unroutable dead letter: %v", err)
	}

	out, err := runReplay(t, "DLQ_TASKING", "--seq", fmt.Sprint(unroutable.Sequence))
	if err == nil {
		t.Fatalf("the replay reported success for a message no stream stored:\n%s", out)
	}
	// The REASON, not just the exit code. A script that dies because its own
	// dependencies are missing also exits non-zero, and this test would then
	// pass while proving nothing about the safety check — which is how a test
	// ends up guarding an empty room.
	if !strings.Contains(out, "keeping the dead letter") {
		t.Fatalf("the replay failed for the wrong reason; the refusal must be the delivery check:\n%s", out)
	}

	if _, err := env.js.GetMsg("DLQ_TASKING", unroutable.Sequence); err != nil {
		t.Fatalf("the dead letter was deleted after an unconfirmed replay: %v", err)
	}
}

func replay(t *testing.T, stream string, args ...string) {
	t.Helper()
	out, err := runReplay(t, stream, args...)
	if err != nil {
		t.Fatalf("scripts/dlq-replay.sh failed: %v\n%s", err, out)
	}
	t.Logf("dlq-replay.sh:\n%s", out)
}

func runReplay(t *testing.T, stream string, args ...string) (string, error) {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("locating the repository root: %v", err)
	}
	// The script, not a Go reimplementation of it. A test that reimplements the
	// tool proves the test works.
	cmd := exec.CommandContext(t.Context(), //nolint:gosec // fixed script path, test-controlled args
		filepath.Join(root, "scripts", "dlq-replay.sh"),
		append([]string{stream}, args...)...)
	cmd.Env = append(os.Environ(), "NATS_URL="+env.natsURL)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
