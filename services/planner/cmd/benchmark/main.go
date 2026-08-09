// Command benchmark runs the four allocation policies over identical generated
// scenarios and writes docs/policy-benchmark.md.
//
// This is the chart that makes the Strategy pattern worth having: it turns
// "which algorithm?" from an argument into a measurement, and it is the
// evidence ADR-0007 is waiting on to leave `proposed`.
//
// Everything is seeded. Identical inputs across policies or the comparison is
// meaningless; a fixed seed or the report changes under nobody's feet. The one
// thing that legitimately varies between runs is RUNTIME, which depends on the
// machine — the report says whose numbers it carries.
package main

import (
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mhayk/overpass/services/planner/internal/allocation"
	"github.com/mhayk/overpass/services/planner/internal/domain"
)

func main() {
	out := flag.String("out", "docs/policy-benchmark.md", "where to write the report")
	flag.Parse()

	report := Run(defaults())
	if err := os.WriteFile(*out, []byte(report), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "writing %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", *out)
}

// The scenario grid. Two levels on each axis the issue names: contention,
// deadline pressure, geographic clustering. Eight classes, every policy over
// every instance of every class.
type scenarioClass struct {
	Name       string
	Contended  bool // budget ~30% of demand, or ~200%
	TightDates bool // deadline slack 0–2 min, or 1–4 h
	Clustered  bool // rolls within ±5°, or spread ±45°
}

func classes() []scenarioClass {
	var out []scenarioClass
	for _, contended := range []bool{false, true} {
		for _, tight := range []bool{false, true} {
			for _, clustered := range []bool{false, true} {
				name := map[bool]string{true: "contended", false: "uncontended"}[contended] +
					"/" + map[bool]string{true: "tight-deadlines", false: "loose-deadlines"}[tight] +
					"/" + map[bool]string{true: "clustered", false: "dispersed"}[clustered]
				out = append(out, scenarioClass{name, contended, tight, clustered})
			}
		}
	}
	return out
}

// config sizes the run. The committed report uses Defaults; the harness's own
// tests use a fast shape, because a test that takes minutes is a test that
// stops being run.
type config struct {
	Seed         uint64
	Instances    int // per class
	SmallSize    int // inside ExactDP's limit, so instances have a true optimum
	ExactTimeout time.Duration
	ScalingSizes []int
}

func defaults() config {
	return config{
		Seed:         20260809, // fixed; the report is reproducible bar runtimes
		Instances:    12,
		SmallSize:    11,
		ExactTimeout: 5 * time.Second,
		ScalingSizes: []int{100, 1000, 5000},
	}
}

var bucketStart = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

func newRng(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, 1)) }

func generate(rng *rand.Rand, class scenarioClass, size int) domain.Problem {
	problem := domain.Problem{
		Key:       domain.RoundKey{SatelliteID: "BENCH-1", BucketStart: bucketStart},
		BucketEnd: bucketStart.Add(3 * time.Hour),
		Now:       bucketStart,
		Profile: domain.SatelliteProfile{
			Agility: domain.Agility{
				SlewRateDegS: 1.0, SettleTimeS: 5.0, ModeTransitionS: 0, MaxRollDeg: 45,
			},
		},
	}

	var totalDemand float64
	orbit := 47110
	for i := range size {
		duration := 20 + rng.Float64()*100
		roll := rng.Float64()*90 - 45
		if class.Clustered {
			roll = rng.NormFloat64() * 5
			roll = math.Max(-44, math.Min(44, roll))
		}
		open := time.Duration(rng.Int64N(int64(150 * time.Minute)))
		slack := time.Duration(rng.Int64N(int64(20 * time.Minute)))

		deadlineSlop := time.Duration(60+rng.Int64N(180)) * time.Minute
		if class.TightDates {
			deadlineSlop = time.Duration(rng.Int64N(int64(2 * time.Minute)))
		}

		cost := duration * (1 + rng.Float64()*0.5)
		totalDemand += cost

		problem.Candidates = append(problem.Candidates, domain.ScoredCandidate{
			Candidate: domain.Candidate{
				OpportunityID:        fmt.Sprintf("o%04d", i),
				RequestID:            fmt.Sprintf("r%04d", i),
				SatelliteID:          "BENCH-1",
				Mode:                 "STRIPMAP",
				AccessStart:          bucketStart.Add(open),
				AccessEnd:            bucketStart.Add(open + slack),
				AcquisitionDurationS: duration,
				OrbitNumber:          &orbit,
				DutyCycleCostS:       cost,
				QualityScore:         0.5 + rng.Float64()*0.5,
				GeometryJSON:         []byte(`{}`),
				FootprintGeoJSON:     []byte(`{}`),
			},
			CustomerID:     "bench",
			EffectiveValue: 50 + rng.Float64()*900,
			Deadline: bucketStart.Add(open).
				Add(time.Duration(duration) * time.Second).
				Add(deadlineSlop),
			Attitude: domain.Attitude{RollDeg: roll, Mode: "STRIPMAP"},
		})
	}

	if class.Contended {
		problem.Profile.DutyCycleBudgetS = totalDemand * 0.30
	} else {
		problem.Profile.DutyCycleBudgetS = totalDemand * 2.0
	}
	return problem
}

type measurement struct {
	Value     int64
	Fulfilled int
	Requests  int
	Utilised  float64
	Runtime   time.Duration
	Ratio     float64 // against the exact optimum; NaN when unsolved
	HasRatio  bool
}

type aggregate struct {
	measurements []measurement
}

func (a *aggregate) add(m measurement) { a.measurements = append(a.measurements, m) }

func (a *aggregate) meanRatio() (float64, int) {
	total, n := 0.0, 0
	for _, m := range a.measurements {
		if m.HasRatio {
			total += m.Ratio
			n++
		}
	}
	if n == 0 {
		return math.NaN(), 0
	}
	return total / float64(n), n
}

func (a *aggregate) meanRuntime() time.Duration {
	var total time.Duration
	for _, m := range a.measurements {
		total += m.Runtime
	}
	if len(a.measurements) == 0 {
		return 0
	}
	return total / time.Duration(len(a.measurements))
}

func (a *aggregate) meanValue() float64 {
	total := 0.0
	for _, m := range a.measurements {
		total += float64(m.Value)
	}
	return total / float64(len(a.measurements))
}

func (a *aggregate) meanFulfilled() float64 {
	total := 0.0
	for _, m := range a.measurements {
		total += float64(m.Fulfilled) / float64(m.Requests)
	}
	return 100 * total / float64(len(a.measurements))
}

func (a *aggregate) meanUtilisation() float64 {
	total := 0.0
	for _, m := range a.measurements {
		total += m.Utilised
	}
	return 100 * total / float64(len(a.measurements))
}

func utilisation(p domain.Problem, plan domain.Plan) float64 {
	var used float64
	for _, a := range plan.Acquisitions {
		used += a.DutyCycleCostS
	}
	if p.Profile.DutyCycleBudgetS == 0 {
		return 0
	}
	return math.Min(1, used/p.Profile.DutyCycleBudgetS)
}

// Run produces the whole report.
func Run(cfg config) string {
	heuristics := []domain.AllocationPolicy{
		allocation.GreedyByBid{},
		allocation.GreedyByValueDensity{},
		allocation.VickreySealedBid{},
	}
	exact := allocation.ExactDP{MaxCandidates: cfg.SmallSize + 1, Timeout: cfg.ExactTimeout}

	// class -> policy -> aggregate
	results := map[string]map[string]*aggregate{}
	exactRuntime := map[string]*aggregate{}
	solved := map[string]int{}

	rng := rand.New(rand.NewPCG(cfg.Seed, 1))
	for _, class := range classes() {
		results[class.Name] = map[string]*aggregate{}
		exactRuntime[class.Name] = &aggregate{}
		for _, policy := range heuristics {
			results[class.Name][policy.Name()] = &aggregate{}
		}

		for range cfg.Instances {
			problem := generate(rng, class, cfg.SmallSize)
			requests := len(problem.Candidates)

			started := time.Now()
			optimal, report := exact.Solve(problem)
			exactElapsed := time.Since(started)
			exactRuntime[class.Name].add(measurement{
				Value: optimal.Value(), Fulfilled: len(optimal.Acquisitions),
				Requests: requests, Utilised: utilisation(problem, optimal),
				Runtime: exactElapsed,
			})
			if report.Optimal {
				solved[class.Name]++
			}

			for _, policy := range heuristics {
				begun := time.Now()
				plan := policy.Allocate(problem)
				elapsed := time.Since(begun)

				m := measurement{
					Value: plan.Value(), Fulfilled: len(plan.Acquisitions),
					Requests: requests, Utilised: utilisation(problem, plan),
					Runtime: elapsed,
				}
				// Optimality ratio ONLY on instances the exact solver proved.
				// The harness says which those were rather than quietly
				// excluding the rest.
				if report.Optimal && optimal.Value() > 0 {
					m.Ratio = float64(plan.Value()) / float64(optimal.Value())
					m.HasRatio = true
				}
				results[class.Name][policy.Name()].add(m)
			}
		}
	}

	scaling := measureScaling(cfg)

	return render(cfg, results, exactRuntime, solved, scaling)
}

type scalingRow struct {
	Size    int
	Runtime map[string]time.Duration
	Value   map[string]int64
}

// measureScaling runs the heuristics alone at sizes the exact solver cannot
// reach, up to the contract's 5 000-opportunity cap. Runtime is the story here:
// the p95 budget for a round is 800 ms.
func measureScaling(cfg config) []scalingRow {
	heuristics := []domain.AllocationPolicy{
		allocation.GreedyByBid{},
		allocation.GreedyByValueDensity{},
		allocation.VickreySealedBid{},
	}
	class := scenarioClass{Name: "contended/loose-deadlines/dispersed", Contended: true}

	var rows []scalingRow
	for _, size := range cfg.ScalingSizes {
		rng := rand.New(rand.NewPCG(cfg.Seed, uint64(size)))
		problem := generate(rng, class, size)
		row := scalingRow{Size: size, Runtime: map[string]time.Duration{}, Value: map[string]int64{}}
		for _, policy := range heuristics {
			started := time.Now()
			plan := policy.Allocate(problem)
			row.Runtime[policy.Name()] = time.Since(started)
			row.Value[policy.Name()] = plan.Value()
		}
		rows = append(rows, row)
	}
	return rows
}

func render(
	cfg config,
	results map[string]map[string]*aggregate,
	exactRuntime map[string]*aggregate,
	solved map[string]int,
	scaling []scalingRow,
) string {
	var b strings.Builder
	policies := []string{"GREEDY_BY_BID", "GREEDY_BY_VALUE_DENSITY", "VICKREY_SEALED_BID"}

	fmt.Fprintf(&b, `# Policy benchmark

Four allocation policies over identical generated scenarios, seeded with %d so
the numbers are reproducible — bar the runtimes, which belong to whatever
machine ran %s. Regenerate with:

    make benchmark

Every optimality ratio is against a PROVEN optimum from ExactDP; instances the
exact solver could not finish carry no ratio, and the solved count per class
says how many that was rather than quietly excluding them. %d instances per
class, %d candidates each — inside the exact solver's limit deliberately.

## Optimality ratio by scenario class

How much of the optimal plan value each heuristic captured, averaged per class.

`, cfg.Seed, "`make benchmark`", cfg.Instances, cfg.SmallSize)

	// The mermaid chart: mean ratio per class, one bar series per policy.
	fmt.Fprintf(&b, "```mermaid\nxychart-beta\n    title \"Mean optimality ratio (%% of ExactDP)\"\n    x-axis [")
	names := sortedClasses(results)
	short := map[string]string{}
	for i, name := range names {
		short[name] = fmt.Sprintf("C%d", i+1)
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", short[name])
	}
	b.WriteString("]\n    y-axis \"ratio %\" 0 --> 100\n")
	for _, policy := range policies {
		b.WriteString("    bar [")
		for i, name := range names {
			if i > 0 {
				b.WriteString(", ")
			}
			ratio, _ := results[name][policy].meanRatio()
			fmt.Fprintf(&b, "%.1f", 100*ratio)
		}
		b.WriteString("]\n")
	}
	b.WriteString("```\n\n")

	b.WriteString("Series order: GREEDY_BY_BID, GREEDY_BY_VALUE_DENSITY, VICKREY_SEALED_BID.\n\nClass key:\n\n")
	for _, name := range names {
		fmt.Fprintf(&b, "- **%s** — %s\n", short[name], name)
	}

	b.WriteString("\n## Full results\n")
	for _, name := range names {
		fmt.Fprintf(&b, "\n### %s\n\n", name)
		fmt.Fprintf(&b, "%d of %d instances solved to proven optimality.\n\n", solved[name], cfg.Instances)
		b.WriteString("| policy | optimality | plan value | fulfilled | utilisation | runtime |\n")
		b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
		exactAggregate := exactRuntime[name]
		fmt.Fprintf(&b, "| EXACT_DP (reference) | 100%% | %.0f | %.0f%% | %.0f%% | %s |\n",
			exactAggregate.meanValue(), exactAggregate.meanFulfilled(),
			exactAggregate.meanUtilisation(), roundDuration(exactAggregate.meanRuntime()))
		for _, policy := range policies {
			a := results[name][policy]
			ratio, n := a.meanRatio()
			ratioCell := "—"
			if n > 0 {
				ratioCell = fmt.Sprintf("%.1f%%", 100*ratio)
			}
			fmt.Fprintf(&b, "| %s | %s | %.0f | %.0f%% | %.0f%% | %s |\n",
				policy, ratioCell, a.meanValue(), a.meanFulfilled(),
				a.meanUtilisation(), roundDuration(a.meanRuntime()))
		}
	}

	// Worst class per heuristic: the number that matters more than any
	// flattering average.
	b.WriteString("\n## Where each heuristic is worst\n\n")
	b.WriteString("| policy | worst class | ratio there |\n| --- | --- | --- |\n")
	for _, policy := range policies {
		worstName, worstRatio := "", math.Inf(1)
		for _, name := range names {
			if ratio, n := results[name][policy].meanRatio(); n > 0 && ratio < worstRatio {
				worstName, worstRatio = name, ratio
			}
		}
		fmt.Fprintf(&b, "| %s | %s | %.1f%% |\n", policy, worstName, 100*worstRatio)
	}

	b.WriteString(`
## Runtime scaling

Heuristics only — the exact solver refuses instances this size, loudly, which
is its job. One contended instance per size; the round's p95 budget is 800 ms.

| candidates | GREEDY_BY_BID | GREEDY_BY_VALUE_DENSITY | VICKREY_SEALED_BID |
| --- | --- | --- | --- |
`)
	for _, row := range scaling {
		fmt.Fprintf(&b, "| %d | %s | %s | %s |\n", row.Size,
			roundDuration(row.Runtime["GREEDY_BY_BID"]),
			roundDuration(row.Runtime["GREEDY_BY_VALUE_DENSITY"]),
			roundDuration(row.Runtime["VICKREY_SEALED_BID"]))
	}

	fmt.Fprintf(&b, "\nPlan value at %d candidates: ", scaling[len(scaling)-1].Size)
	last := scaling[len(scaling)-1]
	fmt.Fprintf(&b, "GREEDY_BY_BID %d, GREEDY_BY_VALUE_DENSITY %d, VICKREY_SEALED_BID %d.\n",
		last.Value["GREEDY_BY_BID"], last.Value["GREEDY_BY_VALUE_DENSITY"], last.Value["VICKREY_SEALED_BID"])

	return b.String()
}

func sortedClasses(results map[string]map[string]*aggregate) []string {
	var names []string
	for name := range results {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func roundDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000)
	default:
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
}
