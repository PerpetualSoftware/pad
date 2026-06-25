package mcp

import (
	"encoding/json"
	"testing"
)

// toolSurfaceDoc is the decoded shape of ToolSurfaceJSON's output —
// just the fields these tests assert on.
type toolSurfaceDoc struct {
	ToolSurfaceVersion string `json:"tool_surface_version"`
	RolloutStatus      string `json:"rollout_status"`
	Tools              []struct {
		Name      string `json:"name"`
		Workspace bool   `json:"workspace"`
		Actions   []struct {
			Name     string `json:"name"`
			ReadOnly bool   `json:"read_only"`
		} `json:"actions"`
		Params []struct {
			Name string   `json:"name"`
			Type string   `json:"type"`
			Enum []string `json:"enum,omitempty"`
		} `json:"params"`
	} `json:"tools"`
}

func decodeToolSurface(t *testing.T) toolSurfaceDoc {
	t.Helper()
	body, err := ToolSurfaceJSON()
	if err != nil {
		t.Fatalf("ToolSurfaceJSON: %v", err)
	}
	var doc toolSurfaceDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, body)
	}
	return doc
}

// TestToolSurfaceJSON_MirrorsCatalog locks the cycle-free serializer to
// the package-global Catalog: one entry per tool, version stamped,
// pad_set_workspace excluded (it's not in Catalog).
func TestToolSurfaceJSON_MirrorsCatalog(t *testing.T) {
	doc := decodeToolSurface(t)

	if doc.ToolSurfaceVersion != ToolSurfaceVersion {
		t.Errorf("tool_surface_version = %q, want %q", doc.ToolSurfaceVersion, ToolSurfaceVersion)
	}
	if len(doc.Tools) != len(Catalog) {
		t.Fatalf("tools length = %d, want %d (one per Catalog entry)", len(doc.Tools), len(Catalog))
	}
	for _, tl := range doc.Tools {
		if tl.Name == "pad_set_workspace" {
			t.Errorf("pad_set_workspace must not appear (registered separately, not in Catalog)")
		}
	}
	// Every Catalog entry must be present by name.
	byName := map[string]bool{}
	for _, tl := range doc.Tools {
		byName[tl.Name] = true
	}
	for _, def := range Catalog {
		if !byName[def.Name] {
			t.Errorf("Catalog tool %q missing from ToolSurfaceJSON output", def.Name)
		}
	}
}

// TestToolSurfaceJSON_ReadOnlyFlags asserts each action carries a
// read_only bool that matches the readOnlyActions allowlist — and
// spot-checks a few known read vs write actions so the set can't drift
// silently. Mixed tools (pad_item) must expose BOTH a read-only=true and
// a read-only=false action so the Phase 3 browser layer correctly
// withholds the readOnlyHint.
func TestToolSurfaceJSON_ReadOnlyFlags(t *testing.T) {
	doc := decodeToolSurface(t)

	// Build (tool, action) → read_only from the serialized output.
	got := map[[2]string]bool{}
	for _, tl := range doc.Tools {
		for _, a := range tl.Actions {
			got[[2]string{tl.Name, a.Name}] = a.ReadOnly
			// Every action's read_only must equal the allowlist lookup —
			// proves the serializer consults readOnlyActions, not some
			// other (drifting) source.
			if want := isReadOnlyAction(tl.Name, a.Name); a.ReadOnly != want {
				t.Errorf("%s.%s read_only = %v, want %v (per readOnlyActions)",
					tl.Name, a.Name, a.ReadOnly, want)
			}
		}
	}

	// Spot-check known reads.
	reads := [][2]string{
		{"pad_item", "get"},
		{"pad_item", "list"},
		{"pad_item", "backlinks"},
		{"pad_item", "export"},
		{"pad_project", "dashboard"},
		{"pad_search", "query"},
		{"pad_meta", "tool-surface"},
		{"pad_library", "get"},
		{"pad_playbook", "run"},
	}
	for _, k := range reads {
		v, ok := got[k]
		if !ok {
			t.Errorf("%s.%s absent from surface", k[0], k[1])
			continue
		}
		if !v {
			t.Errorf("%s.%s expected read_only=true", k[0], k[1])
		}
	}

	// Spot-check known writes.
	writes := [][2]string{
		{"pad_item", "create"},
		{"pad_item", "delete"},
		{"pad_item", "import"},
		{"pad_collection", "create"},
		{"pad_workspace", "invite"},
		{"pad_library", "activate"},
	}
	for _, k := range writes {
		v, ok := got[k]
		if !ok {
			t.Errorf("%s.%s absent from surface", k[0], k[1])
			continue
		}
		if v {
			t.Errorf("%s.%s expected read_only=false", k[0], k[1])
		}
	}

	// pad_item is a mixed tool — it must carry at least one read AND one
	// write so DR-2's "all-read ⇒ readOnlyHint" derivation withholds the
	// hint for it.
	var sawRead, sawWrite bool
	for _, tl := range doc.Tools {
		if tl.Name != "pad_item" {
			continue
		}
		for _, a := range tl.Actions {
			if a.ReadOnly {
				sawRead = true
			} else {
				sawWrite = true
			}
		}
	}
	if !sawRead || !sawWrite {
		t.Errorf("pad_item should be mixed: sawRead=%v sawWrite=%v", sawRead, sawWrite)
	}
}

// TestReadOnlyActions_NoStaleEntries guards against drift: readOnlyActions
// is an allowlist (only reads listed; absence ⇒ write), so we can't
// require every action to appear. We CAN require the reverse — every
// (tool, action) listed as read-only must still exist in the live
// Catalog, so a removed/renamed read action doesn't leave a stale
// entry silently advertising a non-existent action as read-only.
func TestReadOnlyActions_NoStaleEntries(t *testing.T) {
	live := map[[2]string]bool{}
	knownTool := map[string]bool{}
	for _, def := range Catalog {
		knownTool[def.Name] = true
		for action := range def.Actions {
			live[[2]string{def.Name, action}] = true
		}
	}
	for tool, set := range readOnlyActions {
		if !knownTool[tool] {
			t.Errorf("readOnlyActions has tool %q not in Catalog (stale entry)", tool)
			continue
		}
		for action, ro := range set {
			if !ro {
				t.Errorf("readOnlyActions[%s][%s] is false — the allowlist should only list reads (omit writes)",
					tool, action)
			}
			if !live[[2]string{tool, action}] {
				t.Errorf("readOnlyActions[%s][%s] has no matching Catalog action (stale entry)",
					tool, action)
			}
		}
	}
}
