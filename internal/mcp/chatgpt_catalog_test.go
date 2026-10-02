package mcp

import (
	"sort"
	"strings"
	"testing"
)

// TestChatGPTCatalog_Valid: the catalog as shipped passes the checks
// registration applies.
func TestChatGPTCatalog_Valid(t *testing.T) {
	if err := ValidateChatGPTCatalog(); err != nil {
		t.Fatal(err)
	}
}

// TestChatGPTCatalog_CoversEveryOperation is the parity guard the lead's
// ruling asks for (TASK-3321): every /mcp operation, and pad_set_workspace,
// is EITHER a ChatGPT entry OR an exclusion with a reason, never both and
// never neither, and neither list names an operation that does not exist.
// Adding an action to Catalog without deciding its ChatGPT fate fails here.
func TestChatGPTCatalog_CoversEveryOperation(t *testing.T) {
	ops := map[ChatGPTSource]bool{{Tool: chatGPTSetWorkspaceSource}: true}
	for _, def := range Catalog {
		for action := range def.Actions {
			ops[ChatGPTSource{def.Name, action}] = true
		}
	}
	entries := map[ChatGPTSource]string{}
	for _, e := range ChatGPTCatalog {
		if prev, dup := entries[e.Source]; dup {
			t.Errorf("%s is the source of both %s and %s", e.Source, prev, e.Name)
		}
		entries[e.Source] = e.Name
	}
	var missing, both []string
	for op := range ops {
		_, inEntries := entries[op]
		reason, inExclusions := ChatGPTExclusions[op]
		switch {
		case inEntries && inExclusions:
			both = append(both, op.String())
		case !inEntries && !inExclusions:
			missing = append(missing, op.String())
		case inExclusions && strings.TrimSpace(reason) == "":
			t.Errorf("exclusion %s has no reason", op)
		}
	}
	sort.Strings(missing)
	sort.Strings(both)
	if len(missing) > 0 {
		t.Errorf("/mcp operations with neither a ChatGPT entry nor an exclusion: %v", missing)
	}
	if len(both) > 0 {
		t.Errorf("operations both exposed and excluded: %v", both)
	}
	for op := range ChatGPTExclusions {
		if !ops[op] {
			t.Errorf("exclusion %s names no /mcp operation (stale)", op)
		}
	}
	for op, name := range entries {
		if !ops[op] {
			t.Errorf("entry %s names no /mcp operation %s (stale)", name, op)
		}
	}
	if got, want := len(entries)+len(ChatGPTExclusions), len(ops); got != want {
		t.Errorf("entries %d + exclusions %d = %d, want every operation (%d)", len(entries), len(ChatGPTExclusions), got, want)
	}
}

// TestChatGPTCatalog_HintsAsRuled pins each tool's hints to the ruling,
// written out here rather than read back from the catalog, so the two can
// disagree and fail. update_item is NOT destructive: every content change is
// saved as a version first (ruling (b) on TASK-3321). archive_item is.
func TestChatGPTCatalog_HintsAsRuled(t *testing.T) {
	writes := map[string]bool{ // name -> destructive
		"create_item":  false,
		"update_item":  false,
		"archive_item": true,
		"restore_item": false,
		"add_comment":  false,
		"link_items":   false,
	}
	seen := 0
	for _, e := range ChatGPTCatalog {
		destructive, isWrite := writes[e.Name]
		if isWrite {
			seen++
			if e.Hints.ReadOnly || e.Hints.Destructive != destructive {
				t.Errorf("%s hints = %+v, want readOnly=false destructive=%v", e.Name, e.Hints, destructive)
			}
		} else if !e.Hints.ReadOnly || e.Hints.Destructive {
			t.Errorf("%s hints = %+v, want read-only and non-destructive", e.Name, e.Hints)
		}
		if e.Hints.OpenWorld {
			t.Errorf("%s is open-world; no v1 tool reaches outside the workspace", e.Name)
		}
	}
	if seen != len(writes) {
		t.Errorf("found %d of the %d expected write tools", seen, len(writes))
	}
}

// The validator is an instrument: each defect it exists to catch must fail
// it. Each case breaks one entry on a copy of the catalog.
func TestValidateChatGPTCatalog_RefusesDefects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ChatGPTTool)
		want   string
	}{
		{"unknown param", func(e *ChatGPTTool) { e.Params = append(e.Params, "no_such_param") }, "is not a parameter"},
		{"required not declared", func(e *ChatGPTTool) { e.Required = append(e.Required, "title") }, "is not a declared param"},
		{"read-only over a write", func(e *ChatGPTTool) { e.Source = ChatGPTSource{"pad_item", "create"} }, "not in readOnlyActions"},
		{"unknown action", func(e *ChatGPTTool) { e.Source.Action = "nope" }, "does not exist"},
		{"open-world mismatch", func(e *ChatGPTTool) { e.Hints.OpenWorld = true }, "disagrees with openWorldActions"},
		{"write without justification", func(e *ChatGPTTool) { e.Hints = ChatGPTHints{} }, "needs a Justification"},
	}
	orig := ChatGPTCatalog
	t.Cleanup(func() { ChatGPTCatalog = orig })
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := append([]ChatGPTTool(nil), orig...)
			for i := range cp {
				if cp[i].Name == "get_item" {
					cp[i].Params = append([]string(nil), cp[i].Params...)
					cp[i].Required = append([]string(nil), cp[i].Required...)
					tc.mutate(&cp[i])
				}
			}
			ChatGPTCatalog = cp
			err := ValidateChatGPTCatalog()
			ChatGPTCatalog = orig
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validator error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// No ChatGPT tool or parameter description may use /mcp's vocabulary: action
// names, "Required for:" lists or pad_* tool names describe operations these
// tools do not have, and OpenAI's review requires descriptions that match
// the tool.
func TestChatGPTCatalog_DescriptionsSpeakForThisSurface(t *testing.T) {
	banned := []string{"action=", "Required for", "Optional for", "pad_", "bulk-update"}
	check := func(where, text string) {
		for _, b := range banned {
			if strings.Contains(text, b) {
				t.Errorf("%s mentions %q: %s", where, b, text)
			}
		}
	}
	for _, e := range ChatGPTCatalog {
		check(e.Name, e.Description)
		var params []ParamDef
		if def, ok := catalogDef(e.Source.Tool); ok {
			params = chatGPTParams(e, def)
		}
		for _, p := range params {
			check(e.Name+"."+p.Name, p.Description)
		}
	}
	check("instructions", ChatGPTInstructions)
}
