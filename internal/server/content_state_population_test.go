package server

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// BUG-3033, CONVE-35 — the ENUMERATION guard.
//
// Three review rounds of this unit each found another door of the same class,
// and one had its precedent already in the tree. A loop that keeps finding new
// members is measuring the enumeration, not the code, so this test IS the
// table: every site that turns an item body into text under another name is
// listed with why it is safe, and anything else fails.
//
// It parses Go rather than grepping. The grep draft passed four controls and
// codex round 3 then walked through it in four ways — a one-line function, a
// helper assigned to a local and called through it, a comment inside the call
// (`PlaybookSummary /* x */ (body)`), and an argument changed below the opening
// line of a multiline call — while failing on harmless prose that merely
// mentioned a helper. Both directions wrong is the signature of a scanner that
// is not reading the language it claims to be reading.
//
// What it sees and does not, stated because a guard whose boundary is unstated
// gets trusted past it:
//
//   - CALL EXPRESSIONS whose callee names one of the derivation helpers, by
//     bare identifier or through a package selector, anywhere in a non-test .go
//     file under internal/ or cmd/. Position, comments and line breaks inside
//     the call are irrelevant to the AST, which is the point.
//   - A helper NAME used as a value rather than called — assigned to a
//     variable, passed as an argument — is reported too, and fails closed: the
//     call through that alias is invisible, so the reference itself is the last
//     honest place to stop.
//   - It does NOT prove a listed site carries the marker. That is each door's
//     own both-directions test. This answers a different question: has a site
//     appeared that nobody has ruled on.
//   - It cannot see a door built on a helper it does not know, or one that
//     inlines the derivation without any helper at all. The helper list is the
//     grammar it reads; the failure message therefore asks for the helper too.
//   - It scans GO only. Browser consumers of the same fields are a separate
//     surface with their own item (BUG-3050): the marker reaches them on the
//     wire and the TypeScript types declare it, but where a stale badge belongs
//     in a panel or a command palette is a UI decision, not a field copy.
func TestBodyDerivedTextSitesAreAllRuledOn(t *testing.T) {
	// The helpers whose output is item-body text under another name. Adding one
	// here is how this guard learns about a new shape.
	helpers := map[string]bool{
		"snippetAround":   true,
		"PlaybookSummary": true,
		"contentPreview":  true,
		"FTSSnippet":      true,
	}

	type ruling struct {
		// sites are "file:line-ish" free of line numbers on purpose: a key that
		// moved with an unrelated edit above it would be re-ruled every time.
		// The COUNT is the load-bearing half — an added call in an already
		// listed file is the hole the grep draft had.
		count  int
		reason string
	}
	allowed := map[string]ruling{
		"internal/store/wiki_links.go:snippetAround": {
			count:  2,
			reason: "backlink snippets (both query sites) — models.Backlink.ContentState carries the marker, set only when a snippet was produced",
		},
		"internal/cli/item_summary.go:contentPreview": {
			count:  1,
			reason: "list previews — cli.ItemSummary.ContentState carries the marker (BUG-3000)",
		},
		"internal/server/handlers_bootstrap.go:PlaybookSummary": {
			count:  1,
			reason: "playbook summaries — AgentBootstrapPlaybookMeta.ContentState carries the marker, set only when a summary was produced",
		},
		"internal/server/handlers_playbook_library.go:PlaybookSummary": {
			count:  1,
			reason: "LIBRARY playbooks are static Go-defined entries, not item rows: no op-log can be ahead of them, so there is nothing to mark",
		},
		"internal/store/search.go:FTSSnippet": {
			count:  2,
			reason: "FTS search snippets — the body is dropped from the result but the snippet is cut from it, so models.Item.ContentState survives on that path deliberately (and is cleared on the title-only direct-ref paths beside it)",
		},
		"internal/store/items.go:FTSSnippet": {
			count:  2,
			reason: "item-list FTS snippets — same path and same reasoning as search.go's, over the shared item queries that already splice contentStateSQL",
		},
	}

	root := repoRoot(t)
	found := map[string]int{}
	where := map[string][]string{}
	visited := map[string]int{}

	for _, dir := range []string{"internal", "cmd"} {
		base := filepath.Join(root, dir)
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				if b := info.Name(); b == "node_modules" || b == "vendor" || b == ".git" || b == "build" || b == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			visited[dir]++

			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				// A file this guard cannot parse is a file it cannot rule on.
				return fmt.Errorf("parse %s: %w", path, err)
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)

			// nameOf reports the helper a callee or value expression names.
			nameOf := func(e ast.Expr) string {
				switch v := e.(type) {
				case *ast.Ident:
					if helpers[v.Name] {
						return v.Name
					}
				case *ast.SelectorExpr:
					if helpers[v.Sel.Name] {
						return v.Sel.Name
					}
				}
				return ""
			}

			// Positions that are NOT a value-use of a helper: the callee of a
			// call, and any DECLARATION of the name (a func, or an interface
			// method / struct field spec). Collected first so the value-use arm
			// below cannot fire on a call's own callee — which it did on its
			// first run, reporting every legitimate call twice.
			notAValueUse := map[token.Pos]bool{}
			ast.Inspect(file, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.CallExpr:
					switch fn := v.Fun.(type) {
					case *ast.Ident:
						notAValueUse[fn.Pos()] = true
					case *ast.SelectorExpr:
						notAValueUse[fn.Sel.Pos()] = true
					}
				case *ast.FuncDecl:
					notAValueUse[v.Name.Pos()] = true
				case *ast.Field:
					for _, nm := range v.Names {
						notAValueUse[nm.Pos()] = true
					}
				}
				return true
			})

			ast.Inspect(file, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.FuncDecl:
					// The DEFINITION of a helper is not a use of it. Its body is
					// still walked — a helper calling another helper counts.
					if helpers[v.Name.Name] {
						if v.Body != nil {
							ast.Inspect(v.Body, func(inner ast.Node) bool {
								if c, ok := inner.(*ast.CallExpr); ok {
									if name := nameOf(c.Fun); name != "" {
										key := rel + ":" + name
										found[key]++
										where[key] = append(where[key], fmt.Sprintf("%s:%d", rel, fset.Position(c.Lparen).Line))
									}
								}
								return true
							})
						}
						return false
					}
				case *ast.CallExpr:
					if name := nameOf(v.Fun); name != "" {
						key := rel + ":" + name
						found[key]++
						where[key] = append(where[key], fmt.Sprintf("%s:%d", rel, fset.Position(v.Lparen).Line))
					}
				case *ast.Ident:
					// A helper NAME that is not the callee of a call — assigned
					// to a variable, passed along — makes the eventual call
					// invisible to this guard. Fail closed on the reference.
					if helpers[v.Name] && !notAValueUse[v.Pos()] {
						key := rel + ":" + v.Name + " (used as a value, not called)"
						found[key]++
						where[key] = append(where[key], fmt.Sprintf("%s:%d", rel, fset.Position(v.Pos()).Line))
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}

	// POSITIVE CONTROLS. This guard's failure mode is silence, and silence reads
	// as a clean tree.
	for _, dir := range []string{"internal", "cmd"} {
		if visited[dir] == 0 {
			t.Fatalf("walk root %q contributed no .go files; every door under it is invisible "+
				"to this guard and it would still be green", dir)
		}
	}
	if len(found) == 0 {
		t.Fatal("the scan found no body-derivation call sites anywhere; it is broken, " +
			"and a broken scan looks exactly like a clean tree")
	}

	var problems []string
	for site, n := range found {
		rule, ok := allowed[site]
		if !ok {
			problems = append(problems, fmt.Sprintf("UNRULED %s (%d)\n      %s",
				site, n, strings.Join(where[site], "\n      ")))
			continue
		}
		if n != rule.count {
			problems = append(problems, fmt.Sprintf("COUNT CHANGED %s: ruled on %d, found %d\n      %s\n      reason on file: %s",
				site, rule.count, n, strings.Join(where[site], "\n      "), rule.reason))
		}
	}
	for site := range allowed {
		if _, ok := found[site]; !ok {
			problems = append(problems, fmt.Sprintf("STALE RULING %s matches nothing any more — the site moved or was renamed; re-rule it rather than leaving an exemption that guards nothing", site))
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("body-derived text is produced at %d site(s) that do not match what was ruled on (BUG-3033):\n    %s\n\n"+
			"Each turns an item's body into text under another name, so a stale body makes it stale. Either carry "+
			"content_state there (set only when the text was actually produced) or update this test's table with "+
			"the new count and the reason it needs no marker. If the new site uses a derivation helper this guard "+
			"does not know, add the helper too — the helper list is the grammar this test reads, and a door built "+
			"outside it is invisible.",
			len(problems), strings.Join(problems, "\n    "))
	}
}

// repoRoot finds the directory holding go.mod, so the table's keys read the same
// regardless of the checkout's own name (which differs per worktree).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory; the scan cannot anchor its paths")
		}
		dir = parent
	}
}
