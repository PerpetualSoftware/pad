package appstore

import (
	"strings"
	"testing"
)

// TestAppstoreBoundary: the real package reaches no store method outside the
// reviewed allow-list, no raw database handle, no interface a store type
// answers, no reflection, and no ordinary attachments Put (SPEC-6 §4).
func TestAppstoreBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("loads the module from source")
	}
	for _, vs := range checkBoundary(t, ".", ".") {
		for _, v := range vs {
			t.Error(v)
		}
	}
}

// Each fixture breaks exactly one rule; the checker must report it. These are
// the checker's red-first evidence: a rule that misses its fixture is a rule
// that would miss the real defect.
func TestAppstoreBoundaryCatchesEachRule(t *testing.T) {
	if testing.Short() {
		t.Skip("loads the module from source")
	}
	fixtures := map[string]string{
		"humanmutation":  "store-method",
		"rawdb":          "raw-db",
		"rawsql":         "raw-sql",
		"ifacecall":      "interface-call",
		"ifacedecl":      "interface-decl",
		"reflectuse":     "reflect-unsafe",
		"attachmentsput": "attachments-put",
		"transitive":     "store-method",
	}
	var roots []string
	for fixture := range fixtures {
		roots = append(roots, "./testdata/"+fixture)
	}
	results := checkBoundary(t, ".", roots...)
	for fixture, rule := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			vs := results["github.com/PerpetualSoftware/pad/internal/appstore/testdata/"+fixture]
			for _, v := range vs {
				if v.rule == rule {
					return
				}
			}
			var got []string
			for _, v := range vs {
				got = append(got, v.String())
			}
			t.Fatalf("rule %s not reported; got:\n%s", rule, strings.Join(got, "\n"))
		})
	}
}
