package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BUG-2699 — the enforcement step for an invariant this package's own doc
// comments assert.
//
// publishWatchNotification's doc comment says the push handler is the ONLY
// direct Bus.Publish caller in this package, because it is the only site
// whose notification has no durable backing to fall back on. A sentence in
// a comment protects whoever reads that comment before adding a producer;
// it does nothing about the producer added by someone who didn't. So the
// split is checked here instead of asserted there.
//
// This is a SOURCE scan rather than a behavioural test on purpose: the
// thing being protected is a call-site population, and a population claim
// is checked by enumerating it. A new producer that calls
// s.watchEvents.Publish directly fails this test by name and has to make
// a deliberate choice — use the best-effort helper, or extend
// allowedDirectPublishFiles with a reason.
//
// WHAT IT STILL CANNOT SEE, said plainly rather than left to be
// discovered (codex round 7): a bus obtained indirectly — returned from a
// method, passed in as a parameter, reached through an interface value
// stored elsewhere. Catching those needs type information, not syntax.
// The structural alternative is stronger and is the right answer if this
// ever has to grow again: make the field unreachable outside the helper
// rather than detectable. Until then this covers the forms a producer
// would plausibly be written in, and it FAILS on them — which is more
// than the doc comment it replaced could do.
var allowedDirectPublishFiles = map[string]string{
	// The one caller that acts on the result: a push has no inbox, no
	// store row, nothing to read back, so a refused publish loses the
	// instruction outright and the caller has to be told.
	"handlers_push.go": "push has no durable backing; it maps the error onto the response",
	// The helper itself — where the best-effort discard is ruled, once,
	// for every other producer.
	"handlers_watch_notify.go": "publishWatchNotification: the single best-effort discard",
}

func TestWatchEventsPublishSitesAreRuled(t *testing.T) {
	t.Parallel()

	// AST, not a line grep (codex round 2, P2). The first version matched
	// the literal string `s.watchEvents.Publish(` on a single line, which a
	// producer could evade without trying: a local alias (`bus :=
	// s.watchEvents`), a selector split across lines by gofmt, a receiver
	// named anything else. A scanner that only catches the spelling it
	// expects reports a clean package while the invariant is broken — the
	// same class of false green this whole unit is about.
	//
	// Parsing resolves calls structurally: any call whose function is a
	// selector `.Publish` on an expression that mentions the watchEvents
	// field, however it is spelled or wrapped.
	// Files enumerated and parsed individually rather than with
	// parser.ParseDir, which is deprecated as of Go 1.25 (it ignores build
	// tags). Per-file parsing needs no extra dependency, and this package
	// has no build-tagged files for the tags to matter to.
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	found := map[string]int{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		{
			base := filepath.Base(name)
			file, err := parser.ParseFile(fset, name, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Publish" {
					return true
				}
				// The receiver expression mentions the watchEvents field —
				// `s.watchEvents`, `srv.watchEvents`, or anything that
				// selects it. An ALIAS assigned to a local variable is the
				// one shape this still cannot see; aliasDeclarations below
				// covers that separately.
				if mentionsWatchEventsField(sel.X) {
					found[base]++
				}
				return true
			})
			// A local alias defeats the structural check above, so the
			// alias itself is what gets flagged: any code that reads
			// s.watchEvents into a variable is treated as a publish site,
			// because the ruling is about which code may HOLD the bus, not
			// about how the call is spelled.
			//
			// Both binding forms, not just `:=` (codex round 7): a
			// `var bus = s.watchEvents` is a GenDecl/ValueSpec, not an
			// AssignStmt, and the first version of this check walked only
			// the latter.
			ast.Inspect(file, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.AssignStmt:
					for _, rhs := range node.Rhs {
						if isWatchEventsFieldSelector(rhs) {
							found[base]++
						}
					}
				case *ast.ValueSpec:
					for _, v := range node.Values {
						if isWatchEventsFieldSelector(v) {
							found[base]++
						}
					}
				}
				return true
			})
		}
	}

	// POSITIVE CONTROL. Without it, a scanner that matched nothing — wrong
	// directory, renamed method, a parse that quietly returned no files —
	// would report a clean bill of health for a package it never read.
	if found["handlers_push.go"] == 0 {
		t.Fatal("scanner found no direct Publish call in handlers_push.go — the scan itself is broken, not the invariant")
	}

	for file, count := range found {
		if _, ok := allowedDirectPublishFiles[file]; !ok {
			t.Errorf("%s reaches watchEvents.Publish directly (%d site(s)).\n"+
				"Producers layered on a committed store write must use s.publishWatchNotification, "+
				"which rules the best-effort discard in one place (BUG-2699).\n"+
				"If this site genuinely needs to act on the result, add it to allowedDirectPublishFiles with a reason.",
				file, count)
		}
	}
	for file, reason := range allowedDirectPublishFiles {
		if found[file] == 0 {
			t.Errorf("%s is listed as an allowed direct Publish caller (%q) but has none — "+
				"stale allow-list entry, remove it", file, reason)
		}
	}
}

// mentionsWatchEventsField reports whether expr reads the Server's
// watchEvents field anywhere inside it, so a wrapped or parenthesised
// receiver is still recognised.
func mentionsWatchEventsField(expr ast.Expr) bool {
	seen := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if isWatchEventsFieldSelector(n) {
			seen = true
			return false
		}
		return true
	})
	return seen
}

func isWatchEventsFieldSelector(n ast.Node) bool {
	sel, ok := n.(*ast.SelectorExpr)
	return ok && sel.Sel != nil && sel.Sel.Name == "watchEvents"
}
