package main

import (
	"strings"
	"testing"
	"time"
)

// fast is the harness at test size: one instance per class, tiny scaling, tight
// timeout. The committed report uses defaults(); a test that takes minutes is a
// test that stops being run.
func fast() config {
	return config{
		Seed:         42,
		Instances:    1,
		SmallSize:    6,
		ExactTimeout: 2 * time.Second,
		ScalingSizes: []int{30},
	}
}

func TestTheGridCoversAllEightClasses(t *testing.T) {
	all := classes()
	if len(all) != 8 {
		t.Fatalf("%d classes, want 2x2x2 = 8", len(all))
	}
	seen := map[string]bool{}
	for _, c := range all {
		if seen[c.Name] {
			t.Errorf("class %s appears twice", c.Name)
		}
		seen[c.Name] = true
	}
}

// IDENTICAL INPUTS ACROSS POLICIES, FIXED SEED — or the comparison is
// meaningless. Two generations from the same seed must agree byte for byte.
func TestGenerationIsDeterministic(t *testing.T) {
	build := func() string {
		var b strings.Builder
		for _, class := range classes() {
			p := generate(newRng(fast().Seed), class, 9)
			for _, c := range p.Candidates {
				b.WriteString(c.OpportunityID)
				b.WriteString(c.AccessStart.String())
			}
		}
		return b.String()
	}
	first, second := build(), build()
	if first != second {
		t.Fatal("the same seed generated different scenarios; every comparison in the report is noise")
	}
}

func TestContentionActuallyTightensTheBudget(t *testing.T) {
	loose := generate(newRng(7), scenarioClass{Contended: false}, 9)
	tight := generate(newRng(7), scenarioClass{Contended: true}, 9)
	if tight.Profile.DutyCycleBudgetS >= loose.Profile.DutyCycleBudgetS {
		t.Fatalf("contended budget %v is not tighter than uncontended %v",
			tight.Profile.DutyCycleBudgetS, loose.Profile.DutyCycleBudgetS)
	}
}

// The whole harness, at test size. The report must carry every section the
// issue names, and its seeded content must be stable across runs.
func TestRunProducesTheFullReport(t *testing.T) {
	report := Run(fast())

	for _, section := range []string{
		"## Optimality ratio by scenario class",
		"```mermaid",
		"## Full results",
		"## Where each heuristic is worst",
		"## Runtime scaling",
		"EXACT_DP (reference)",
		"GREEDY_BY_BID",
		"GREEDY_BY_VALUE_DENSITY",
		"VICKREY_SEALED_BID",
	} {
		if !strings.Contains(report, section) {
			t.Errorf("the report has no %q", section)
		}
	}

	// The deterministic parts must not drift between runs. Runtimes vary by
	// machine, so compare only the lines without a duration in them.
	stable := func(r string) string {
		var keep []string
		for _, line := range strings.Split(r, "\n") {
			if strings.Contains(line, "µs") || strings.Contains(line, "ms") || strings.Contains(line, "s |") {
				continue
			}
			keep = append(keep, line)
		}
		return strings.Join(keep, "\n")
	}
	if stable(report) != stable(Run(fast())) {
		t.Fatal("two runs from one seed disagree outside the runtime columns")
	}
}

func TestAggregateMathIsHonest(t *testing.T) {
	a := &aggregate{}
	a.add(measurement{Value: 100, Fulfilled: 1, Requests: 2, Utilised: 0.5, Ratio: 0.8, HasRatio: true})
	a.add(measurement{Value: 300, Fulfilled: 2, Requests: 2, Utilised: 1.0}) // unsolved: no ratio

	ratio, n := a.meanRatio()
	if n != 1 || ratio != 0.8 {
		t.Errorf("meanRatio = %v over %d; an unsolved instance leaked into the ratio", ratio, n)
	}
	if got := a.meanValue(); got != 200 {
		t.Errorf("meanValue = %v", got)
	}
	if got := a.meanFulfilled(); got != 75 {
		t.Errorf("meanFulfilled = %v%%, want 75", got)
	}
}

func TestDurationsRenderAtHumanScale(t *testing.T) {
	cases := map[time.Duration]string{
		250 * time.Microsecond:  "250µs",
		3500 * time.Microsecond: "3.5ms",
		2500 * time.Millisecond: "2.50s",
	}
	for d, want := range cases {
		if got := roundDuration(d); got != want {
			t.Errorf("roundDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
