package store

import (
	"encoding/json"
	"errors"
	"testing"
)

// TASK-3408 (U10b): app DTOs come from the block and the table's snapshot
// keys only.
func TestTask3408_AppEventDTOs(t *testing.T) {
	block := `"app_projection":{"v":1,"collection_id":"c1","creator":{"user_id":"u1","display":"Ann","kind":"user","via_app":"inst-1"},"fields":{"status":"open"}}`
	item := `{"id":"i1","workspace_id":"w1","slug":"s","title":"T","content":"C","created_at":"a","updated_at":"b",` + block + `}`
	decode := func(t *testing.T, b []byte) map[string]any {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	b, coll, err := BuildAppEventDTO("item.created", "e1", "t0", "ws-test", []byte(item))
	if err != nil || coll != "c1" {
		t.Fatalf("created: %v %q", err, coll)
	}
	m := decode(t, b)
	if m["via_app"] != "inst-1" || m["title"] != "T" || m["content"] != "C" || m["item_id"] != "i1" || m["id"] != "e1" || m["occurred_at"] != "t0" {
		t.Fatalf("created body %s", b)
	}
	// The install's workspace is the caller's to supply (BUG-3416); the
	// snapshot's own keys never pass through.
	if m["workspace_id"] != "ws-test" {
		t.Fatalf("created body workspace_id = %v, want the one passed in", m["workspace_id"])
	}
	for _, k := range []string{"slug", "app_projection"} {
		if _, ok := m[k]; ok {
			t.Fatalf("created body carries %q", k)
		}
	}

	// Not a create: the creator's install is not the writer, so no via_app.
	b, _, _ = BuildAppEventDTO("item.updated", "e2", "t0", "ws-test", []byte(item))
	if m := decode(t, b); m["via_app"] != nil {
		t.Fatalf("item.updated carries an envelope via_app: %s", b)
	} else if c, _ := m["creator"].(map[string]any); c["via_app"] != "inst-1" {
		t.Fatalf("item.updated lost the creator's via_app: %s", b)
	}

	// Partial: ref-only.
	partial := `{"id":"i1","title":"T","content":"C","app_projection":{"v":1,"collection_id":"c1","creator":{},"partial":true}}`
	b, _, err = BuildAppEventDTO("item.updated", "e3", "t0", "ws-test", []byte(partial))
	if err != nil {
		t.Fatal(err)
	}
	if m := decode(t, b); m["partial"] != true || m["title"] != nil || m["content"] != nil || m["fields"] != nil {
		t.Fatalf("partial body %s", b)
	}

	// item.deleted: identifiers only (the envelope's workspace_id is one,
	// BUG-3416).
	b, _, _ = BuildAppEventDTO("item.deleted", "e4", "t0", "ws-test", []byte(item))
	if m := decode(t, b); len(m) != 6 || m["item_id"] != "i1" || m["collection_id"] != "c1" || m["workspace_id"] != "ws-test" {
		t.Fatalf("deleted body %s", b)
	}

	// comment.deleted from the ref-only payload.
	cdel := `{"id":"cm1","workspace_id":"w1","item_id":"i1","app_projection":{"v":1,"collection_id":"c1","creator":{},"item_id":"i1","parent_comment_id":"cm0"}}`
	b, coll, err = BuildAppEventDTO("comment.deleted", "e5", "t0", "ws-test", []byte(cdel))
	if err != nil || coll != "c1" {
		t.Fatal(err)
	}
	if m := decode(t, b); m["comment_id"] != "cm1" || m["parent_comment_id"] != "cm0" || m["body"] != nil {
		t.Fatalf("comment.deleted body %s", b)
	}

	// No block, or not an app event.
	for _, tc := range []struct{ ev, p string }{
		{"item.created", `{"id":"i1","title":"T"}`},
		{"comment.created", `{"id":"cm1","body":"x"}`},
		{"item.bulk_updated", item},
	} {
		if _, _, err := BuildAppEventDTO(tc.ev, "e", "t", "ws-test", []byte(tc.p)); !errors.Is(err, ErrNoAppProjection) {
			t.Fatalf("%s %s: got %v, want ErrNoAppProjection", tc.ev, tc.p, err)
		}
	}
}
