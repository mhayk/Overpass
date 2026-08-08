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

const modulePath = "github.com/mhayk/overpass/services/plan-gateway"

// The rule, stated once. Each layer may import only the layers listed.
var allowed = map[string][]string{
	"internal/domain": {},
	"internal/port":   {"internal/domain"},
	"internal/app":    {"internal/domain", "internal/port"},
	// render turns port types into CZML and GeoJSON. It touches no database, no
	// broker and no HTTP, so it is not an adapter — it sits beside app rather
	// than inside adapter, and this test is what established that.
	//
	// It was written as internal/adapter/geo first and the layering check
	// objected immediately: an adapter importing a sibling adapter is exactly
	// the coupling the rule exists to prevent. The honest fix was to admit the
	// package had never been an adapter, not to widen the rule to let it stay.
	"internal/render": {"internal/domain", "internal/port"},
	// adapter may import anything inward; that is what an adapter is for.
	"internal/adapter": {"internal/domain", "internal/port", "internal/app", "internal/render"},
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
	// render is allowed FROM adapter and must not reach back. Renderers that
	// know about Postgres stop being testable without one, which is the whole
	// reason the golden files are cheap to run.
	if isPermitted(violation, allowed["internal/render"]) {
		t.Fatal("render importing an adapter was accepted; the check proves nothing")
	}
	if !isPermitted(modulePath+"/internal/render", allowed["internal/adapter"]) {
		t.Fatal("adapter importing render was rejected; the rendering layer would be unreachable")
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
