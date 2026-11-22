package store_test

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-4701: an imported comment's created_at and updated_at are normalized to
// the RFC3339 UTC the store writes. An unreadable stamp never drops the
// comment: it takes the other stamp's value, and only a comment with neither
// readable gets the import time.
func TestTask4701ImportNormalizesCommentStamps(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, s *store.Store) {
		wsID, _, item := seedStaleItem(t, s)
		for _, body := range []string{"comment A", "comment B", "comment C", "comment D"} {
			if _, err := s.CreateComment(wsID, item.ID, "", models.CommentCreate{Author: "tester", Body: body}); err != nil {
				t.Fatalf("CreateComment %s: %v", body, err)
			}
		}
		exp, err := s.ExportWorkspace("cs")
		if err != nil {
			t.Fatalf("ExportWorkspace: %v", err)
		}
		seen := 0
		for i := range exp.Comments {
			cm := &exp.Comments[i]
			switch cm.Body {
			case "comment A":
				cm.CreatedAt = "2026-02-30T10:00:00Z" // impossible date
				cm.UpdatedAt = "2026-03-03T07:30:00Z"
			case "comment B":
				cm.CreatedAt = "2026-03-01 09:15:00"
				cm.UpdatedAt = "2026-03-01 09:20:00"
			case "comment C":
				cm.CreatedAt = "2026-03-02T08:00:00+02:00"
				cm.UpdatedAt = "not-a-time"
			case "comment D":
				cm.CreatedAt = ""
				cm.UpdatedAt = "2026-13-01T00:00:00Z"
			default:
				continue
			}
			seen++
		}
		if seen != 4 {
			t.Fatalf("bundle carries %d of the 4 seeded comments", seen)
		}

		before := time.Now().UTC().Truncate(time.Second)
		if _, err := s.ImportWorkspace(exp, "Imported stamps", "", "test"); err != nil {
			t.Fatalf("ImportWorkspace: %v", err)
		}
		after := time.Now().UTC()

		// No placeholders, so the query reads the same on both dialects. Only
		// the import writes imported = 1.
		rows, err := s.DB().Query(`SELECT body, created_at, updated_at FROM comments WHERE imported = 1`)
		if err != nil {
			t.Fatalf("query imported comments: %v", err)
		}
		defer rows.Close()
		type stamps struct{ created, updated string }
		got := map[string]stamps{}
		for rows.Next() {
			var body, created, updated string
			if err := rows.Scan(&body, &created, &updated); err != nil {
				t.Fatal(err)
			}
			got[body] = stamps{created, updated}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(got) != 4 {
			t.Fatalf("imported %d comments (%v), want all 4", len(got), got)
		}

		for body, want := range map[string]stamps{
			"comment A": {"2026-03-03T07:30:00Z", "2026-03-03T07:30:00Z"},
			"comment B": {"2026-03-01T09:15:00Z", "2026-03-01T09:20:00Z"},
			"comment C": {"2026-03-02T06:00:00Z", "2026-03-02T06:00:00Z"},
		} {
			if st := got[body]; st != want {
				t.Errorf("%s stamps = %+v, want %+v", body, st, want)
			}
		}

		d := got["comment D"]
		if d.updated != d.created {
			t.Errorf("comment D updated_at %q, want it equal to created_at %q", d.updated, d.created)
		}
		if ts, err := time.Parse(time.RFC3339, d.created); err != nil || ts.Before(before) || ts.After(after) {
			t.Errorf("comment D created_at %q, want an RFC3339 import time in [%s, %s]", d.created, before.Format(time.RFC3339), after.Format(time.RFC3339))
		}
	})
}
