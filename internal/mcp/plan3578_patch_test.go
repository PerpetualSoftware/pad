package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// PLAN-3578 step 1 at the MCP doors.

func TestPLAN3578_MCPPatchLinesAndRefusals(t *testing.T) {
	f := newParentFixture(t)
	ws, ref := f.workspace.Slug, f.child.Slug
	f.dispatch(t, []string{"item", "update"}, map[string]any{"workspace": ws, "ref": ref, "content": "alpha\nbeta\ngamma beta\n", "replace": true})

	// get with lines: numbered lines and the seq, on the catalog path both
	// transports share.
	res, err := actionItemGet(t.Context(), map[string]any{"workspace": ws, "ref": ref, "lines": "2-3"}, f.env(t))
	if err != nil || res.IsError {
		t.Fatalf("get lines: %v %s", err, textOf(res))
	}
	var view struct {
		Seq   int64 `json:"seq"`
		Lines []struct {
			N    int    `json:"n"`
			Text string `json:"text"`
		} `json:"lines"`
		TotalLines int `json:"total_lines"`
	}
	b, _ := json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(b, &view)
	if len(view.Lines) != 2 || view.Lines[0].N != 2 || view.Lines[0].Text != "beta" || view.TotalLines != 3 || view.Seq == 0 {
		t.Fatalf("lines view = %+v", view)
	}

	// A patch lands and answers with the diff.
	res = f.dispatchResult2(t, []string{"item", "patch"}, map[string]any{"workspace": ws, "ref": ref,
		"edits": []any{map[string]any{"old": "alpha", "new": "ALPHA"}}})
	if res.IsError {
		t.Fatalf("patch: %s", textOf(res))
	}
	m, _ := res.StructuredContent.(map[string]any)
	if d, _ := m["diff"].(string); !strings.Contains(d, "+ALPHA") {
		t.Fatalf("patch answer = %v", m)
	}

	// An ambiguous anchor: its own code, the lines, the shared hint.
	res = f.dispatchResult2(t, []string{"item", "patch"}, map[string]any{"workspace": ws, "ref": ref,
		"edits": []any{map[string]any{"old": "beta", "new": "B"}}})
	env, ok := res.StructuredContent.(ErrorEnvelope)
	if !res.IsError || !ok || env.Error.Code != ErrPatchAnchorAmbiguous || env.Error.Hint != PatchAnchorHint {
		t.Fatalf("ambiguous: %s", textOf(res))
	}
	var d map[string]any
	_ = json.Unmarshal(env.Error.Details, &d)
	if lines, _ := d["lines"].([]any); len(lines) != 2 {
		t.Fatalf("details = %v", d)
	}
}

// Stdio: the CLI's marker carries the code, details and hint.
func TestPLAN3578_StdioPatchRefusal(t *testing.T) {
	for code, hint := range map[string]string{
		"patch_anchor_missing":     PatchAnchorHint,
		"patch_anchor_ambiguous":   PatchAnchorHint,
		"patch_room_open":          PatchRoomOpenHint,
		"patch_unstable_roundtrip": PatchUnstableHint,
	} {
		var stderr bytes.Buffer
		cli.WritePatchRefusalError(&stderr, &cli.APIError{Code: code, Message: "m", Details: json.RawMessage(`{"ref":"TASK-1","edit":0}`)})
		res := extractStructuredCLIError(stderr.String())
		if res == nil {
			t.Fatalf("%s: marker not recognised (not in the allow-list?)", code)
		}
		env := res.StructuredContent.(ErrorEnvelope)
		if string(env.Error.Code) != code || env.Error.Hint != hint {
			t.Fatalf("%s: got %s %q", code, env.Error.Code, env.Error.Hint)
		}
	}
}

func TestPLAN3578_CLIAndMCPAgreeOnPatchHints(t *testing.T) {
	if cli.PatchAnchorHint != PatchAnchorHint || cli.PatchRoomOpenHint != PatchRoomOpenHint || cli.PatchUnstableHint != PatchUnstableHint {
		t.Fatal("patch hints differ between internal/cli and internal/mcp")
	}
	for _, c := range []string{cli.PatchAnchorMissingCode, cli.PatchAnchorAmbiguousCode, cli.PatchRoomOpenCode, cli.PatchUnstableCode} {
		if _, ok := allowedStructuredErrorCodes[c]; !ok {
			t.Fatalf("%s is not in allowedStructuredErrorCodes", c)
		}
	}
}

func (f *parentFixture) httpDispatcher() *HTTPHandlerDispatcher {
	return &HTTPHandlerDispatcher{
		Handler:      f.srv,
		UserResolver: func(context.Context) *models.User { return f.owner },
	}
}

func (f *parentFixture) env(t *testing.T) ActionEnv {
	t.Helper()
	return ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState(f.workspace.Slug), Dispatcher: f.httpDispatcher()}
}

func (f *parentFixture) dispatchResult2(t *testing.T, cmd []string, input map[string]any) *CallToolResult {
	t.Helper()
	res, err := f.httpDispatcher().Dispatch(WithDispatchInput(context.Background(), input), cmd, nil)
	if err != nil {
		t.Fatalf("Dispatch(%v): %v", cmd, err)
	}
	return res
}
