package server

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// BUG-3033, CONVE-35 — the ENUMERATION guard.
//
// Three review rounds of this unit each found another door of the same class,
// and one of them had its precedent already in the tree. A loop that keeps
// finding new members is measuring the enumeration, not the code, so this test
// is the table: every site that turns an item body into text under another name
// is listed here with why it is safe, and anything NOT listed fails.
//
// What it can and cannot see, stated because a guard whose boundary is unstated
// gets trusted past it:
//
//   - It scans for the three DERIVATION HELPERS this codebase actually uses to
//     make body-derived text (snippetAround, PlaybookSummary, contentPreview).
//     A door built on a NEW helper is invisible to it — which is why the failure
//     message asks the reader to add the helper, not merely the site.
//   - It does NOT prove a listed site carries the marker. That is each door's
//     own both-directions test. This guard answers a different question: has a
//     site appeared that nobody has ruled on.
//   - It fails CLOSED. An unlisted site is a failure even if it is fine, since
//     the cost of that is one line in a table and the cost of the opposite is
//     another review round finding door twelve.
func TestBodyDerivedTextSitesAreAllRuledOn(t *testing.T) {
	// The helpers whose output is item-body text under another name. Adding one
	// here is how this guard learns about a new shape.
	helpers := []string{"snippetAround(", "PlaybookSummary(", "contentPreview("}

	// site -> the exact call lines ruled on there, and why they are safe.
	//
	// The lines are part of the key, not decoration. An earlier draft keyed on
	// file+helper alone and a negative control caught the hole: a SECOND call
	// site added to an already-listed file passed silently — and
	// handlers_bootstrap.go, which is listed, is exactly where a new bootstrap
	// projection would be written. Matching the lines with multiplicity means a
	// new site, a removed site, or a changed expression all fail.
	type ruling struct {
		lines  []string
		reason string
	}
	allowed := map[string]ruling{
		"internal/store/wiki_links.go:snippetAround(": {
			lines: []string{
				"snippet := snippetAround(content, position)",
				"snippet := snippetAround(content, position)",
			},
			reason: "backlink snippets (both query sites) — models.Backlink.ContentState carries the marker, set only when a snippet was produced",
		},
		"internal/cli/item_summary.go:contentPreview(": {
			lines:  []string{"ContentPreview:  contentPreview(item.Content),"},
			reason: "list previews — cli.ItemSummary.ContentState carries the marker (BUG-3000)",
		},
		"internal/server/handlers_bootstrap.go:PlaybookSummary(": {
			lines:  []string{"summary := collections.PlaybookSummary(it.Content)"},
			reason: "playbook summaries — AgentBootstrapPlaybookMeta.ContentState carries the marker, set only when a summary was produced",
		},
		"internal/server/handlers_playbook_library.go:PlaybookSummary(": {
			lines:  []string{"pb.Summary = collections.PlaybookSummary(pb.Content)"},
			reason: "LIBRARY playbooks are static Go-defined entries, not item rows: no op-log can be ahead of them, so there is nothing to mark",
		},
	}

	roots := []string{"..", "../../cmd"}
	found := map[string][]string{}
	// Files actually visited per root. A walk root that silently contributes
	// nothing (a wrong relative path, a SkipDir that swallowed it) would make
	// every door under it invisible while the guard stayed green — the failure
	// mode where silence and success look identical.
	visited := map[string]int{}
	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				// Vendored and generated trees are not ours to rule on.
				if base := info.Name(); base == "node_modules" || base == "vendor" || base == ".git" || base == "build" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			visited[root]++
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()

			rel := repoRelative(t, path)
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
			for sc.Scan() {
				line := sc.Text()
				trimmed := strings.TrimSpace(line)
				// A comment MENTIONING a helper is not a call site. Without this
				// the guard trips on its own explanatory prose — the comment
				// blindness that has bitten a source scanner in this repo before.
				if strings.HasPrefix(trimmed, "//") {
					continue
				}
				// The DEFINITION of a helper is not a use of it.
				if strings.HasPrefix(trimmed, "func ") {
					continue
				}
				for _, h := range helpers {
					if strings.Contains(line, h) {
						key := rel + ":" + h
						found[key] = append(found[key], strings.TrimSpace(line))
					}
				}
			}
			return sc.Err()
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	// POSITIVE CONTROLS, both of them, because this guard's failure mode is
	// silence and silence reads as a clean tree.
	//
	// (1) every walk root really produced files. This is what an external
	// mutation cannot easily prove: a control that adds a call site under cmd/
	// has to compile to run at all, and no derivation helper is importable
	// there, so the coverage of that root is asserted structurally instead.
	for _, root := range roots {
		if visited[root] == 0 {
			t.Fatalf("walk root %q contributed no .go files; every door under it is invisible "+
				"to this guard and it would still be green", root)
		}
	}
	// (2) the scan found call sites at all.
	if len(found) == 0 {
		t.Fatal("the scan found no body-derivation call sites anywhere; it is broken, " +
			"and a broken scan looks exactly like a clean tree")
	}
	// Every allow-list entry must still exist, or the table is describing a tree
	// that no longer exists and would silently stop guarding a renamed site.
	for site := range allowed {
		if _, ok := found[site]; !ok {
			t.Errorf("allow-list entry %q matches nothing any more — the site moved or was renamed; "+
				"re-rule it rather than leaving a stale exemption", site)
		}
	}

	var problems []string
	for site, lines := range found {
		rule, ok := allowed[site]
		if !ok {
			problems = append(problems, fmt.Sprintf("UNRULED %s\n      %s", site, strings.Join(lines, "\n      ")))
			continue
		}
		got := append([]string(nil), lines...)
		want := append([]string(nil), rule.lines...)
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			problems = append(problems, fmt.Sprintf(
				"CHANGED %s\n      ruled on (%d):\n        %s\n      found now (%d):\n        %s\n      reason on file: %s",
				site, len(want), strings.Join(want, "\n        "), len(got), strings.Join(got, "\n        "), rule.reason))
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("body-derived text is produced at %d site(s) that do not match what was ruled on (BUG-3033):\n    %s\n\n"+
			"Each turns an item's body into text under another name, so a stale body makes it stale. For an UNRULED "+
			"or CHANGED site: either carry content_state there (set only when the text was actually produced) or "+
			"update this test's table with the new line and the reason it needs no marker. If the new site uses a "+
			"derivation helper this guard does not know, add the helper too — the helper list is the grammar this "+
			"test can see, and a door built outside it is invisible.",
			len(problems), strings.Join(problems, "\n    "))
	}
}

// repoRelative turns a walk path into a repo-relative key, so the allow-list
// reads the same regardless of which walk root produced the entry. Anchored on
// the directory holding go.mod rather than on a hardcoded prefix, since the
// checkout's own name differs per worktree.
func repoRelative(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("abs %s: %v", path, err)
	}
	rel, err := filepath.Rel(repoRoot(t), abs)
	if err != nil {
		t.Fatalf("rel %s: %v", path, err)
	}
	return filepath.ToSlash(rel)
}

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
