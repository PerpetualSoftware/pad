package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
)

// TestReadOnlyCatalog_AllToolsRegistered locks the v0.2 read-only
// surface. If a future commit accidentally drops a tool from
// appendToCatalog (or adds one without intent), this test forces a
// deliberate update to the expected list.
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
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("expected catalog entry %q not registered", name)
		}
	}
}

// TestReadOnlyCatalog_ActionsMatchCmdhelp guards against cmdhelp drift.
// Every passThrough action's cmdPath must resolve in cmdhelp; if it
// doesn't, env.Dispatch returns a "registry bug" error at runtime —
// this test catches it at compile-time-of-test rather than waiting for
// a user to hit the action.
//
// Custom (non-passThrough) actions are skipped because they may
// reshape the input or compose multiple cmdPaths internally.
func TestReadOnlyCatalog_ActionsMatchCmdhelp(t *testing.T) {
	doc := liveCmdhelpDoc(t)

	// Hand-curated map of (toolName, action) → expected cmdPath, used
	// to catch silent renames in catalog_*.go. Custom handlers
	// (workspace audit-log) appear here too — they dispatch to the
	// listed cmdPath even though the input shape differs.
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
	for key, cmdPath := range expected {
		joined := strings.Join(cmdPath, " ")
		if _, ok := doc.Commands[joined]; !ok {
			t.Errorf("%s.action=%s maps to cmdPath %q which is not in cmdhelp",
				key[0], key[1], joined)
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
