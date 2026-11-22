package store_test

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-4701: an imported comment's created_at and updated_at are normalized to
// the RFC3339 UTC the store writes. A comment whose created_at is not an
// instant is skipped, not fatal; an unreadable updated_at falls back to the
// normalized created_at.
func TestTask4701ImportNormalizesCommentStamps(t *testing.T) {
	t.Parallel()
	eachBackend(t, func(t *testing.T, s *store.Store) {
		wsID, _, item := seedStaleItem(t, s)
		for _, body := range []string{"comment A", "comment B", "comment C"} {
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
			case "comment B":
				cm.CreatedAt = "2026-03-01 09:15:00"
				cm.UpdatedAt = "2026-03-01 09:20:00"
			case "comment C":
				cm.CreatedAt = "2026-03-02T08:00:00+02:00"
				cm.UpdatedAt = "not-a-time"
			default:
				continue
			}
			seen++
		}
		if seen != 3 {
			t.Fatalf("bundle carries %d of the 3 seeded comments", seen)
		}

		if _, err := s.ImportWorkspace(exp, "Imported stamps", "", "test"); err != nil {
			t.Fatalf("ImportWorkspace: %v", err)
		}

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

		if st, ok := got["comment A"]; ok {
			t.Errorf("comment A (impossible created_at) imported with %+v, want it skipped", st)
		}
		if st, want := got["comment B"], (stamps{"2026-03-01T09:15:00Z", "2026-03-01T09:20:00Z"}); st != want {
			t.Errorf("comment B stamps = %+v, want %+v", st, want)
		}
		if st, want := got["comment C"], (stamps{"2026-03-02T06:00:00Z", "2026-03-02T06:00:00Z"}); st != want {
			t.Errorf("comment C stamps = %+v, want %+v", st, want)
		}
	})
}
