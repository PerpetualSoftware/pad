package models

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TASK-2695: `edited` is derived from the timestamps and emitted on every
// serialised comment, nested replies included.
func TestCommentEditedMarker(t *testing.T) {
	created := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	fresh := Comment{ID: "c1", CreatedAt: created, UpdatedAt: created}
	edited := Comment{ID: "c2", CreatedAt: created, UpdatedAt: created.Add(time.Second)}

	if fresh.IsEdited() {
		t.Error("a comment whose timestamps match is not edited")
	}
	if !edited.IsEdited() {
		t.Error("a comment updated after creation is edited")
	}
	if (Comment{}).IsEdited() {
		t.Error("a zero comment is not edited")
	}

	fresh.Replies = []Comment{edited}
	b, err := json.Marshal(fresh)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, `"edited":false`) || !strings.Contains(got, `"edited":true`) {
		t.Fatalf("want edited on the comment and on its reply, got %s", got)
	}
	if !strings.Contains(got, `"id":"c1"`) || !strings.Contains(got, `"replies":[`) {
		t.Fatalf("the ordinary fields must still serialise: %s", got)
	}

	var back Comment
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("a serialised comment must still decode: %v", err)
	}
	if back.ID != "c1" || len(back.Replies) != 1 {
		t.Fatalf("round trip lost fields: %+v", back)
	}
}
