package mcp

import (
	"context"
	"strings"
	"testing"
)

// TestPadItemLink_DispatchTable exercises every link_type ↔ cmdPath
// mapping in itemLinkRoutes for both action=link (create) and
// action=unlink (remove). Acts as the regression test for the
// "uniform (ref, target) → CLI's per-type positional names" rename.
//
// Cases are derived from itemLinkRoutes itself rather than hand-
// written so adding a new link_type only requires updating the route
// table, not this test.
func TestPadItemLink_DispatchTable(t *testing.T) {
	doc := liveCmdhelpDoc(t)
	ws := NewWorkspaceState("docapp")

	for linkType, route := range itemLinkRoutes {
		t.Run("link/"+linkType, func(t *testing.T) {
			disp := &fakeDispatcher{}
			env := ActionEnv{Doc: doc, Workspace: ws, Dispatcher: disp}
			input := map[string]any{
				"link_type": linkType,
				"ref":       "TASK-A",
				"target":    "TASK-B",
			}
			res, err := actionItemLink(context.Background(), input, env)
			if err != nil {
				t.Fatalf("actionItemLink error: %v", err)
			}
			if res != nil && res.IsError {
				t.Fatalf("error result: %s", textOf(res))
			}
			if !equalStrings(disp.gotPath, route.link.cmdPath) {
				t.Errorf("cmdPath = %v, want %v", disp.gotPath, route.link.cmdPath)
			}
			if len(disp.gotArgs) < 2 {
				t.Fatalf("cliArgs too short: %v", disp.gotArgs)
			}
			wantFirst, wantSecond := "TASK-A", "TASK-B"
			if route.link.inverted {
				wantFirst, wantSecond = "TASK-B", "TASK-A"
			}
			if disp.gotArgs[0] != wantFirst || disp.gotArgs[1] != wantSecond {
				t.Errorf("positionals = (%q, %q), want (%q, %q) — link op = %+v",
					disp.gotArgs[0], disp.gotArgs[1], wantFirst, wantSecond, route.link)
			}
		})

		if route.unlink.cmdPath == nil {
			continue
		}
		t.Run("unlink/"+linkType, func(t *testing.T) {
			disp := &fakeDispatcher{}
			env := ActionEnv{Doc: doc, Workspace: ws, Dispatcher: disp}
			input := map[string]any{
				"link_type": linkType,
				"ref":       "TASK-A",
				"target":    "TASK-B",
			}
			res, err := actionItemUnlink(context.Background(), input, env)
			if err != nil {
				t.Fatalf("actionItemUnlink error: %v", err)
			}
			if res != nil && res.IsError {
				t.Fatalf("error result: %s", textOf(res))
			}
			if !equalStrings(disp.gotPath, route.unlink.cmdPath) {
				t.Errorf("cmdPath = %v, want %v", disp.gotPath, route.unlink.cmdPath)
			}
			if len(disp.gotArgs) < 2 {
				t.Fatalf("cliArgs too short: %v", disp.gotArgs)
			}
			wantFirst, wantSecond := "TASK-A", "TASK-B"
			if route.unlink.inverted {
				wantFirst, wantSecond = "TASK-B", "TASK-A"
			}
			if disp.gotArgs[0] != wantFirst || disp.gotArgs[1] != wantSecond {
				t.Errorf("positionals = (%q, %q), want (%q, %q) — unlink op = %+v",
					disp.gotArgs[0], disp.gotArgs[1], wantFirst, wantSecond, route.unlink)
			}
		})
	}
}

// TestPadItemLink_MissingLinkType returns a structured error listing
// the valid options. Ensures the agent gets enough signal to retry
// with a correct value.
func TestPadItemLink_MissingLinkType(t *testing.T) {
	disp := &fakeDispatcher{}
	env := ActionEnv{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: disp,
	}
	res, err := actionItemLink(context.Background(), map[string]any{
		"ref":    "TASK-A",
		"target": "TASK-B",
	}, env)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError, got %s", textOf(res))
	}
	msg := textOf(res)
	if !strings.Contains(msg, "link_type is required") {
		t.Errorf("message %q should mention required link_type", msg)
	}
	for k := range itemLinkRoutes {
		if !strings.Contains(msg, k) {
			t.Errorf("message %q should list link_type %q", msg, k)
		}
	}
	if len(disp.gotPath) > 0 {
		t.Errorf("dispatcher should not have been called; got %v", disp.gotPath)
	}
}

// TestPadItemLink_UnknownLinkType rejects values outside the route
// table. Must surface what's valid so consumers can correct.
func TestPadItemLink_UnknownLinkType(t *testing.T) {
	env := ActionEnv{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: &fakeDispatcher{},
	}
	res, err := actionItemLink(context.Background(), map[string]any{
		"link_type": "totally-invalid",
		"ref":       "TASK-A",
		"target":    "TASK-B",
	}, env)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError, got %s", textOf(res))
	}
	// Post-TASK-1077 the message lives inside the structured envelope
	// (quotes are JSON-escaped in the wire form). Substring on the
	// bad-value token is sufficient; the literal-quoted form has
	// backslashes around it.
	msg := textOf(res)
	if !strings.Contains(msg, `totally-invalid`) {
		t.Errorf("message %q should name the bad value", msg)
	}
}

// TestPadItemLink_MissingRefOrTarget covers the other two required
// inputs. Failing fast at the action handler keeps BuildCLIArgs from
// emitting a confusingly-named CLI error.
func TestPadItemLink_MissingRefOrTarget(t *testing.T) {
	cases := []struct {
		name  string
		input map[string]any
		want  string
	}{
		{
			name: "missing ref",
			input: map[string]any{
				"link_type": "blocks",
				"target":    "TASK-B",
			},
			want: "ref is required",
		},
		{
			name: "missing target",
			input: map[string]any{
				"link_type": "blocks",
				"ref":       "TASK-A",
			},
			want: "target is required",
		},
	}
	env := ActionEnv{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: &fakeDispatcher{},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := actionItemLink(context.Background(), tc.input, env)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected IsError, got %s", textOf(res))
			}
			if !strings.Contains(textOf(res), tc.want) {
				t.Errorf("message %q should contain %q", textOf(res), tc.want)
			}
		})
	}
}

// TestPadItemBulkUpdate_TranslatesRefsArray verifies the catalog's
// `refs: array<string>` param becomes the CLI's repeatable positional
// `ref` (one positional per refs entry). Regression test for the
// scalar/array mismatch Codex flagged on PR #354.
func TestPadItemBulkUpdate_TranslatesRefsArray(t *testing.T) {
	t.Run("array of strings", func(t *testing.T) {
		disp := &fakeDispatcher{}
		env := ActionEnv{
			Doc:        liveCmdhelpDoc(t),
			Workspace:  NewWorkspaceState("docapp"),
			Dispatcher: disp,
		}
		_, err := actionItemBulkUpdate(context.Background(), map[string]any{
			"refs":   []any{"TASK-5", "TASK-8"},
			"status": "done",
		}, env)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !equalStrings(disp.gotPath, []string{"item", "bulk-update"}) {
			t.Errorf("cmdPath = %v, want [item bulk-update]", disp.gotPath)
		}
		// Both refs become positionals (BuildCLIArgs expands repeatable
		// args). The `--status done` flag follows.
		joined := strings.Join(disp.gotArgs, " ")
		if !strings.HasPrefix(joined, "TASK-5 TASK-8") {
			t.Errorf("cliArgs %q should begin with both refs as positionals", joined)
		}
		if !strings.Contains(joined, "--status done") {
			t.Errorf("cliArgs %q should contain --status done", joined)
		}
	})

	t.Run("single string lenient fallback", func(t *testing.T) {
		disp := &fakeDispatcher{}
		env := ActionEnv{
			Doc:        liveCmdhelpDoc(t),
			Workspace:  NewWorkspaceState("docapp"),
			Dispatcher: disp,
		}
		_, err := actionItemBulkUpdate(context.Background(), map[string]any{
			"refs":   "TASK-5", // unwrapped — still acceptable
			"status": "done",
		}, env)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if disp.gotArgs[0] != "TASK-5" {
			t.Errorf("first positional = %q, want TASK-5", disp.gotArgs[0])
		}
	})

	t.Run("missing refs", func(t *testing.T) {
		env := ActionEnv{
			Doc:        liveCmdhelpDoc(t),
			Workspace:  NewWorkspaceState("docapp"),
			Dispatcher: &fakeDispatcher{},
		}
		res, err := actionItemBulkUpdate(context.Background(), map[string]any{
			"status": "done",
		}, env)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !res.IsError {
			t.Fatalf("expected IsError, got %s", textOf(res))
		}
		if !strings.Contains(textOf(res), "refs is required") {
			t.Errorf("error %q should mention refs requirement", textOf(res))
		}
	})

	t.Run("empty refs array", func(t *testing.T) {
		env := ActionEnv{
			Doc:        liveCmdhelpDoc(t),
			Workspace:  NewWorkspaceState("docapp"),
			Dispatcher: &fakeDispatcher{},
		}
		res, err := actionItemBulkUpdate(context.Background(), map[string]any{
			"refs":   []any{},
			"status": "done",
		}, env)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !res.IsError {
			t.Fatalf("expected IsError, got %s", textOf(res))
		}
	})
}

// TestPadItemLink_ForwardsAuxFields confirms that non-link fields
// (workspace, format) flow through to the dispatcher unchanged. The
// rename should ONLY touch ref/target/link_type.
func TestPadItemLink_ForwardsAuxFields(t *testing.T) {
	disp := &fakeDispatcher{}
	env := ActionEnv{
		Doc:        liveCmdhelpDoc(t),
		Workspace:  NewWorkspaceState("docapp"),
		Dispatcher: disp,
	}
	_, err := actionItemLink(context.Background(), map[string]any{
		"link_type": "blocks",
		"ref":       "TASK-A",
		"target":    "TASK-B",
		"workspace": "docapp", // explicit workspace
	}, env)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	joined := strings.Join(disp.gotArgs, " ")
	if !strings.Contains(joined, "--workspace docapp") {
		t.Errorf("explicit workspace should reach dispatcher; got %q", joined)
	}
}
