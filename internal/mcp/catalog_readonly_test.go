package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
)

// TestReadOnlyCatalog_AllToolsRegistered locks the v0.2 read-only
// surface. Catches drift in BOTH directions — drops (expected tool
// missing) and unexpected adds (a new tool registered without
// updating this test).
func TestReadOnlyCatalog_AllToolsRegistered(t *testing.T) {
	want := map[string]bool{
		// pad_meta is from TASK-979; the rest are TASK-980.
		"pad_meta":       false,
		"pad_workspace":  false,
		"pad_collection": false,
		"pad_project":    false,
		"pad_role":       false,
		"pad_search":     false,
	}
	for _, def := range Catalog {
		if _, ok := want[def.Name]; ok {
			want[def.Name] = true
			continue
		}
		// Unexpected tool — surface it loudly. If the addition is
		// intentional (TASK-981 will add pad_item), bump this test's
		// `want` map in the same commit.
		t.Errorf("unexpected catalog entry %q — update want{} in this test if intentional",
			def.Name)
	}
	for name, found := range want {
		if !found {
			t.Errorf("expected catalog entry %q not registered", name)
		}
	}
	if len(Catalog) != len(want) {
		t.Errorf("Catalog has %d tools, expected exactly %d", len(Catalog), len(want))
	}
}

// TestReadOnlyCatalog_ActionsMatchCmdhelp guards against cmdhelp drift.
// Three-way validation:
//
//  1. Every entry in `expected` must point at a cmdPath that exists in
//     cmdhelp (catches typos in our own table).
//  2. Every action in every catalog tool (except pad_meta, which
//     handles inline) must have an entry in `expected` (catches new
//     actions added without test coverage).
//  3. Every entry in `expected` must correspond to an actual catalog
//     action (catches table entries that outlive the action they
//     describe).
//
// pad_meta is excluded because its actions are inline server-info /
// version / tool-surface — they don't dispatch to a CLI cmdPath.
//
// pad_item is excluded for now — TASK-981 adds it; the same test
// gets extended there with the expected entries.
func TestReadOnlyCatalog_ActionsMatchCmdhelp(t *testing.T) {
	doc := liveCmdhelpDoc(t)

	// Hand-curated map of (toolName, action) → expected cmdPath. Custom
	// handlers (workspace audit-log) appear here too — they dispatch
	// to the listed cmdPath even though the input shape differs.
	expected := map[[2]string][]string{
		{"pad_workspace", "list"}:      {"workspace", "list"},
		{"pad_workspace", "members"}:   {"workspace", "members"},
		{"pad_workspace", "invite"}:    {"workspace", "invite"},
		{"pad_workspace", "storage"}:   {"workspace", "storage"},
		{"pad_workspace", "audit-log"}: {"workspace", "audit-log"},

		{"pad_collection", "list"}:   {"collection", "list"},
		{"pad_collection", "create"}: {"collection", "create"},

		{"pad_project", "dashboard"}: {"project", "dashboard"},
		{"pad_project", "next"}:      {"project", "next"},
		{"pad_project", "standup"}:   {"project", "standup"},
		{"pad_project", "changelog"}: {"project", "changelog"},

		{"pad_role", "list"}:   {"role", "list"},
		{"pad_role", "create"}: {"role", "create"},
		{"pad_role", "delete"}: {"role", "delete"},

		{"pad_search", "query"}: {"item", "search"},
	}

	// Direction 1: every expected cmdPath resolves in cmdhelp.
	for key, cmdPath := range expected {
		joined := strings.Join(cmdPath, " ")
		if _, ok := doc.Commands[joined]; !ok {
			t.Errorf("expected[%s.%s] = %q is not in cmdhelp",
				key[0], key[1], joined)
		}
	}

	// Tools that handle their actions inline (no CLI dispatch) are
	// skipped — they're correct as-is and don't need expected entries.
	skipTools := map[string]bool{
		"pad_meta": true,
	}

	// Direction 2 + 3: catalog ⇄ expected bijection (modulo skipTools).
	// Every catalog action (in non-skipped tools) needs a cmdPath
	// entry; every entry needs a corresponding catalog action.
	catalogActions := map[[2]string]bool{}
	for _, def := range Catalog {
		if skipTools[def.Name] {
			continue
		}
		for actionName := range def.Actions {
			catalogActions[[2]string{def.Name, actionName}] = true
		}
	}
	for key := range catalogActions {
		if _, ok := expected[key]; !ok {
			t.Errorf("catalog has %s.%s but no expected cmdPath entry — add one to expected{}",
				key[0], key[1])
		}
	}
	for key := range expected {
		if !catalogActions[key] {
			t.Errorf("expected entry for %s.%s but no such action in catalog",
				key[0], key[1])
		}
	}
}

// TestReadOnlyCatalog_ActionsDispatchExpectedCmdPath actually invokes
// every catalog action through a fake dispatcher and asserts the
// captured cmdPath matches the expected table from
// TestReadOnlyCatalog_ActionsMatchCmdhelp. Closes the catalog → dispatch
// drift hole — without this, a typo like
// passThrough([]string{"some", "other"}) on pad_search.query would
// pass the bijection check (the action name still matches) but
// silently dispatch to the wrong command.
//
// The fixtureInput map covers every required positional arg across
// the read-only surface so the action handlers all reach Dispatch
// rather than erroring on missing input. Extra keys are harmless —
// BuildCLIArgs ignores anything that isn't in cmdInfo.Flags or
// cmdInfo.Args.
func TestReadOnlyCatalog_ActionsDispatchExpectedCmdPath(t *testing.T) {
	doc := liveCmdhelpDoc(t)
	// Mirror of expected{} in the bijection test, kept literal so a
	// renamed cmdPath fails THIS test loudly with the actual
	// dispatched path printed in the error message.
	expected := map[[2]string][]string{
		{"pad_workspace", "list"}:      {"workspace", "list"},
		{"pad_workspace", "members"}:   {"workspace", "members"},
		{"pad_workspace", "invite"}:    {"workspace", "invite"},
		{"pad_workspace", "storage"}:   {"workspace", "storage"},
		{"pad_workspace", "audit-log"}: {"workspace", "audit-log"},

		{"pad_collection", "list"}:   {"collection", "list"},
		{"pad_collection", "create"}: {"collection", "create"},

		{"pad_project", "dashboard"}: {"project", "dashboard"},
		{"pad_project", "next"}:      {"project", "next"},
		{"pad_project", "standup"}:   {"project", "standup"},
		{"pad_project", "changelog"}: {"project", "changelog"},

		{"pad_role", "list"}:   {"role", "list"},
		{"pad_role", "create"}: {"role", "create"},
		{"pad_role", "delete"}: {"role", "delete"},

		{"pad_search", "query"}: {"item", "search"},
	}

	// Required-positional fixture: every CLI command in the read-only
	// surface gets its required args satisfied so BuildCLIArgs reaches
	// dispatcher.Dispatch instead of returning a missing-arg error.
	fixtureInput := map[string]any{
		"email": "test@example.com",
		"name":  "test-name",
		"slug":  "test-slug",
		"query": "test-query",
	}

	for _, def := range Catalog {
		if def.Name == "pad_meta" {
			continue // inline-handled, doesn't dispatch
		}
		for actionName, handler := range def.Actions {
			key := [2]string{def.Name, actionName}
			wantCmdPath, ok := expected[key]
			if !ok {
				// Already covered by TestReadOnlyCatalog_ActionsMatchCmdhelp's
				// bijection check; skip silently here to keep this test
				// focused on dispatch correctness.
				continue
			}
			t.Run(def.Name+"."+actionName, func(t *testing.T) {
				disp := &fakeDispatcher{}
				env := ActionEnv{
					Doc:        doc,
					Workspace:  NewWorkspaceState("docapp"),
					Dispatcher: disp,
				}
				// Per-test copy so cross-test ordering doesn't matter.
				input := make(map[string]any, len(fixtureInput))
				for k, v := range fixtureInput {
					input[k] = v
				}
				res, err := handler(context.Background(), input, env)
				if err != nil {
					t.Fatalf("handler returned protocol error: %v", err)
				}
				if res != nil && res.IsError {
					t.Fatalf("handler returned error result (BuildCLIArgs likely missing input): %s",
						textOf(res))
				}
				if !equalStrings(disp.gotPath, wantCmdPath) {
					t.Errorf("dispatched cmdPath = %v, want %v", disp.gotPath, wantCmdPath)
				}
			})
		}
	}
}

// TestPadWorkspaceAuditLog_RenamesActionFilter is the regression test
// for the action-flag-shadow bug: the catalog's `action_filter` input
// must arrive at the dispatcher as `--action <value>`. If the rename
// regresses, audit-log filtering silently breaks.
func TestPadWorkspaceAuditLog_RenamesActionFilter(t *testing.T) {
	disp := &fakeDispatcher{}
	env := ActionEnv{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: disp,
	}

	_, err := actionWorkspaceAuditLog(context.Background(), map[string]any{
		"action_filter": "item.created",
		"days":          float64(7),
	}, env)
	if err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	if !equalStrings(disp.gotPath, []string{"workspace", "audit-log"}) {
		t.Errorf("cmdPath = %v, want [workspace audit-log]", disp.gotPath)
	}
	// The dispatched cliArgs should include "--action item.created"
	// and "--days 7" — not "--action_filter ..." and not bare action.
	joined := strings.Join(disp.gotArgs, " ")
	if !strings.Contains(joined, "--action item.created") {
		t.Errorf("cliArgs should contain '--action item.created'; got %q", joined)
	}
	if strings.Contains(joined, "action_filter") {
		t.Errorf("cliArgs should not contain 'action_filter' (rename leaked); got %q", joined)
	}
	if !strings.Contains(joined, "--days 7") {
		t.Errorf("cliArgs should contain '--days 7'; got %q", joined)
	}
}

// TestPadWorkspaceAuditLog_ForwardsWithoutFilter exercises the
// no-rename path: a call with no action_filter dispatches identically
// to passThrough.
func TestPadWorkspaceAuditLog_ForwardsWithoutFilter(t *testing.T) {
	disp := &fakeDispatcher{}
	env := ActionEnv{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: disp,
	}
	_, err := actionWorkspaceAuditLog(context.Background(), map[string]any{
		"actor": "dave",
	}, env)
	if err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	joined := strings.Join(disp.gotArgs, " ")
	if !strings.Contains(joined, "--actor dave") {
		t.Errorf("cliArgs should contain '--actor dave'; got %q", joined)
	}
	if strings.Contains(joined, "--action ") {
		t.Errorf("cliArgs should not contain '--action ...' when no filter set; got %q", joined)
	}
}

// liveCmdhelpDoc returns a minimal cmdhelp.Document with every
// command path the read-only catalog references. Lighter than
// re-running pad's full cobra emitter inside tests; lets the
// table-driven assertions above run without external deps.
func liveCmdhelpDoc(t *testing.T) *cmdhelp.Document {
	t.Helper()
	mkFlags := func(names ...string) map[string]cmdhelp.Flag {
		out := make(map[string]cmdhelp.Flag, len(names))
		for _, n := range names {
			out[n] = cmdhelp.Flag{Type: "string"}
		}
		return out
	}
	mkArgs := func(names ...string) []cmdhelp.Arg {
		out := make([]cmdhelp.Arg, len(names))
		for i, n := range names {
			out[i] = cmdhelp.Arg{Name: n, Required: true}
		}
		return out
	}
	return &cmdhelp.Document{
		CmdhelpVersion: "0.1",
		Binary:         "pad",
		Commands: map[string]cmdhelp.Command{
			// Workspace
			"workspace list":    {Summary: "list ws"},
			"workspace members": {Summary: "list members", Flags: mkFlags("workspace")},
			"workspace invite": {
				Summary: "invite",
				Args:    mkArgs("email"),
				Flags:   mkFlags("workspace", "role"),
			},
			"workspace storage": {Summary: "storage", Flags: mkFlags("workspace")},
			"workspace audit-log": {
				Summary: "audit log",
				Flags: map[string]cmdhelp.Flag{
					"action": {Type: "string"},
					"actor":  {Type: "string"},
					"days":   {Type: "int"},
					"limit":  {Type: "int"},
				},
			},
			// Collection
			"collection list": {Summary: "list cols", Flags: mkFlags("workspace")},
			"collection create": {
				Summary: "create col",
				Args:    mkArgs("name"),
				Flags: mkFlags(
					"workspace", "fields", "icon", "description",
					"layout", "default-view", "board-group-by",
				),
			},
			// Project
			"project dashboard": {Summary: "dash", Flags: mkFlags("workspace")},
			"project next":      {Summary: "next", Flags: mkFlags("workspace")},
			"project standup": {
				Summary: "standup",
				Flags: map[string]cmdhelp.Flag{
					"workspace": {Type: "string"},
					"days":      {Type: "int"},
				},
			},
			"project changelog": {
				Summary: "changelog",
				Flags: map[string]cmdhelp.Flag{
					"workspace": {Type: "string"},
					"days":      {Type: "int"},
					"since":     {Type: "string"},
					"parent":    {Type: "string"},
				},
			},
			// Role
			"role list": {Summary: "list roles", Flags: mkFlags("workspace")},
			"role create": {
				Summary: "create role",
				Args:    mkArgs("name"),
				Flags:   mkFlags("workspace", "description", "icon", "tools"),
			},
			"role delete": {
				Summary: "delete role",
				Args:    mkArgs("slug"),
				Flags:   mkFlags("workspace"),
			},
			// Search (item search)
			"item search": {
				Summary: "search items",
				Args:    mkArgs("query"),
				Flags: mkFlags(
					"workspace", "collection", "status", "priority",
					"sort", "limit", "offset",
				),
			},
		},
	}
}
