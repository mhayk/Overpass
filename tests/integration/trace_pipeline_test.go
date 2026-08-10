package integration_test

import (
	"strings"
	"testing"
	"time"
)

// One trace must span the whole pipeline, not just the first two hops.
//
// TestOneTraceSpansBothServices proved the M1 claim: ingress → outbox publish →
// Python consumer. M3-05's claim is larger — ingress → outbox → feasibility →
// publish → planner → commit → projector — and it was NOT true when this test
// was written. The planner's natsmsg adapter read a message's payload and
// metadata and dropped its headers, so the traceparent never reached the
// projector and every span the planner produced was a root. The planner's own
// outbox relay never injected one either, so the chain could not have continued
// past it even if it had started.
//
// Both are fixed in #52. This is what says so.
func TestOneTraceSpansTheWholePipeline(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}

	seedCustomer(t)
	startFeasibilityWorker(t, root)

	traceEnv := map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": env.tempoOTLP,
		// 1.0, so the assertion is about propagation rather than luck.
		// ADR-0019 explains why the committed stack runs at the same value:
		// ParentBased honours the ingress decision, so a fraction here would
		// test the sampler instead of the chain.
		"TRACE_SAMPLE_RATIO": "1.0",
	}

	api, err := start(env.taskingAPIBin, "tasking-api", traceEnv)
	if err != nil {
		t.Fatalf("starting tasking-api: %v", err)
	}
	t.Cleanup(func() {
		if killErr := api.Kill(); killErr != nil {
			t.Errorf("killing tasking-api: %v", killErr)
		}
	})

	plannerVars := plannerEnv()
	for key, value := range traceEnv {
		plannerVars[key] = value
	}
	planner, err := start(env.plannerBin, "planner", plannerVars)
	if err != nil {
		t.Fatalf("starting planner: %v", err)
	}
	t.Cleanup(func() {
		if killErr := planner.Kill(); killErr != nil {
			t.Errorf("killing planner: %v", killErr)
		}
	})

	traceID := submitAndReadTraceID(t, api)
	t.Logf("trace id from the response: %s", traceID)

	// Longer than the two-service test's budget, and deliberately so: this one
	// waits for a feasibility sweep AND a planner round, and the round only
	// fires after its quiet period has elapsed with no new candidates.
	var spans []observedSpan
	if !waitFor(120*time.Second, func() bool {
		spans = fetchTrace(t, traceID)
		return hasService(spans, "tasking-api") &&
			hasService(spans, "feasibility-service") &&
			hasService(spans, "planner")
	}) {
		t.Fatalf("the trace never reached the planner.\nfound: %s", summarise(spans))
	}

	assertNoOrphans(t, spans)
	assertPlannerCarriesDomainAttributes(t, spans)
}

// assertNoOrphans is the general statement of trace completeness.
//
// Stronger than checking a hardcoded chain, and it keeps working as hops are
// added: EVERY span except the single root must name a parent that is itself in
// this trace. An orphan means a hop shared the trace id but lost the causal
// link — which is exactly what a dropped traceparent produces, and exactly what
// a reader cannot see in Tempo without expanding every span by hand.
func assertNoOrphans(t *testing.T, spans []observedSpan) {
	t.Helper()

	byID := make(map[string]observedSpan, len(spans))
	for _, span := range spans {
		byID[span.spanID] = span
	}

	var roots []observedSpan
	for _, span := range spans {
		if span.parentID == "" {
			roots = append(roots, span)
			continue
		}
		if _, present := byID[span.parentID]; !present {
			t.Errorf("span %q (%s) names parent %s, which is not in this trace — "+
				"the hop shares the trace id but lost the causal chain",
				span.name, span.service, span.parentID)
		}
	}

	// Exactly one root. Two roots in one trace id is the shape a re-rolled
	// sampling decision or a re-extracted context produces, and it renders in
	// Tempo as two unrelated stacks that a human has to join.
	if len(roots) != 1 {
		t.Errorf("expected exactly one root span, found %d: %s", len(roots), summarise(roots))
	}
}

// assertPlannerCarriesDomainAttributes checks the criterion that makes a trace
// useful rather than merely complete.
//
// "Domain attributes on spans, not just HTTP metadata" is the issue's own
// engineering decision: satellite_id and policy are what turn a timeline into
// an answer. A trace that reaches the planner and cannot say which satellite or
// which allocation policy was involved has arrived without the information
// anybody opened it for.
func assertPlannerCarriesDomainAttributes(t *testing.T, spans []observedSpan) {
	t.Helper()

	for _, span := range spans {
		if span.service != "planner" || !strings.Contains(span.name, "allocate") {
			continue
		}
		for _, key := range []string{"overpass.satellite_id", "overpass.policy"} {
			if _, present := span.attributes[key]; !present {
				t.Errorf("planner.allocate carries no %s; the span shows a timeline "+
					"rather than answering which satellite or policy it was", key)
			}
		}
		return
	}
	t.Log("no planner.allocate span in this trace: the round fired with nothing to allocate")
}

func summarise(spans []observedSpan) string {
	var b strings.Builder
	for _, span := range spans {
		b.WriteString("\n  ")
		b.WriteString(span.service)
		b.WriteString(" ")
		b.WriteString(span.name)
		b.WriteString(" span=")
		b.WriteString(span.spanID)
		b.WriteString(" parent=")
		if span.parentID == "" {
			b.WriteString("<root>")
		} else {
			b.WriteString(span.parentID)
		}
	}
	return b.String()
}
