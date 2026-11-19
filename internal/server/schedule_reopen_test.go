package server

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCommitmentReopenHTTP(t *testing.T) {
	f := ackHTTPFixture(t)
	token := f.token(f.owner, "")
	path := f.path(f.item) + "/reopen"
	for _, b := range []string{`null`, `[]`, `{"resolution_id":1}`, `{"resolution_id":"x","resolution_id":"y"}`, `{"resolution_id":"x","force":true}`, `{"resolution_id":"\u0000"}`} {
		f.expectErr(f.as(token, "POST", path, b), 400, "invalid_body", "")
	}
	for _, tc := range []struct{ body, code string }{{`{}`, "required"}, {`{"resolution_id":"not-a-uuid"}`, "format"}} {
		d := f.expectErr(f.as(token, "POST", path, tc.body), 400, "validation_error", tc.code)
		if d["field"] != "resolution_id" {
			t.Fatal(d)
		}
	}
	rr := f.resolve(map[string]any{"state": "done", "occurrence_at": resolveX, "end_schedule": true})
	if rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	id := readReopenID(t, f)
	body := map[string]any{"resolution_id": id}
	viewer := f.member("reopen-viewer@example.com", "viewer")
	if rr = f.as(f.token(viewer, ""), "POST", path, body); rr.Code != 403 {
		t.Fatalf("viewer=%d %s", rr.Code, rr.Body.String())
	}
	guest := f.member("reopen-hidden@example.com", "editor")
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, guest.ID, "specific", []string{}); err != nil {
		t.Fatal(err)
	}
	hidden := f.as(f.token(guest, ""), "POST", path, body)
	missing := f.as(f.token(guest, ""), "POST", strings.Replace(path, f.item.Slug, "NOPE-99999", 1), body)
	if hidden.Code != 404 || missing.Code != 404 || hidden.Body.String() != missing.Body.String() {
		t.Fatalf("404 oracle: %s %s", hidden.Body.String(), missing.Body.String())
	}
	f.expectErr(f.as(token, "POST", path, map[string]any{"resolution_id": "11111111-1111-4111-8111-111111111111"}), 409, "reopen_conflict", "")
	editor := f.member("reopen-editor@example.com", "editor")
	rr = f.as(f.token(editor, ""), "POST", path, body)
	if rr.Code != 200 {
		t.Fatalf("reopen=%d %s", rr.Code, rr.Body.String())
	}
	m := schedMap(t, rr)
	if m["commitment"] == nil || m["reopen"].(map[string]any)["resolution_id"] != id {
		t.Fatal(m)
	}
	f.expectErr(f.as(token, "POST", path, body), 409, "reopen_conflict", "")
}

func TestCommitmentReopenHistoryAndSSE(t *testing.T) {
	f := newSchedHTTPOn(t, testServerWithEvents(t))
	f.create(map[string]any{"roles": map[string]any{}, "dtstart": "2099-01-01T09:00", "tz": "UTC"})
	rr := f.resolve(map[string]any{"state": "done", "occurrence_at": resolveX, "end_schedule": true, "comment": "Kept"})
	if rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	id := readReopenID(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, _, _ := f.srv.events.Subscribe(ctx, f.ws.ID)
	path := f.path(f.item) + "/reopen"
	body := map[string]any{"resolution_id": id}
	rr = f.as(f.token(f.owner, ""), "POST", path, body)
	if rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	if f.count(`SELECT COUNT(*) FROM activities WHERE document_id=? AND action='schedule_reopened'`, f.item.ID) != 1 || f.count(`SELECT COUNT(*) FROM schedule_changes WHERE item_id=? AND change_type='reopen'`, f.item.ID) != 1 {
		t.Fatal("missing or duplicate history")
	}
	select {
	case ev := <-ch:
		if ev.Type != "item_updated" {
			t.Fatalf("event=%+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("missing refresh")
	}
	f.expectErr(f.as(f.token(f.owner, ""), "POST", path, body), 409, "reopen_conflict", "")
	select {
	case ev := <-ch:
		t.Fatalf("extra event=%+v", ev)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestCommitmentReopenUppercaseID(t *testing.T) {
	f := ackHTTPFixture(t)
	rr := f.resolve(map[string]any{"state": "done", "occurrence_at": resolveX, "end_schedule": true})
	if rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	id := readReopenID(t, f)
	upper := strings.ToUpper(id)
	if upper == id {
		t.Fatalf("id %q has no lowercase hex to uppercase", id)
	}
	rr = f.as(f.token(f.owner, ""), "POST", f.path(f.item)+"/reopen", map[string]any{"resolution_id": upper})
	if rr.Code != 200 {
		t.Fatalf("reopen=%d %s", rr.Code, rr.Body.String())
	}
	m := schedMap(t, rr)
	if m["commitment"] == nil || m["reopen"].(map[string]any)["resolution_id"] != id {
		t.Fatal(m)
	}
}
