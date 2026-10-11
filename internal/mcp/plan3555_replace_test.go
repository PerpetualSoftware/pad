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

// PLAN-3555 phase 1 at the MCP doors: the unguarded-write warning reaches the
// agent, replace reaches the server and is refused where it contradicts
// itself, and a content conflict carries the do-not-resend hint on both
// transports.

func (f *parentFixture) dispatchResult(t *testing.T, input map[string]any) *CallToolResult {
	t.Helper()
	d := &HTTPHandlerDispatcher{
		Handler:      f.srv,
		UserResolver: func(context.Context) *models.User { return f.owner },
	}
	res, err := d.Dispatch(WithDispatchInput(context.Background(), input), []string{"item", "update"}, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	return res
}

func unguardedFlag(t *testing.T, res *CallToolResult) bool {
	t.Helper()
	if res.IsError {
		t.Fatalf("update refused: %s", textOf(res))
	}
	m, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content is %T", res.StructuredContent)
	}
	w, _ := m["warnings"].(map[string]any)
	flagged, _ := w["unguarded_content_write"].(bool)
	return flagged
}

func TestPLAN3555_MCPReplaceAndWarning(t *testing.T) {
	f := newParentFixture(t)
	base := func() map[string]any {
		return map[string]any{"workspace": f.workspace.Slug, "ref": f.child.Slug}
	}

	in := base()
	in["content"] = "unguarded"
	if !unguardedFlag(t, f.dispatchResult(t, in)) {
		t.Fatal("an unguarded content update did not carry warnings.unguarded_content_write")
	}

	in = base()
	in["content"] = "deliberate"
	in["replace"] = true
	if unguardedFlag(t, f.dispatchResult(t, in)) {
		t.Fatal("replace=true was flagged unguarded: it did not reach the server")
	}

	cur, err := f.store.GetItem(f.child.ID)
	if err != nil {
		t.Fatal(err)
	}
	in = base()
	in["content"] = "contradiction"
	in["replace"] = true
	in["expected_seq"] = float64(cur.Seq)
	if msg := f.dispatchExpectingError(t, []string{"item", "update"}, in); !strings.Contains(msg, "replace conflicts with expected_seq") {
		t.Fatalf("replace with a token: %s", msg)
	}

	in = base()
	in["title"] = "No body"
	in["replace"] = true
	if msg := f.dispatchExpectingError(t, []string{"item", "update"}, in); !strings.Contains(msg, "replace applies to a content write") {
		t.Fatalf("replace without content: %s", msg)
	}
}

func TestPLAN3555_MCPContentConflictHint(t *testing.T) {
	f := newParentFixture(t)
	read, err := f.store.GetItem(f.child.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.dispatch(t, []string{"item", "update"}, map[string]any{"workspace": f.workspace.Slug, "ref": f.child.Slug, "content": "theirs", "replace": true})

	hint := func(extra map[string]any) (string, map[string]any) {
		t.Helper()
		in := map[string]any{"workspace": f.workspace.Slug, "ref": f.child.Slug, "expected_seq": float64(read.Seq)}
		for k, v := range extra {
			in[k] = v
		}
		res := f.dispatchResult(t, in)
		env, ok := res.StructuredContent.(ErrorEnvelope)
		if !res.IsError || !ok || env.Error.Code != ErrUpdateConflict {
			t.Fatalf("want update_conflict, got %s", textOf(res))
		}
		var d map[string]any
		_ = json.Unmarshal(env.Error.Details, &d)
		return env.Error.Hint, d
	}
	h, d := hint(map[string]any{"content": "mine"})
	if h != UpdateConflictContentHint || d["content_write"] != true {
		t.Fatalf("content conflict: hint %q details %v", h, d)
	}
	if h, _ := hint(map[string]any{"title": "Mine"}); h == UpdateConflictContentHint {
		t.Fatal("a field-only conflict got the content hint")
	}
}

// Stdio half, through the REAL CLI writer: the marker carries the hint and the
// new details, which the CLI re-marshals from its own struct.
func TestPLAN3555_StdioContentConflict(t *testing.T) {
	seqs := []int64{4, 7}
	apiErr := &cli.APIError{Code: "update_conflict", Message: "TASK-2 changed since you read it (seq 4 → 7)."}
	uc := &cli.UpdateConflictDetails{
		Ref: "TASK-2", ExpectedSeq: &seqs[0], ActualSeq: &seqs[1], ConflictType: "seq",
		ContentWrite: true, ChangedAt: "2026-10-11T07:00:00Z", ChangedByKind: "agent",
	}
	var stderr bytes.Buffer
	cli.WriteUpdateConflictError(&stderr, apiErr, uc)
	res := extractStructuredCLIError(stderr.String())
	if res == nil {
		t.Fatal("stdio classifier did not recognise the marker")
	}
	env := res.StructuredContent.(ErrorEnvelope)
	if env.Error.Hint != UpdateConflictContentHint {
		t.Fatalf("stdio hint = %q", env.Error.Hint)
	}
	var d map[string]any
	_ = json.Unmarshal(env.Error.Details, &d)
	if d["content_write"] != true || d["changed_at"] != "2026-10-11T07:00:00Z" || d["changed_by_kind"] != "agent" {
		t.Fatalf("stdio details = %v", d)
	}
	if !strings.Contains(stderr.String(), "apply your edit to the current body") {
		t.Fatalf("human lines lack the merge instruction:\n%s", stderr.String())
	}
}

func TestPLAN3555_CLIAndMCPAgreeOnContentConflictHint(t *testing.T) {
	if cli.UpdateConflictContentHint != UpdateConflictContentHint {
		t.Errorf("hints differ:\n cli %q\n mcp %q", cli.UpdateConflictContentHint, UpdateConflictContentHint)
	}
}
