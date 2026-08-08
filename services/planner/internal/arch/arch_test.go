// Package arch enforces the layering, because a convention decays and a test
// does not.
//
// What the layering buys is specific: the state machine and, later, the
// allocation logic are testable without Postgres or NATS, which is what makes
// property-based testing practical at all. The moment domain imports an
// adapter, that stops being true — and it stops being true silently, because
// the code still compiles and the tests still pass until the day someone needs
// to run the domain without a database.
package arch_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/mhayk/overpass/services/planner"

// The rule, stated once. Each layer may import only the layers listed.
var allowed = map[string][]string{
	"internal/domain": {},
	"internal/port":   {"internal/domain"},
	"internal/app":    {"internal/domain", "internal/port", "internal/allocation"},
	// allocation holds the AllocationPolicy implementations. It touches no
	// database, no broker and no HTTP, so it is not an adapter — it sits beside
	// app, importing only the domain it is a strategy over.
	//
	// It may NOT import port. A policy that could see a port could be handed
	// something that reads, and Allocate being a pure function is what makes
	// M2-12's property tests and M2-13's identical-inputs comparison possible.
	"internal/allocation": {"internal/domain"},
	// adapter may import anything inward; that is what an adapter is for.
	//
	// It includes internal/app because natsmsg binds against app.Streams — the
	// stream-to-consumer map. That is inward, so the rule permits it, and it
	// keeps the topology stated once instead of copied into the adapter where
	// it would drift from deploy/nats/init.sh.
	//
	// plan-gateway also lists internal/render here. The planner has no render
	// layer: it publishes events and commits plans, and turning either into
	// CZML is the gateway's job.
	"internal/adapter": {"internal/domain", "internal/port", "internal/app", "internal/allocation"},
}

func TestLayeringIsRespected(t *testing.T) {
	root := repoRelative(t, "..", "..")

	for layer, permitted := range allowed {
		t.Run(layer, func(t *testing.T) {
			imports := internalImportsUnder(t, filepath.Join(root, layer))
			for file, list := range imports {
				for _, imported := range list {
					if !isPermitted(imported, permitted) {
						t.Errorf(
							"%s imports %s\n  %s may import %v and nothing else internal\n"+
								"  the dependency arrow points inward; an adapter reaching into a\n"+
								"  layer that must stay pure is how the domain stops being testable\n"+
								"  without Postgres",
							rel(root, file), imported, layer, permitted,
						)
					}
				}
			}
		})
	}
}

// TestTheRuleWouldCatchAViolation is the guard on the guard.
//
// A checker with a bug in its path matching finds nothing and reports success,
// which is exactly what the M0 codegen drift gate did. This feeds it a
// deliberate violation and requires it to object.
func TestTheRuleWouldCatchAViolation(t *testing.T) {
	violation := modulePath + "/internal/adapter/postgres"
	if isPermitted(violation, allowed["internal/domain"]) {
		t.Fatal("domain importing an adapter was accepted; the check proves nothing")
	}
	if isPermitted(violation, allowed["internal/app"]) {
		t.Fatal("app importing an adapter was accepted; the check proves nothing")
	}
	if !isPermitted(modulePath+"/internal/domain", allowed["internal/app"]) {
		t.Fatal("app importing domain was rejected; the check is too strict to be useful")
	}
	// port must not reach outward either. The allocation logic that arrives
	// with M2-04 is testable without Postgres only for as long as this holds.
	if isPermitted(violation, allowed["internal/port"]) {
		t.Fatal("port importing an adapter was accepted; the check proves nothing")
	}
	if !isPermitted(modulePath+"/internal/app", allowed["internal/adapter"]) {
		t.Fatal("adapter importing app was rejected; natsmsg could not read app.Streams")
	}
	// A policy that could reach a port could be handed something that reads,
	// and Allocate being pure is what the property tests and the benchmark
	// both rest on.
	if isPermitted(modulePath+"/internal/port", allowed["internal/allocation"]) {
		t.Fatal("allocation importing port was accepted; a policy could then read something that differs between runs")
	}
	if isPermitted(violation, allowed["internal/allocation"]) {
		t.Fatal("allocation importing an adapter was accepted; the check proves nothing")
	}
}

// TestEveryLayerActuallyHasFiles stops the suite passing vacuously.
//
// If a directory were renamed, the walk would find nothing and every rule would
// hold over an empty set.
func TestEveryLayerActuallyHasFiles(t *testing.T) {
	root := repoRelative(t, "..", "..")
	for layer := range allowed {
		files := internalImportsUnder(t, filepath.Join(root, layer))
		if len(files) == 0 {
			t.Errorf("no Go files found under %s — the layering check is running over nothing", layer)
		}
	}
}

func isPermitted(imported string, permitted []string) bool {
	if !strings.HasPrefix(imported, modulePath+"/") {
		return true // third-party and stdlib are not this test's business
	}
	suffix := strings.TrimPrefix(imported, modulePath+"/")
	for _, p := range permitted {
		if suffix == p || strings.HasPrefix(suffix, p+"/") {
			return true
		}
	}
	return false
}

func internalImportsUnder(t *testing.T, dir string) map[string][]string {
	t.Helper()
	out := map[string][]string{}

	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		// Record the file even with no imports at all. domain/health.go has
		// none — that is the entire point of it — and keying the map only on
		// files that import something made the vacuity check below fail
		// against a correctly pure domain. Found by that check, which is what
		// it is for.
		out[path] = nil
		for _, spec := range parsed.Imports {
			value, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			out[path] = append(out[path], value)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return out
}

func repoRelative(t *testing.T, parts ...string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Join(append([]string{wd}, parts...)...)
}

func rel(root, path string) string {
	if r, err := filepath.Rel(root, path); err == nil {
		return r
	}
	return path
}
