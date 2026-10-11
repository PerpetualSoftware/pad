package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// PLAN-3578 step 1: the no-room patch.

type patchFixture struct {
	srv  *Server
	slug string
	item models.Item
	path string
}

func newPatchFixture(t *testing.T, srv *Server, body string) *patchFixture {
	t.Helper()
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Patched", `{"status":"open"}`)
	f := &patchFixture{srv: srv, slug: slug, item: item, path: "/api/v1/workspaces/" + slug + "/items/" + item.Slug}
	if rr := doRequest(srv, "PATCH", f.path, map[string]any{"content": body, "replace": true}); rr.Code != http.StatusOK {
		t.Fatalf("seed body: %d %s", rr.Code, rr.Body.String())
	}
	return f
}

func (f *patchFixture) current(t *testing.T) *models.Item {
	t.Helper()
	it, err := f.srv.store.GetItem(f.item.ID)
	if err != nil || it == nil {
		t.Fatalf("GetItem: %v", err)
	}
	return it
}

func (f *patchFixture) patch(t *testing.T, body map[string]any) (int, map[string]any) {
	t.Helper()
	rr := doRequest(f.srv, "POST", f.path+"/patch", body)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func edits(pairs ...string) []map[string]any {
	var out []map[string]any
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, map[string]any{"old": pairs[i], "new": pairs[i+1]})
	}
	return out
}

func TestPLAN3578_PatchAppliesAndReturnsTheDiff(t *testing.T) {
	f := newPatchFixture(t, testServer(t), "alpha\nbeta\ngamma\n")
	before := f.current(t)
	code, out := f.patch(t, map[string]any{"edits": edits("beta", "BETA"), "expected_seq": before.Seq})
	if code != http.StatusOK {
		t.Fatalf("patch: %d %v", code, out)
	}
	if got := f.current(t).Content; got != "alpha\nBETA\ngamma\n" {
		t.Fatalf("body = %q", got)
	}
	d, _ := out["diff"].(string)
	if !strings.Contains(d, "-beta\n") || !strings.Contains(d, "+BETA\n") {
		t.Fatalf("diff = %q", d)
	}
	if out["rebased"] != false || out["intervening"] != nil {
		t.Fatalf("an unmoved seq reported a rebase: %v", out)
	}
	if v, _ := out["version_id"].(string); v == "" {
		t.Fatalf("a patch must write a version (ForceVersion): %v", out)
	}
	if int64(out["seq"].(float64)) != f.current(t).Seq {
		t.Fatalf("seq in the answer is not the row's")
	}
}

func TestPLAN3578_PatchRefusals(t *testing.T) {
	f := newPatchFixture(t, testServer(t), "one\ntwo two\n")
	body := f.current(t).Content
	for _, c := range []struct {
		name   string
		edits  []map[string]any
		status int
		code   string
	}{
		{"missing anchor", edits("three", "3"), http.StatusConflict, "patch_anchor_missing"},
		{"ambiguous anchor", edits("two", "2"), http.StatusConflict, "patch_anchor_ambiguous"},
		{"empty anchor", edits("", "x"), http.StatusBadRequest, "bad_request"},
		{"later edit fails, earlier one not applied", edits("one", "1", "nope", "x"), http.StatusConflict, "patch_anchor_missing"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out := f.patch(t, map[string]any{"edits": c.edits})
			e, _ := out["error"].(map[string]any)
			if code != c.status || e["code"] != c.code {
				t.Fatalf("got %d %v", code, out)
			}
			if got := f.current(t).Content; got != body {
				t.Fatalf("a refused patch changed the body: %q", got)
			}
		})
	}
	code, _ := f.patch(t, map[string]any{"edits": []map[string]any{}})
	if code != http.StatusBadRequest {
		t.Fatalf("no edits: %d", code)
	}
}

// A moved seq is applied over when the anchors still match, and reported:
// the intervening write, stamped in the chain, and complete.
func TestPLAN3578_RebaseReportsTheInterveningWrites(t *testing.T) {
	f := newPatchFixture(t, testServer(t), "alpha\nbeta\n")
	read := f.current(t)
	// Someone else edits another part of the body, through the PATCH door.
	if rr := doRequestWithHeaders(f.srv, "PATCH", f.path, map[string]any{"content": "alpha\nbeta\nomega\n", "replace": true}, map[string]string{"X-Pad-Agent": "other"}); rr.Code != http.StatusOK {
		t.Fatalf("intervening write: %d %s", rr.Code, rr.Body.String())
	}
	mid := f.current(t)
	code, out := f.patch(t, map[string]any{"edits": edits("alpha", "ALPHA"), "expected_seq": read.Seq})
	if code != http.StatusOK || out["rebased"] != true {
		t.Fatalf("rebase: %d %v", code, out)
	}
	if got := f.current(t).Content; got != "ALPHA\nbeta\nomega\n" {
		t.Fatalf("the rebase lost the other write: %q", got)
	}
	iv, _ := out["intervening"].(map[string]any)
	changes, _ := iv["changes"].([]any)
	if iv["complete"] != true || len(changes) != 1 {
		t.Fatalf("intervening = %v", iv)
	}
	c := changes[0].(map[string]any)
	if int64(c["seq"].(float64)) != mid.Seq || int64(c["prev_seq"].(float64)) != read.Seq || c["actor"] != "agent" {
		t.Fatalf("change = %v (read %d, mid %d)", c, read.Seq, mid.Seq)
	}
}

// A write that recorded no stamped entry breaks the chain: complete is false.
func TestPLAN3578_UnstampedWriteBreaksTheChain(t *testing.T) {
	f := newPatchFixture(t, testServer(t), "alpha\nbeta\n")
	read := f.current(t)
	// A store write with no handler records no activity entry at all.
	if _, err := f.srv.store.UpdateItem(f.item.ID, models.ItemUpdate{FieldsPatch: map[string]interface{}{"status": "in-progress"}}); err != nil {
		t.Fatal(err)
	}
	code, out := f.patch(t, map[string]any{"edits": edits("beta", "B"), "expected_seq": read.Seq})
	if code != http.StatusOK || out["rebased"] != true {
		t.Fatalf("patch: %d %v", code, out)
	}
	iv, _ := out["intervening"].(map[string]any)
	if iv["complete"] != false {
		t.Fatalf("a write with no stamped entry read as complete: %v", iv)
	}
}

func TestPLAN3578_PatchRefusedWhileAnEditorIsOpen(t *testing.T) {
	srv := testServerWithCollab(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	f := newPatchFixture(t, srv, "alpha\n")
	conn, resp, err := dialCollab(t, ts.URL, f.item.ID, nil, "")
	if err != nil {
		status := ""
		if resp != nil {
			status = resp.Status
		}
		t.Fatalf("dialCollab: %v (%s)", err, status)
	}
	stop := applierEcho(t, conn)
	t.Cleanup(stop)
	waitForApplierPath(t, srv, f.slug, f.item.Slug, f.item.ID)
	body := f.current(t).Content
	code, out := f.patch(t, map[string]any{"edits": edits("alpha", "A")})
	e, _ := out["error"].(map[string]any)
	if code != http.StatusConflict || e["code"] != "patch_room_open" {
		t.Fatalf("got %d %v", code, out)
	}
	if got := f.current(t).Content; got != body {
		t.Fatalf("a refused patch changed the body: %q", got)
	}
}

func TestPLAN3578_PatchRefusedOverUnflushedEdits(t *testing.T) {
	srv := testServerWithCollab(t)
	f := newPatchFixture(t, srv, "alpha\n")
	if _, err := srv.store.AppendYjsUpdate(f.item.ID, []byte{0x00, 0x02, 0x04, 0x01, 0x9A, 0x7C, 0x00}, "1"); err != nil {
		t.Fatal(err)
	}
	if f.current(t).ContentState != models.ContentOutcomeAppliedPendingFlush {
		t.Fatal("premise: the item must read pending")
	}
	code, out := f.patch(t, map[string]any{"edits": edits("alpha", "A")})
	e, _ := out["error"].(map[string]any)
	if code != http.StatusConflict || e["code"] != "content_pending_flush" {
		t.Fatalf("got %d %v", code, out)
	}
}

// The seq-chain and patch keys are bookkeeping: an agent's body-only patch
// renders its version card, never an empty activity card.
func TestPLAN3578_PatchLeavesNoEmptyTimelineCard(t *testing.T) {
	srv := testServer(t)
	token, ws, slug := debounceFixture(t, srv)
	rr := authedAgentRequest(t, srv, token, "wren", "PATCH", "/api/v1/workspaces/"+ws+"/items/"+slug,
		map[string]any{"content": "alpha\nbeta\n", "replace": true})
	if rr.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rr.Code, rr.Body.String())
	}
	rr = authedAgentRequest(t, srv, token, "wren", "POST", "/api/v1/workspaces/"+ws+"/items/"+slug+"/patch",
		map[string]any{"edits": edits("beta", "BETA")})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rr.Code, rr.Body.String())
	}
	for _, e := range updatedActivityEntries(fetchTimelineAuthed(t, srv, token, ws, slug).Entries) {
		if changesOf(t, e.Activity.Metadata) == "" {
			t.Fatalf("an updated entry with no change rendered: metadata %s", e.Activity.Metadata)
		}
	}
}
