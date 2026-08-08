package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The claim #32 exists to make: ONE trace spans an HTTP ingress in Go, an
// outbox publish, and a Python consumer.
//
// It cannot be shown any other way. Both services already emit spans and both
// export them successfully whether or not propagation works — a broken chain
// produces a forest of one-span traces, every one of which looks healthy in
// isolation. The only evidence is a single trace id with both services' spans
// under it, which is what this asserts.

// tempoTrace is the subset of Tempo's response this test reads.
type tempoTrace struct {
	Batches []struct {
		Resource struct {
			Attributes []tempoAttribute `json:"attributes"`
		} `json:"resource"`
		ScopeSpans []struct {
			Spans []struct {
				TraceID      string           `json:"traceId"`
				SpanID       string           `json:"spanId"`
				ParentSpanID string           `json:"parentSpanId"`
				Name         string           `json:"name"`
				Kind         string           `json:"kind"`
				Links        []map[string]any `json:"links"`
				Attributes   []tempoAttribute `json:"attributes"`
			} `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"batches"`
}

type tempoAttribute struct {
	Key   string `json:"key"`
	Value struct {
		StringValue string `json:"stringValue"`
	} `json:"value"`
}

type observedSpan struct {
	service  string
	name     string
	kind     string
	spanID   string
	parentID string
	links    int
}

// startFeasibilityWorker runs the Python consumer as a real process.
//
// uv, not a hand-built virtualenv: the service's dependencies are locked in
// uv.lock and reproducing that resolution by hand is how a test ends up
// exercising a different set of libraries than production.
func startFeasibilityWorker(t *testing.T, root string) {
	t.Helper()
	if _, err := exec.LookPath("uv"); err != nil {
		// Skipping is fine on a laptop without uv. In CI it is not: a skipped
		// assertion is a green tick for work nobody did, which is the exact
		// failure this test exists to prevent. GitHub sets CI=true.
		if os.Getenv("CI") != "" {
			t.Fatal("uv is not installed, so the Python half of the trace cannot run — " +
				"this assertion must never silently skip in CI")
		}
		t.Skip("uv is not installed; the Python half of the trace cannot run")
	}

	// Resolve dependencies FIRST, with a budget of its own.
	//
	// `uv run` syncs on demand and prints nothing while it does. The first
	// version of this test skipped straight to starting the worker and spent
	// twenty minutes downloading numpy, skyfield and pyproj behind a 45-second
	// readiness wait — so the failure looked like a worker that would not start
	// rather than a test measuring dependency resolution.
	syncCtx, cancelSync := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancelSync()
	sync := exec.CommandContext(syncCtx, "uv", "sync", "--frozen")
	sync.Dir = filepath.Join(root, "services", "feasibility")
	if output, err := sync.CombinedOutput(); err != nil {
		t.Fatalf("uv sync: %v\n%s", err, output)
	}

	// The venv's OWN interpreter, not `uv run`.
	//
	// `uv run` spawns python as a CHILD, so cmd.Process.Kill() kills uv and
	// leaves the worker alive — still bound to the durable `feasibility-worker`
	// consumer and still draining it. That was survivable while exactly one test
	// started a worker. It stopped being survivable when a second one did:
	// TestEveryConsumerReceivesWhatItFiltersOn publishes a probe message and
	// requires the consumer's pending count to RISE, and an orphaned worker eats
	// the probe before it can be counted. Diagnosed from that failure, in CI.
	//
	// `uv sync --frozen` above has already built the venv, so executing its
	// interpreter directly is the same environment with one fewer process
	// between the test and the thing it is trying to stop.
	//nolint:noctx // Killed explicitly below; a context would reap it politely
	// and this test needs it to stop when told, not when the framework decides.
	cmd := exec.Command(venvPython(t, root), "-m", "feasibility")
	cmd.Dir = filepath.Join(root, "services", "feasibility")
	cmd.Env = append(os.Environ(),
		"NATS_URL="+env.natsURL,
		"DATABASE_URL="+env.dsn,
		"OTEL_EXPORTER_OTLP_ENDPOINT="+env.tempoOTLP,
		"TRACE_SAMPLE_RATIO=1.0",
		"LOG_LEVEL=INFO",
	)
	logs := &syncBuffer{}
	cmd.Stdout = logs
	cmd.Stderr = logs

	// WaitDelay, or teardown hangs and the worker log is never printed.
	//
	// `uv run` spawns python as a CHILD, so killing uv leaves python alive
	// holding the stdout pipe it inherited — and cmd.Wait() blocks reading that
	// pipe until the whole test times out. Two runs of this test died that way,
	// and because Wait() never returned the t.Logf below never ran: the bug
	// suppressed the one piece of evidence needed to diagnose anything.
	//
	// WaitDelay bounds it without process-group handling, which would be
	// platform-specific for no gain here.
	cmd.WaitDelay = 5 * time.Second

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the feasibility worker: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill() //nolint:errcheck // best effort teardown
			_ = cmd.Wait()         //nolint:errcheck // killed on purpose
		}
		t.Logf("feasibility worker log:\n%s", logs.String())
	})

	// The worker binds a pre-declared durable consumer and exits immediately if
	// it is missing. Waiting for the log line rather than sleeping means a
	// startup failure is reported as itself instead of as a missing trace.
	if !waitFor(45*time.Second, func() bool {
		return strings.Contains(logs.String(), "feasibility worker starting")
	}) {
		t.Fatalf("the feasibility worker never started:\n%s", logs.String())
	}
}

const traceCustomerID = "acme-imaging"

// seedCustomer satisfies the foreign key on tasking_requests.
//
// The reference tables are populated by #31's seed, which does not exist yet,
// so a submission against an empty database fails with
// tasking_requests_customer_id_fkey and a 503 that reads as a database outage.
// One row here rather than waiting: the trace assertion is about propagation,
// not about seeding, and the constraint doing its job is correct behaviour.
// venvPython resolves the interpreter `uv sync` just built.
//
// Both layouts, because the path differs by platform and a test that only works
// on the CI runner is a test nobody can reproduce locally.
func venvPython(t *testing.T, root string) string {
	t.Helper()
	venv := filepath.Join(root, "services", "feasibility", ".venv")
	for _, candidate := range []string{
		filepath.Join(venv, "bin", "python"),
		filepath.Join(venv, "Scripts", "python.exe"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	t.Fatalf("no interpreter in %s; did `uv sync --frozen` run?", venv)
	return ""
}

func seedCustomer(t *testing.T) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO reference.customers (customer_id, display_name)
		VALUES ($1, 'Trace Probe Customer')
		ON CONFLICT (customer_id) DO NOTHING
	`, traceCustomerID); err != nil {
		t.Fatalf("seeding the customer: %v", err)
	}
}

func TestOneTraceSpansBothServices(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}

	seedCustomer(t)
	startFeasibilityWorker(t, root)

	api, err := start(env.taskingAPIBin, "tasking-api", map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": env.tempoOTLP,
		"TRACE_SAMPLE_RATIO":          "1.0",
	})
	if err != nil {
		t.Fatalf("starting tasking-api: %v", err)
	}
	t.Cleanup(func() {
		if killErr := api.Kill(); killErr != nil {
			t.Errorf("killing tasking-api: %v", killErr)
		}
	})

	traceID := submitAndReadTraceID(t, api)
	t.Logf("trace id from the response: %s", traceID)

	// Both services batch spans, and Tempo has an idle period before a trace is
	// queryable. Polling rather than sleeping so a fast machine is fast.
	var spans []observedSpan
	if !waitFor(60*time.Second, func() bool {
		spans = fetchTrace(t, traceID)
		return hasService(spans, "tasking-api") && hasService(spans, "feasibility-service")
	}) {
		t.Fatalf("the trace never contained both services.\nfound: %+v", spans)
	}

	assertTheChain(t, spans)
}

// submitAndReadTraceID posts a request and returns the trace it created.
//
// The traceparent is SENT rather than read back, so the test knows the id
// without depending on the service echoing one. That also exercises the path a
// real caller uses: a client that already has a trace expects its request to
// join it rather than start a new one.
func submitAndReadTraceID(t *testing.T, api *service) string {
	t.Helper()

	traceID := strings.ReplaceAll(uuid.NewString(), "-", "")
	rootSpan := strings.ReplaceAll(uuid.NewString(), "-", "")[:16]

	body := map[string]any{
		"customer_id": traceCustomerID,
		// Required by the API even though the event schema makes it optional:
		// the ingress asks for a human label so a request is identifiable in the
		// UI, and the event only carries one if it was given.
		"target_name": "trace probe",
		"target":      map[string]any{"type": "Point", "coordinates": []float64{4.4, 51.9}},
		"window": map[string]string{
			"start": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
			"end":   time.Now().UTC().Add(25 * time.Hour).Format(time.RFC3339),
		},
		"priority_tier":   "BEST_EFFORT",
		"bid_credits":     0,
		"requested_modes": []string{"SCAN"},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}

	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, api.baseURL+"/v1/tasking-requests", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", uuid.NewString())
	// Sampled flag set, so ParentBased keeps the whole trace.
	req.Header.Set("traceparent", fmt.Sprintf("00-%s-%s-01", traceID, rootSpan))

	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("submitting: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // status is what matters

	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusCreated {
		var detail bytes.Buffer
		_, _ = detail.ReadFrom(resp.Body) //nolint:errcheck // diagnostics only
		t.Fatalf("submit returned %d: %s\n%s", resp.StatusCode, detail.String(), api.logs.String())
	}
	return traceID
}

func fetchTrace(t *testing.T, traceID string) []observedSpan {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodGet, env.tempoQuery+"/api/traces/"+traceID, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil // Tempo not answering yet
	}
	defer resp.Body.Close() //nolint:errcheck // polled

	if resp.StatusCode != http.StatusOK {
		return nil // 404 until the trace is flushed and indexed
	}

	var payload tempoTrace
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil
	}

	var out []observedSpan
	for _, batch := range payload.Batches {
		service := ""
		for _, attribute := range batch.Resource.Attributes {
			if attribute.Key == "service.name" {
				service = attribute.Value.StringValue
			}
		}
		for _, scope := range batch.ScopeSpans {
			for _, span := range scope.Spans {
				out = append(out, observedSpan{
					service:  service,
					name:     span.Name,
					kind:     span.Kind,
					spanID:   span.SpanID,
					parentID: span.ParentSpanID,
					links:    len(span.Links),
				})
			}
		}
	}
	return out
}

func hasService(spans []observedSpan, name string) bool {
	for _, span := range spans {
		if span.service == name {
			return true
		}
	}
	return false
}

// assertTheChain checks the SHAPE, not just co-membership.
//
// Both services having spans in one trace is necessary and not sufficient: two
// roots sharing a trace id would satisfy it and would still mean the causal
// chain is broken. What has to hold is that the consumer names a parent, and
// that the parent is a span in the same trace rather than a dangling id.
func assertTheChain(t *testing.T, spans []observedSpan) {
	t.Helper()

	byID := make(map[string]observedSpan, len(spans))
	for _, span := range spans {
		byID[span.spanID] = span
	}

	var producer, consumer *observedSpan
	for i := range spans {
		switch {
		case spans[i].service == "tasking-api" && strings.Contains(spans[i].name, "outbox"):
			producer = &spans[i]
		case spans[i].service == "feasibility-service":
			consumer = &spans[i]
		}
	}

	if producer == nil {
		t.Fatalf("no outbox publish span from tasking-api; the relay is not tracing.\n%+v", spans)
	}
	if consumer == nil {
		t.Fatalf("no span from feasibility-service.\n%+v", spans)
	}

	if consumer.parentID == "" {
		t.Error("the consumer span is a root: it shares the trace id but not the causal chain")
	}
	if _, present := byID[consumer.parentID]; !present {
		t.Errorf("the consumer names parent %s, which is not a span in this trace",
			consumer.parentID)
	}
	if consumer.parentID != producer.spanID {
		t.Errorf("the consumer's parent is %s, want the publish span %s — "+
			"parenting anywhere else hides the queue wait",
			consumer.parentID, producer.spanID)
	}
	if consumer.links == 0 {
		t.Error("the consumer span carries no link; nothing marks this as a " +
			"message-driven continuation rather than a nested call")
	}
	if consumer.kind != "SPAN_KIND_CONSUMER" {
		t.Errorf("consumer span kind = %q, want SPAN_KIND_CONSUMER", consumer.kind)
	}
	if producer.parentID == "" {
		t.Error("the publish span is a root: it is not attached to the request that caused it")
	}

	t.Logf("chain: ingress -> %s (%s) -> %s (%s)",
		producer.name, producer.spanID, consumer.name, consumer.spanID)
}
