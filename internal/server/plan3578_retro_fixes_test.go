package server

import (
	"net/http"
	"strings"
	"testing"
)

// Retro CONVE-735 findings on #2055 and #2058.

// A no-op edit, or edits that cancel out, used to report success and advance
// seq with no version behind it. They are refused and write nothing.
func TestPLAN3578_NoOpPatchIsRefused(t *testing.T) {
	t.Parallel()
	f := newPatchFixture(t, testServer(t), "alpha\nbeta\n")
	before := f.current(t)
	for name, e := range map[string][]map[string]any{
		"one edit, old == new": edits("beta", "beta"),
		"two edits cancel out": edits("beta", "BETA", "BETA", "beta"),
	} {
		code, out := f.patch(t, map[string]any{"edits": e})
		if code != http.StatusBadRequest {
			t.Fatalf("%s: %d %v", name, code, out)
		}
	}
	if after := f.current(t); after.Seq != before.Seq || after.Content != before.Content {
		t.Fatalf("a refused no-op wrote: seq %d → %d", before.Seq, after.Seq)
	}
}

// Deleting a line's only content left the undo with no anchor; it is
// undoable now. A patch that empties the WHOLE body still cannot be undone
// by anchor, and says so (History restores it).
func TestPLAN3578_UndoOfAnEmptiedLine(t *testing.T) {
	t.Parallel()
	f := newPatchFixture(t, testServer(t), "alpha\nbeta\ngamma\n")
	code, out := f.patch(t, map[string]any{"edits": edits("beta", "")})
	if code != http.StatusOK {
		t.Fatalf("patch: %d %v", code, out)
	}
	code, out = f.patch(t, map[string]any{"undo": out["patch_id"]})
	if code != http.StatusOK || f.current(t).Content != "alpha\nbeta\ngamma\n" {
		t.Fatalf("undo: %d %v, body %q", code, out, f.current(t).Content)
	}

	g := newPatchFixture(t, testServer(t), "only")
	code, out = g.patch(t, map[string]any{"edits": edits("only", "")})
	if code != http.StatusOK {
		t.Fatalf("emptying patch: %d %v", code, out)
	}
	code, out = g.patch(t, map[string]any{"undo": out["patch_id"]})
	msg, _ := out["error"].(map[string]any)["message"].(string)
	if code != http.StatusBadRequest || !strings.Contains(msg, "history") {
		t.Fatalf("undo of a whole-body clear: %d %v", code, out)
	}
}
