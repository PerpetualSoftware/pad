package mcp

import (
	"context"
	"strings"
	"testing"
)

// TestCatalogWorkspacePrecedence covers the four resolution cases
// from TASK-972's spec. Drives a real catalog action through the
// fan-out handler so it exercises the same code path Claude Desktop
// hits — the catalog tools' workspace parameter must reach
// BuildCLIArgs, which then applies the (explicit > session > CLI-CWD)
// precedence.
//
// Case 4 (no workspace anywhere → no_workspace structured error) is
// out of scope for this task — it depends on TASK-973's error
// taxonomy. For now, an empty-everywhere call simply omits
// --workspace and lets the CLI subprocess fall back to .pad.toml,
// which is the documented v0.2 behaviour.
func TestCatalogWorkspacePrecedence(t *testing.T) {
	doc := liveCmdhelpDoc(t)

	cases := []struct {
		name          string
		explicit      string // workspace passed in tool input
		session       string // pad_set_workspace value
		wantInArgs    bool   // should --workspace appear at all?
		wantWorkspace string // expected --workspace value (when wantInArgs)
		wantOnlyOnce  bool   // assert exactly one --workspace flag
	}{
		{
			name:          "explicit param wins over session",
			explicit:      "explicit-ws",
			session:       "session-ws",
			wantInArgs:    true,
			wantWorkspace: "explicit-ws",
			wantOnlyOnce:  true,
		},
		{
			name:          "session used when no explicit param",
			explicit:      "",
			session:       "session-ws",
			wantInArgs:    true,
			wantWorkspace: "session-ws",
			wantOnlyOnce:  true,
		},
		{
			name:         "neither: --workspace omitted (CLI falls back to CWD .pad.toml)",
			explicit:     "",
			session:      "",
			wantInArgs:   false,
			wantOnlyOnce: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := &fakeDispatcher{}
			env := ActionEnv{
				Doc:        doc,
				Workspace:  NewWorkspaceState(tc.session),
				Dispatcher: disp,
			}
			handler := makeFanOutHandler(padItemTool, env)

			// pad_item.action: list with optional explicit workspace.
			// list is a safe pick — it's the simplest read-only
			// passThrough and exercises the standard input flow.
			input := map[string]any{
				"action": "list",
			}
			if tc.explicit != "" {
				input["workspace"] = tc.explicit
			}
			res, err := handler(context.Background(), callToolRequest(input))
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			if res.IsError {
				t.Fatalf("unexpected error result: %s", textOf(res))
			}

			wsCount := 0
			wsValue := ""
			for i, a := range disp.gotArgs {
				if a == "--workspace" && i+1 < len(disp.gotArgs) {
					wsCount++
					wsValue = disp.gotArgs[i+1]
				}
			}
			if !tc.wantInArgs {
				if wsCount != 0 {
					t.Errorf("expected no --workspace flag (CLI handles CWD); got %d in %v",
						wsCount, disp.gotArgs)
				}
				return
			}
			if tc.wantOnlyOnce && wsCount != 1 {
				t.Errorf("expected exactly 1 --workspace flag, got %d in %v",
					wsCount, disp.gotArgs)
			}
			if wsValue != tc.wantWorkspace {
				t.Errorf("--workspace value = %q, want %q", wsValue, tc.wantWorkspace)
			}
		})
	}
}

// TestCatalogWorkspaceParamAdvertisedOnAllWorkspaceTools confirms
// every tool with Schema.Workspace=true exposes a `workspace` property
// in its tools/list schema. Catches future regressions where someone
// adds a workspace-scoped tool but forgets to set the flag.
//
// pad_meta is the one tool that legitimately omits workspace (it's
// server-wide); enumerated here to make the omission deliberate
// rather than accidental.
func TestCatalogWorkspaceParamAdvertisedOnAllWorkspaceTools(t *testing.T) {
	intentionallyServerWide := map[string]bool{
		"pad_meta": true,
	}
	for _, def := range Catalog {
		t.Run(def.Name, func(t *testing.T) {
			tool := buildToolFromDef(def)
			has := false
			for prop := range tool.InputSchema.Properties {
				if prop == "workspace" {
					has = true
					break
				}
			}
			if intentionallyServerWide[def.Name] {
				if has {
					t.Errorf("%s is documented as server-wide but its schema includes workspace; "+
						"either set Schema.Workspace=false or update the intentional list",
						def.Name)
				}
				return
			}
			if !has {
				t.Errorf("%s is workspace-scoped but its schema is missing 'workspace' property", def.Name)
			}
			if !def.Schema.Workspace {
				t.Errorf("%s has Schema.Workspace=false; should be true for workspace-scoped tools", def.Name)
			}
		})
	}
}

// TestCatalogWorkspaceDescriptionDocumentsPrecedence pins the schema-
// level description so the resolution order is discoverable by agents
// that read the tool's input schema. If someone shortens the
// description below the substantive detail, this test fails.
func TestCatalogWorkspaceDescriptionDocumentsPrecedence(t *testing.T) {
	tool := buildToolFromDef(padItemTool) // any workspace-scoped tool works
	prop, ok := tool.InputSchema.Properties["workspace"]
	if !ok {
		t.Fatalf("pad_item input schema missing workspace property")
	}
	desc, ok := prop.(map[string]any)["description"].(string)
	if !ok || desc == "" {
		t.Fatalf("workspace description missing or non-string: %#v", prop)
	}
	mustContain := []string{
		"explicit",          // explicit param is mentioned
		"pad_set_workspace", // session-default mechanism named
		".pad.toml",         // CWD fallback documented
	}
	for _, want := range mustContain {
		if !strings.Contains(desc, want) {
			t.Errorf("workspace description should mention %q; got %q", want, desc)
		}
	}
}
