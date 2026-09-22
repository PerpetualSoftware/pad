package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3133 at the MCP doors. The server refusal is pinned in
// internal/server; these pin that an agent on either transport receives it as
// its own structured code with guidance that does not send it round a
// re-read loop, and that overwrite_pending_edits reaches the server.

// pendingEditFrameMCP is a well-formed, content-bearing sync update.
var pendingEditFrameMCP = []byte{0x00, 0x02, 0x04, 0x01, 0x9A, 0x7C, 0x00}

func TestMCPUpdate_ContentPendingFlushRefusalAndOverride(t *testing.T) {
	f := newParentFixture(t)
	if _, err := f.store.AppendYjsUpdate(f.child.ID, pendingEditFrameMCP, "1"); err != nil {
		t.Fatalf("AppendYjsUpdate: %v", err)
	}
	cur, err := f.store.GetItem(f.child.ID)
	if err != nil {
		t.Fatal(err)
	}
	// PREMISE: the item reads pending, or the refusal leg measures nothing.
	if cur.ContentState != models.ContentOutcomeAppliedPendingFlush {
		t.Fatalf("premise: item must read pending; got %q", cur.ContentState)
	}

	d := &HTTPHandlerDispatcher{
		Handler:      f.srv,
		UserResolver: func(context.Context) *models.User { return f.owner },
	}
	input := map[string]any{
		"workspace":    f.workspace.Slug,
		"ref":          f.child.Slug,
		"content":      "replacement body",
		"expected_seq": float64(cur.Seq),
	}
	res, err := d.Dispatch(WithDispatchInput(context.Background(), input), []string{"item", "update"}, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	env, ok := res.StructuredContent.(ErrorEnvelope)
	if !res.IsError || !ok {
		t.Fatalf("want a structured refusal, got IsError=%v %T: %s", res.IsError, res.StructuredContent, textOf(res))
	}
	if env.Error.Code != ErrContentPendingFlush {
		t.Fatalf("code = %q, want %q (an unlisted code collapses to generic conflict)", env.Error.Code, ErrContentPendingFlush)
	}
	if env.Error.Hint != ContentPendingFlushHint {
		t.Errorf("hint = %q, want the content_pending_flush hint, not the re-read-and-retry one", env.Error.Hint)
	}
	var details map[string]any
	if err := json.Unmarshal(env.Error.Details, &details); err != nil {
		t.Fatalf("details: %v", err)
	}
	if details["pending_rows"] != float64(1) {
		t.Errorf("details.pending_rows = %v, want 1", details["pending_rows"])
	}

	// The override must reach the server: the same call with it set succeeds.
	input["overwrite_pending_edits"] = true
	f.dispatch(t, []string{"item", "update"}, input)
	got, err := f.store.GetItem(f.child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "replacement body" {
		t.Errorf("content = %q; the override did not reach the server", got.Content)
	}
}

// Stdio half, through the REAL CLI writer: the bytes the CLI puts on stderr are
// the bytes the classifier parses, and the guidance matches the HTTP path's.
func TestStdioSurfacesContentPendingFlush(t *testing.T) {
	apiErr := &cli.APIError{
		Code:    cli.ContentPendingFlushCode,
		Message: "TASK-2 has unflushed collaborative edits",
		Details: json.RawMessage(`{"ref":"TASK-2","pending_rows":3}`),
	}
	var stderr bytes.Buffer
	cli.WriteContentPendingFlushError(&stderr, apiErr)

	res := extractStructuredCLIError(stderr.String())
	if res == nil {
		t.Fatal("stdio classifier did not recognise the CLI's marker line — an agent would receive server_error")
	}
	env, ok := res.StructuredContent.(ErrorEnvelope)
	if !ok {
		t.Fatalf("expected ErrorEnvelope, got %T", res.StructuredContent)
	}
	if env.Error.Code != ErrContentPendingFlush {
		t.Fatalf("code = %q, want %q", env.Error.Code, ErrContentPendingFlush)
	}
	if env.Error.Hint != ContentPendingFlushHint {
		t.Errorf("stdio hint = %q, want the HTTP path's %q", env.Error.Hint, ContentPendingFlushHint)
	}
}

func TestCLIAndMCPAgreeOnContentPendingFlush(t *testing.T) {
	if cli.ContentPendingFlushCode != string(ErrContentPendingFlush) {
		t.Errorf("code strings differ: cli %q, mcp %q", cli.ContentPendingFlushCode, ErrContentPendingFlush)
	}
	if cli.ContentPendingFlushHint != ContentPendingFlushHint {
		t.Errorf("hints differ:\n cli %q\n mcp %q", cli.ContentPendingFlushHint, ContentPendingFlushHint)
	}
}
