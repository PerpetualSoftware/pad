package models_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// PLAN-3535's classification guard (the Go half; the web half lives in
// web/src). The behavioural fixture (server's
// TestPLAN3535_ReferenceItemsCountTowardNoOpenWork) proves every surface it
// lists leaves reference items out; this catches a NEW surface it does not
// list. It enumerates every call of a lifecycle helper, the functions that
// decide "is this item open / done / counted", and requires each call site to
// be classified below: "exclude: <how reference items are left out>" or
// "keep: <why this site counts every item>". A new site, or one that moved to
// another function, fails until someone decides which it is.
//
// DELIBERATELY NAME-BASED, keyed on file + enclosing function + helper, not an
// analysis of what each site counts: the reason is the thing review argues
// about, and a scanner that tried to judge it would invite the open-ended
// review loop a classification table avoids.
var lifecycleHelpers = map[string]bool{
	"IsTerminalItem":                true,
	"IsAbandonedItem":               true,
	"isItemDone":                    true,
	"childProgressState":            true,
	"buildChildrenDoneExpr":         true,
	"buildChildrenUncountedExpr":    true,
	"CollectionCompletedWorkValues": true,
	"nonTerminalFilter":             true,
	"doneFiltersForWorkspace":       true,
}

var lifecycleSites = map[string]string{
	// The helpers themselves, composed of each other.
	"internal/models/close_state.go::ItemCloseState::IsTerminalItem":             "keep: an item's own close state (open/done/abandoned), not a count",
	"internal/models/close_state.go::ItemCloseState::IsAbandonedItem":            "keep: an item's own close state, not a count",
	"internal/server/handlers_dashboard.go::isItemDone::IsTerminalItem":          "keep: the done predicate itself; each caller below is classified",
	"internal/server/handlers_dashboard.go::childProgressState::IsAbandonedItem": "exclude: the progress predicate; isReferenceCollection answers first",
	"internal/server/handlers_dashboard.go::childProgressState::IsTerminalItem":  "exclude: the progress predicate; isReferenceCollection answers first",
	"internal/store/items.go::nonTerminalFilter::doneFiltersForWorkspace":        "keep: the list filter for an item list's own open items (a doc's status filters its own list)",
	"internal/store/items.go::nonTerminalFilter::buildChildrenDoneExpr":          "keep: as above",
	"internal/store/items.go::listItemsQ::nonTerminalFilter":                     "keep: `pad item list` / MCP list's default open filter, a list's own items (ListItems delegates here; ListItemsQ refuses NonTerminal, BUG-3659)",
	"internal/store/items.go::listItemsFTS::nonTerminalFilter":                   "keep: the same filter on the search-backed list",

	// Progress.
	"internal/store/items.go::GetItemProgress::buildChildrenDoneExpr":                   "exclude: counted only where buildChildrenUncountedExpr is false",
	"internal/store/items.go::GetItemProgress::buildChildrenUncountedExpr":              "exclude: reference children are uncounted",
	"internal/store/items.go::GetAllItemProgress::buildChildrenDoneExpr":                "exclude: counted only where buildChildrenUncountedExpr is false",
	"internal/store/items.go::GetAllItemProgress::buildChildrenUncountedExpr":           "exclude: reference children are uncounted",
	"internal/server/handlers_items.go::collectionChildrenProgress::childProgressState": "exclude: childProgressState",
	"internal/server/handlers_items.go::handleGetItemProgress::childProgressState":      "exclude: childProgressState",
	"internal/server/handlers_dashboard.go::buildDashboardResponse::childProgressState": "exclude: active-plan progress, childProgressState",

	// Dashboard open-work sections.
	"internal/server/handlers_item_context.go::handleGetItemContext::isItemDone": "exclude: item-context conflicts (PLAN-3598) mirror the dashboard: the subject item checks isReferenceCollection first; a blocker keeps a reference BLOCKER, an explicit link; a closed parent of any collection is an anomaly (the close guard skips only reference children)",
	"internal/server/handlers_dashboard.go::buildDashboardResponse::isItemDone":  "exclude: overdue, blocked, suggested_next and by_role read workItems; orphaned_task checks isReferenceCollection; the blocker check keeps a reference BLOCKER, an explicit link",
	"internal/server/handlers_dashboard.go::firstActiveBlockers::isItemDone":     "keep: the done check on a BLOCKER, moved out of buildDashboardResponse (TASK-3615); a reference blocker is an explicit link and still blocks",
	"internal/server/handlers_catch_up.go::catchUpNeedsYou::isItemDone":          "exclude: needs_you's blocked items skip isReferenceCollection first, as the dashboard's blocked attention reads workItems (TASK-3615)",

	// Close guard.
	"internal/server/handlers_items_open_children_guard.go::runOpenChildrenGuard::isItemDone": "exclude: a reference child is skipped before the done check",

	// Role breakdown.
	"internal/store/agent_roles.go::GetRoleBreakdown::doneFiltersForWorkspace": "exclude: AND NOT buildReferenceExpr",
	"internal/store/agent_roles.go::GetRoleBreakdown::buildChildrenDoneExpr":   "exclude: AND NOT buildReferenceExpr",

	// Completed work (standup, changelog).
	"internal/server/handlers_project_intel.go::listTerminalItemsSince::CollectionCompletedWorkValues": "exclude: a reference collection contributes no values",
	"cmd/pad/cmd_project.go::listCompletedWorkSince::CollectionCompletedWorkValues":                    "exclude: a reference collection contributes no values (CLI twin)",

	// Decisions.
	"internal/decision/attention.go::attentionEligible::IsTerminalItem": "exclude: ineligible when !CollectionTracksWork",

	// Keep.
	"internal/server/handlers_graph.go::handleGetWorkspaceGraph::isItemDone":            "keep: the graph renders every item, done or not, and only colours by state",
	"internal/server/handlers_reminders.go::collectPendingRemindersBounded::isItemDone": "keep: a reminder is the user's own instruction, on any item",
	"internal/store/item_stars.go::isTerminalWithContext::IsTerminalItem":               "keep: starred items are the user's own picks",
	"internal/server/handlers_me_needs.go::needsForWorkspace::IsTerminalItem":           "keep: an @mention is a real ask of a person in any non-system collection, reference collections included (TASK-3920, lead ruling Q2); only a terminal item is dropped",
	"internal/server/needs_scorers.go::isOpen::IsTerminalItem":                          "keep: the importance scorer counts what an item BLOCKS over explicit blocks links, as the dashboard keeps a reference blocker; an explicit link names held-up work in any collection, and only a terminal item is left out (TASK-3959)",
}

func TestPLAN3535_LifecycleHelperCallSitesAreClassified(t *testing.T) {
	root := repoRoot(t)
	found := map[string]token.Position{}
	fset := token.NewFileSet()
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					name := ""
					switch fun := call.Fun.(type) {
					case *ast.Ident:
						name = fun.Name
					case *ast.SelectorExpr:
						name = fun.Sel.Name
					}
					if lifecycleHelpers[name] {
						key := rel + "::" + fn.Name.Name + "::" + name
						if _, seen := found[key]; !seen {
							found[key] = fset.Position(call.Pos())
						}
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	var unclassified, stale []string
	for key, pos := range found {
		reason, ok := lifecycleSites[key]
		if !ok {
			unclassified = append(unclassified, key+"  ("+filepath.Base(pos.Filename)+":"+itoa(pos.Line)+")")
			continue
		}
		if !strings.HasPrefix(reason, "exclude: ") && !strings.HasPrefix(reason, "keep: ") {
			t.Errorf("%s: a classification starts with \"exclude: \" or \"keep: \", got %q", key, reason)
		}
	}
	for key := range lifecycleSites {
		if _, ok := found[key]; !ok {
			stale = append(stale, key)
		}
	}
	sort.Strings(unclassified)
	sort.Strings(stale)
	for _, k := range unclassified {
		t.Errorf("unclassified lifecycle call (PLAN-3535): %s\n\tdoes this site count open work or progress? If so, leave reference items out (models.CollectionTracksWork) and classify it \"exclude: <how>\"; otherwise \"keep: <why>\"", k)
	}
	for _, k := range stale {
		t.Errorf("classified site no longer exists: %s (remove it, or reclassify where the call moved)", k)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
