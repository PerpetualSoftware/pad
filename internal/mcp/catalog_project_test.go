package mcp

import "testing"

// findCatalogTool returns the ToolDef with the given name, or a failed test.
func findCatalogTool(t *testing.T, name string) ToolDef {
	t.Helper()
	for _, def := range Catalog {
		if def.Name == name {
			return def
		}
	}
	t.Fatalf("tool %q not found in Catalog", name)
	return ToolDef{}
}

// TestCatalogProject_HasActivityAction pins TASK-2018: pad_project exposes
// an `activity` action alongside the existing computed-view actions, and
// the action is a passThrough to `project activity`.
func TestCatalogProject_HasActivityAction(t *testing.T) {
	def := findCatalogTool(t, "pad_project")

	if _, ok := def.Actions["activity"]; !ok {
		t.Fatalf("pad_project is missing the 'activity' action (TASK-2018); actions present: %v", keysOf(def.Actions))
	}

	// The pre-existing computed-view actions must still be present — the
	// bump is purely additive.
	for _, want := range []string{"dashboard", "next", "standup", "changelog", "report"} {
		if _, ok := def.Actions[want]; !ok {
			t.Errorf("pad_project lost the %q action — the activity bump must be additive", want)
		}
	}
}

// TestCatalogProject_ActivityParamsExposed verifies the actor + limit
// params activity relies on are declared on the tool schema (since already
// existed for changelog). Without these the ExecDispatcher's BuildCLIArgs
// can't translate them into --actor / --limit flags.
func TestCatalogProject_ActivityParamsExposed(t *testing.T) {
	def := findCatalogTool(t, "pad_project")

	have := map[string]bool{}
	for _, p := range def.Schema.Params {
		have[p.Name] = true
	}
	for _, want := range []string{"actor", "limit", "since"} {
		if !have[want] {
			t.Errorf("pad_project schema is missing the %q param needed by action=activity", want)
		}
	}
}

func keysOf(m map[string]ActionFn) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
