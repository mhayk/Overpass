package integration_test

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// The topology, declared here in Go because the test broker has no `nats` CLI
// to run deploy/nats/init.sh with.
//
// That is a duplicate, and a duplicate is a thing that drifts. So
// TestTheTestTopologyMatchesTheDeployedOne parses init.sh and requires the two
// to agree — the tests would otherwise pass against a broker shaped nothing
// like the one that gets deployed, which is a worse failure than not testing at
// all because it looks like evidence.

type streamSpec struct {
	name     string
	subjects []string
	maxAge   time.Duration
}

type consumerSpec struct {
	stream     string
	name       string
	filters    []string
	ackWait    time.Duration
	maxDeliver int
	maxPending int
}

var streams = []streamSpec{
	{"TASKING", []string{"tasking.>"}, 168 * time.Hour},
	{"FEASIBILITY", []string{"feasibility.>"}, 72 * time.Hour},
	{"PLANNING", []string{"planning.>", "acquisition.>"}, 168 * time.Hour},
	{"DLQ_TASKING", []string{"dlq.tasking.>"}, 720 * time.Hour},
	{"DLQ_FEASIBILITY", []string{"dlq.feasibility.>"}, 720 * time.Hour},
	{"DLQ_PLANNING", []string{"dlq.planning.>", "dlq.acquisition.>"}, 720 * time.Hour},
}

var consumers = []consumerSpec{
	{"TASKING", "feasibility-worker", []string{"tasking.request.received.v1"}, 120 * time.Second, 5, 64},
	{"FEASIBILITY", "planner-opportunities", []string{"feasibility.opportunities.computed.v1"}, 60 * time.Second, 5, 32},
	{"TASKING", "planner-lifecycle", []string{"tasking.request.>"}, 30 * time.Second, 5, 64},
	{"PLANNING", "simulator-executor", []string{"planning.plan.committed.v1"}, 60 * time.Second, 3, 16},
	{"TASKING", "gateway-projector-tasking", []string{"tasking.>"}, 30 * time.Second, 10, 256},
	{"FEASIBILITY", "gateway-projector-feasibility", []string{"feasibility.>"}, 30 * time.Second, 10, 256},
	{"PLANNING", "gateway-projector-planning", []string{"planning.>", "acquisition.>"}, 30 * time.Second, 10, 256},
}

func applyTopology(js nats.JetStreamContext) error {
	for _, s := range streams {
		if _, err := js.AddStream(&nats.StreamConfig{
			Name:      s.name,
			Subjects:  s.subjects,
			MaxAge:    s.maxAge,
			Storage:   nats.FileStorage,
			Retention: nats.LimitsPolicy,
		}); err != nil {
			return fmt.Errorf("creating stream %s: %w", s.name, err)
		}
	}

	for _, c := range consumers {
		cfg := &nats.ConsumerConfig{
			Durable:       c.name,
			DeliverPolicy: nats.DeliverAllPolicy,
			AckPolicy:     nats.AckExplicitPolicy,
			AckWait:       c.ackWait,
			MaxDeliver:    c.maxDeliver,
			MaxAckPending: c.maxPending,
			ReplayPolicy:  nats.ReplayInstantPolicy,
		}
		// One filter is FilterSubject; several is FilterSubjects. Setting both,
		// or joining several into one string, produces a consumer that matches
		// nothing — see #109.
		if len(c.filters) == 1 {
			cfg.FilterSubject = c.filters[0]
		} else {
			cfg.FilterSubjects = c.filters
		}
		if _, err := js.AddConsumer(c.stream, cfg); err != nil {
			return fmt.Errorf("creating consumer %s/%s: %w", c.stream, c.name, err)
		}
	}
	return nil
}

// TestTheTestTopologyMatchesTheDeployedOne is the anti-drift gate.
//
// It parses the add_stream and add_consumer calls out of deploy/nats/init.sh
// and requires this file's tables to agree with them. Without it, every test in
// this package could pass against a topology that exists nowhere else.
func TestTheTestTopologyMatchesTheDeployedOne(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	declaredStreams, declaredConsumers, err := parseInitScript(filepath.Join(root, "deploy", "nats", "init.sh"))
	if err != nil {
		t.Fatalf("parsing init.sh: %v", err)
	}

	if len(declaredStreams) == 0 || len(declaredConsumers) == 0 {
		// The parser finding nothing would make every assertion below hold
		// vacuously — the exact shape of the M0 codegen drift gate.
		t.Fatalf("parsed %d streams and %d consumers from init.sh; the parser is broken",
			len(declaredStreams), len(declaredConsumers))
	}

	t.Run("streams", func(t *testing.T) {
		here := map[string]string{}
		for _, s := range streams {
			here[s.name] = strings.Join(s.subjects, ",")
		}
		compare(t, "stream", declaredStreams, here)
	})

	t.Run("consumers", func(t *testing.T) {
		here := map[string]string{}
		for _, c := range consumers {
			here[c.stream+"/"+c.name] = strings.Join(c.filters, ",")
		}
		compare(t, "consumer", declaredConsumers, here)
	})
}

func compare(t *testing.T, kind string, declared, here map[string]string) {
	t.Helper()
	for name, subjects := range declared {
		got, ok := here[name]
		if !ok {
			t.Errorf("%s %s is in deploy/nats/init.sh but not in this test's table", kind, name)
			continue
		}
		if got != subjects {
			t.Errorf("%s %s: init.sh declares %q, this test creates %q", kind, name, subjects, got)
		}
	}
	for name := range here {
		if _, ok := declared[name]; !ok {
			t.Errorf("%s %s is created by this test but declared nowhere in deploy/nats/init.sh", kind, name)
		}
	}
}

// parseInitScript reads the add_stream and add_consumer call sites.
//
// Parsing the shell rather than duplicating a list in a fixture: a fixture is
// one more copy to forget to update, and the call sites are the authority.
func parseInitScript(path string) (map[string]string, map[string]string, error) {
	file, err := os.Open(path) //nolint:gosec // a repo-relative path built by the test
	if err != nil {
		return nil, nil, err
	}
	defer file.Close() //nolint:errcheck // read-only

	streamsOut := map[string]string{}
	consumersOut := map[string]string{}

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := splitShellFields(line)
		switch {
		case len(fields) >= 3 && fields[0] == "add_stream":
			streamsOut[fields[1]] = fields[2]
		case len(fields) >= 4 && fields[0] == "add_consumer":
			// stream name filter ...
			consumersOut[fields[1]+"/"+fields[2]] = fields[3]
		}
	}
	return streamsOut, consumersOut, scanner.Err()
}

// splitShellFields splits on whitespace, honouring double quotes. The call
// sites quote their subject lists because those contain commas.
func splitShellFields(line string) []string {
	var fields []string
	var current strings.Builder
	inQuotes := false

	for _, r := range line {
		switch {
		case r == '"':
			inQuotes = !inQuotes
		case (r == ' ' || r == '\t') && !inQuotes:
			if current.Len() > 0 {
				fields = append(fields, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		fields = append(fields, current.String())
	}
	return fields
}

// TestEveryConsumerReceivesWhatItFiltersOn is the Go counterpart of
// scripts/nats-topology-check.sh, run against the container.
//
// Same reason it exists there: a consumer whose filter resolves to nothing is
// indistinguishable from a healthy one until you publish and wait.
func TestEveryConsumerReceivesWhatItFiltersOn(t *testing.T) {
	cases := []struct{ stream, consumer, subject string }{
		{"TASKING", "gateway-projector-tasking", "tasking.request.received.v1"},
		{"FEASIBILITY", "gateway-projector-feasibility", "feasibility.opportunities.computed.v1"},
		{"PLANNING", "gateway-projector-planning", "planning.plan.committed.v1"},
		{"PLANNING", "gateway-projector-planning", "acquisition.executed.v1"},
		{"TASKING", "feasibility-worker", "tasking.request.received.v1"},
		{"FEASIBILITY", "planner-opportunities", "feasibility.opportunities.computed.v1"},
		{"TASKING", "planner-lifecycle", "tasking.request.received.v1"},
		{"PLANNING", "simulator-executor", "planning.plan.committed.v1"},
	}

	for _, tc := range cases {
		t.Run(tc.stream+"/"+tc.consumer+"<-"+tc.subject, func(t *testing.T) {
			before := pending(t, tc.stream, tc.consumer)

			if _, err := env.js.Publish(tc.subject, []byte(`{"topology_check":true}`)); err != nil {
				t.Fatalf("publish: %v", err)
			}

			after := before
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				if after = pending(t, tc.stream, tc.consumer); after > before {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if after <= before {
				t.Fatalf("pending stayed at %d after publishing to %s; this filter matches nothing",
					before, tc.subject)
			}
		})
	}
}

func pending(t *testing.T, stream, consumer string) uint64 {
	t.Helper()
	info, err := env.js.ConsumerInfo(stream, consumer)
	if err != nil {
		t.Fatalf("consumer info %s/%s: %v", stream, consumer, err)
	}
	return info.NumPending
}
