package mcp

// Catalog ↔ HTTP-route parity (BUG-2304).
//
// The registry advertises every catalog action on both transports, but
// the HTTP transport only serves cmdKeys wired into routeTable, the
// dispatcher's specialRoutes map, or itemLinkSpecs. Nothing enforced
// that the two sets stay in sync — which is exactly how `item
// backlinks`, `item history`, and `project report` shipped advertised
// but returning "not yet implemented over HTTP transport" on Pad
// Cloud. The only prior coverage was a hand-picked 12-entry want-list
// (TestRouteTable_ContainsExpectedCommands), which a silently-missing
// route passes.
//
// This test closes the hole structurally: it drives EVERY catalog
// action through its real ActionFn with a fixture input rich enough to
// reach dispatch, captures the cmdPath the action actually emits (the
// same capture technique as TestReadOnlyCatalog_ActionsDispatchExpectedCmdPath),
// and asserts each captured cmdKey is covered by one of the three HTTP
// route surfaces. An action that never reaches dispatch fails too —
// otherwise a fixture gap would silently shrink coverage back to a
// hand-list.
//
// Instrument scope: this checks route MEMBERSHIP, not route BEHAVIOR.
// A mapper with a wrong path or method would pass here — behavioral
// shape belongs to the per-route tests (TestRoute_*, the dispatch_*
// real-server tests), which every new route entry should bring along.

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// parityFixtureInput satisfies every required positional across the
// catalog so each ActionFn reaches env.Dispatch instead of erroring on
// missing input. Extra keys are harmless — BuildCLIArgs ignores
// anything not in the command's declared args/flags.
func parityFixtureInput() map[string]any {
	return map[string]any{
		"email":             "test@example.com",
		"name":              "test-name",
		"slug":              "test-slug",
		"query":             "test-query",
		"ref":               "TASK-1",
		"refs":              []any{"TASK-1", "TASK-2"},
		"collection":        "tasks",
		"title":             "test title",
		"target_collection": "ideas",
		"message":           "test message",
		"summary":           "test summary",
		"decision":          "test decision",
		"code":              "123456",
		"attachment_id":     "att-1",
		"artifact":          "---\ntitle: t\n---\nbody",
		"url":               "https://example.test/hook",
		"status":            "open",
	}
}

func TestHTTPTransport_EveryCatalogActionRouted(t *testing.T) {
	t.Parallel()
	doc := liveCmdhelpDoc(t)
	special := (&HTTPHandlerDispatcher{}).specialRoutes()

	covered := func(cmdKey string) bool {
		if _, ok := routeTable[cmdKey]; ok {
			return true
		}
		if _, ok := special[cmdKey]; ok {
			return true
		}
		if _, ok := itemLinkSpecs[cmdKey]; ok {
			return true
		}
		return false
	}

	var missing []string     // dispatched a cmdKey with no HTTP route
	var unexercised []string // never reached dispatch — fixture gap

	drive := func(t *testing.T, label string, handler ActionFn, input map[string]any) {
		disp := &fakeDispatcher{}
		env := ActionEnv{
			Doc:        doc,
			Workspace:  NewWorkspaceState("docapp"),
			Dispatcher: disp,
		}
		// Handler errors are irrelevant here — only whether a cmdPath
		// was emitted, and which. fakeDispatcher returns success for
		// anything it receives.
		_, _ = handler(context.Background(), input, env)
		if len(disp.gotPath) == 0 {
			unexercised = append(unexercised, label)
			return
		}
		cmdKey := strings.Join(disp.gotPath, " ")
		if !covered(cmdKey) {
			missing = append(missing, label+" → "+cmdKey)
		}
	}

	for _, def := range Catalog {
		if def.Name == "pad_meta" {
			continue // inline-handled; never dispatches a cmdPath
		}
		for actionName, handler := range def.Actions {
			label := def.Name + "." + actionName

			// link/unlink fan out to a different cmdPath per
			// link_type — drive every route in the table so each
			// underlying command is checked, not just one.
			if def.Name == "pad_item" && (actionName == "link" || actionName == "unlink") {
				for linkType := range itemLinkRoutes {
					input := parityFixtureInput()
					input["link_type"] = linkType
					input["target"] = "TASK-2"
					drive(t, label+"("+linkType+")", handler, input)
				}
				continue
			}

			drive(t, label, handler, parityFixtureInput())
		}
	}

	sort.Strings(missing)
	sort.Strings(unexercised)
	if len(missing) > 0 {
		t.Errorf("catalog actions advertised but NOT routed over the HTTP transport "+
			"(add a routeTable entry, a specialRoutes method, or an itemLinkSpecs entry "+
			"in internal/mcp/dispatch_http*.go):\n  %s", strings.Join(missing, "\n  "))
	}
	if len(unexercised) > 0 {
		t.Errorf("catalog actions never reached dispatch — parityFixtureInput is missing "+
			"a required input, so parity coverage silently shrank:\n  %s", strings.Join(unexercised, "\n  "))
	}
}
