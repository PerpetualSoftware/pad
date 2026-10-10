package mcp

import (
	"encoding/json"
	"testing"
)

// PLAN-3535: pad_collection's tracks_work reaches the API as the typed member
// over the remote transport, false included (the case a bool flag would drop).
func TestPLAN3535_CollectionMappersCarryTracksWork(t *testing.T) {
	for _, v := range []bool{false, true} {
		_, _, body, err := mapCollectionUpdate(map[string]any{"workspace": "w", "slug": "docs", "tracks_work": v})
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		_ = json.Unmarshal(body, &got)
		if got["tracks_work"] != v {
			t.Fatalf("update body %s, want tracks_work=%v", body, v)
		}
		_, _, body, err = mapCollectionCreate(map[string]any{"workspace": "w", "name": "Refs", "tracks_work": v})
		if err != nil {
			t.Fatal(err)
		}
		got = nil
		_ = json.Unmarshal(body, &got)
		if got["tracks_work"] != v {
			t.Fatalf("create body %s, want tracks_work=%v", body, v)
		}
	}
	// Omitted: not sent, so the server keeps (update) or defaults (create).
	_, _, body, _ := mapCollectionUpdate(map[string]any{"workspace": "w", "slug": "docs", "name": "D"})
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	if _, has := got["tracks_work"]; has {
		t.Fatalf("an omitted tracks_work was sent: %s", body)
	}
}
