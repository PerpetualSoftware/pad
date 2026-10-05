package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3406: every store write that reads its row back did so on the pool
// after its commit, where a deletion landing in between (an account
// deletion, a revoke, a cascade) made it answer (nil, nil) for a write that
// succeeded: BUG-3405's shape, one caller away from a panic. Each now reads
// back inside its transaction. The hook deletes the row right after each
// commit, so the race is deterministic: every write must still answer the
// row it committed.
//
// Not parallel: it sets the package-level hook.
func TestTask3406_ReadBackSurvivesADeletionAfterTheCommit(t *testing.T) {
	s := testStore(t)
	u := tabsUser(t, s, "T3406")
	ws := tabsWorkspace(t, s, u, "T3406WS")
	coll := createTestCollection(t, s, ws.ID, "T3406C")
	item := createTestItem(t, s, ws.ID, coll.ID, "T3406 item", "")
	other := createTestItem(t, s, ws.ID, coll.ID, "T3406 other", "")
	parent := createTestItem(t, s, ws.ID, coll.ID, "T3406 parent", "")

	exec := func(t *testing.T, q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(s.q(q), args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// What the hook deletes, per kind.
	kill := map[string]func(t *testing.T, id string){
		"collection": func(t *testing.T, id string) {
			exec(t, `UPDATE collections SET deleted_at = ? WHERE id = ?`, now(), id)
		},
		"comment":   func(t *testing.T, id string) { exec(t, `DELETE FROM comments WHERE id = ?`, id) },
		"document":  func(t *testing.T, id string) { exec(t, `DELETE FROM documents WHERE id = ?`, id) },
		"item_link": func(t *testing.T, id string) { exec(t, `DELETE FROM item_links WHERE id = ?`, id) },
		"reaction": func(t *testing.T, commentID string) {
			exec(t, `DELETE FROM comment_reactions WHERE comment_id = ?`, commentID)
		},
		"webhook": func(t *testing.T, id string) { exec(t, `DELETE FROM webhooks WHERE id = ?`, id) },
		"user": func(t *testing.T, id string) {
			if err := s.DeleteAccountAtomic(id); err != nil {
				t.Fatalf("delete account: %v", err)
			}
		},
	}
	arm := func(t *testing.T, want string) *int {
		fired := 0
		readbackAfterCommitHook = func(kind, id string) {
			if kind != want {
				return
			}
			fired++
			kill[kind](t, id)
		}
		t.Cleanup(func() { readbackAfterCommitHook = nil })
		return &fired
	}
	comment := func(t *testing.T, body string) *models.Comment {
		c, err := s.CreateComment(ws.ID, item.ID, u.ID, models.CommentCreate{Body: body, Author: "U", CreatedBy: "user"})
		if err != nil || c == nil {
			t.Fatalf("seed comment: %v", err)
		}
		return c
	}
	doc := func(t *testing.T, title string) *models.Document {
		d, err := s.CreateDocument(ws.ID, models.DocumentCreate{Title: title})
		if err != nil || d == nil {
			t.Fatalf("seed document: %v", err)
		}
		return d
	}

	cases := []struct {
		name, kind string
		run        func(t *testing.T, arm func()) (any, bool) // arm() just before the call under test; the result, and whether it is the committed row
	}{
		{"CreateCollection", "collection", func(t *testing.T, arm func()) (any, bool) {
			arm()
			c, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "T3406 New", Slug: "t3406-new"})
			return c, err == nil && c != nil && c.Slug == "t3406-new"
		}},
		{"UpdateCollection", "collection", func(t *testing.T, arm func()) (any, bool) {
			c, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "T3406 Up", Slug: "t3406-up"})
			if err != nil {
				t.Fatal(err)
			}
			name := "T3406 Renamed"
			arm()
			got, err := s.UpdateCollection(c.ID, models.CollectionUpdate{Name: &name})
			return got, err == nil && got != nil && got.Name == name
		}},
		{"CreateComment", "comment", func(t *testing.T, arm func()) (any, bool) {
			arm()
			c, err := s.CreateComment(ws.ID, item.ID, u.ID, models.CommentCreate{Body: "created", Author: "U", CreatedBy: "user"})
			return c, err == nil && c != nil && c.Body == "created"
		}},
		{"CreateCommentWithActivity", "comment", func(t *testing.T, arm func()) (any, bool) {
			arm()
			c, err := s.CreateCommentWithActivity(ws.ID, item.ID, u.ID, models.Activity{WorkspaceID: ws.ID, DocumentID: item.ID, Action: "commented", Actor: "user"},
				models.CommentCreate{Body: "with activity", Author: "U", CreatedBy: "user"})
			return c, err == nil && c != nil && c.Body == "with activity"
		}},
		{"UpdateComment", "comment", func(t *testing.T, arm func()) (any, bool) {
			c := comment(t, "before")
			arm()
			got, err := s.UpdateComment(c.ID, "after")
			return got, err == nil && got != nil && got.Body == "after"
		}},
		{"CreateDocument", "document", func(t *testing.T, arm func()) (any, bool) {
			arm()
			d, err := s.CreateDocument(ws.ID, models.DocumentCreate{Title: "T3406 doc"})
			return d, err == nil && d != nil && d.Title == "T3406 doc"
		}},
		{"UpdateDocument", "document", func(t *testing.T, arm func()) (any, bool) {
			d := doc(t, "T3406 doc up")
			title := "T3406 doc updated"
			arm()
			got, err := s.UpdateDocument(d.ID, models.DocumentUpdate{Title: &title})
			return got, err == nil && got != nil && got.Title == title
		}},
		{"RestoreDocument", "document", func(t *testing.T, arm func()) (any, bool) {
			d := doc(t, "T3406 doc restore")
			if err := s.DeleteDocument(d.ID); err != nil {
				t.Fatal(err)
			}
			arm()
			got, err := s.RestoreDocument(ws.ID, d.ID)
			return got, err == nil && got != nil && got.ID == d.ID
		}},
		{"ConsumeEmailVerification", "user", func(t *testing.T, arm func()) (any, bool) {
			v := createTestUser(t, s, "t3406-verify@example.com", "Verify", "correct-horse-battery")
			tok, err := s.CreateEmailVerification(v.ID)
			if err != nil {
				t.Fatal(err)
			}
			arm()
			got, err := s.ConsumeEmailVerification(tok)
			return got, err == nil && got != nil && got.ID == v.ID
		}},
		{"CreateItemLink", "item_link", func(t *testing.T, arm func()) (any, bool) {
			arm()
			l, err := s.CreateItemLink(ws.ID, models.ItemLinkCreate{TargetID: other.ID, LinkType: "blocks"}, item.ID)
			return l, err == nil && l != nil && l.TargetID == other.ID
		}},
		{"SetParentLink", "item_link", func(t *testing.T, arm func()) (any, bool) {
			arm()
			l, err := s.SetParentLink(ws.ID, other.ID, parent.ID, "user")
			return l, err == nil && l != nil && l.TargetID == parent.ID
		}},
		{"AddReaction", "reaction", func(t *testing.T, arm func()) (any, bool) {
			c := comment(t, "react to me")
			arm()
			r, err := s.AddReaction(c.ID, u.ID, "user", "👍")
			return r, err == nil && r != nil && r.CommentID == c.ID && r.Emoji == "👍"
		}},
		{"CreateWebhook", "webhook", func(t *testing.T, arm func()) (any, bool) {
			arm()
			w, err := s.CreateWebhook(ws.ID, models.WebhookCreate{URL: "https://hooks.example/t3406"})
			return w, err == nil && w != nil && w.URL == "https://hooks.example/t3406"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var fired *int
			got, ok := tc.run(t, func() { fired = arm(t, tc.kind) })
			if fired == nil {
				t.Fatal("the case never armed the hook")
			}
			if *fired == 0 {
				t.Fatalf("the after-commit hook never fired for %s: the race was not exercised", tc.kind)
			}
			if !ok {
				t.Fatalf("%s answered %#v after its row was deleted post-commit; want the row it committed", tc.name, got)
			}
		})
	}
}
