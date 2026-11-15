package store

import (
	"errors"
	"reflect"
	"testing"
)

func TestReopenSupersededIDRefused(t *testing.T) {
	f := newSchedFixture(t)
	item, sc := f.mk(t, dailyUTC, "2026-10-29T00:00:00Z")
	f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user"}, "2026-10-30T10:00:00Z")
	view, err := f.s.GetCommitment(f.ws.ID, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	id := view.LastResolution.ResolutionID
	sc = f.reload(t, item.ID)
	f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "skip", Actor: "user"}, "2026-10-30T10:01:00Z")
	before := reopenState(t, f.s)
	_, err = f.s.reopenOccurrenceAt(f.ws.ID, item.ID, ReopenInput{ResolutionID: id}, rt("2026-10-30T10:02:00Z"))
	var refusal *ReopenError
	if !errors.As(err, &refusal) || refusal.Code != "reopen_conflict" {
		t.Fatalf("%v", err)
	}
	if !reflect.DeepEqual(before, reopenState(t, f.s)) {
		t.Fatal("refusal changed state")
	}
}

func TestReopenDeletedCommentOmitted(t *testing.T) {
	// A comment with replies is tombstoned (deleted_at set, row kept), not
	// removed, and must read as deleted too.
	for _, mode := range []string{"hard", "tombstone"} {
		t.Run(mode, func(t *testing.T) {
			f := newSchedFixture(t)
			item, sc := f.mk(t, dailyUTC, "2026-10-29T00:00:00Z")
			u := f.user(t, "comment@example.com", "UTC")
			r := f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user", Comment: f.comment(item, u.ID, "Delete me")}, "2026-10-30T10:00:00Z")
			view, err := f.s.GetCommitment(f.ws.ID, item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "hard" {
				f.exec(t, `DELETE FROM comments WHERE id=?`, r.Comment.ID)
			} else {
				f.exec(t, `UPDATE comments SET deleted_at=?, body='' WHERE id=?`, "2026-10-30T10:00:30Z", r.Comment.ID)
			}
			got, err := f.s.reopenOccurrenceAt(f.ws.ID, item.ID, ReopenInput{ResolutionID: view.LastResolution.ResolutionID, Actor: "user"}, rt("2026-10-30T10:01:00Z"))
			if err != nil {
				t.Fatal(err)
			}
			if got.CommentID != "" {
				t.Fatal("deleted comment reported as kept")
			}
		})
	}
}

func TestReopenDependencyPrecedesCheckpointChecks(t *testing.T) {
	for _, checkpoint := range []string{"none", "available", "consumed", "conflict", "unsupported", "bad version"} {
		t.Run(checkpoint, func(t *testing.T) {
			f := newSchedFixture(t)
			item, sc := f.mk(t, dailyUTC, "2026-10-29T00:00:00Z")
			other := f.another(t, "blocker")
			id := newID()
			if checkpoint != "none" {
				f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user"}, "2026-10-30T10:00:00Z")
				view, err := f.s.GetCommitment(f.ws.ID, item.ID)
				if err != nil {
					t.Fatal(err)
				}
				id = view.LastResolution.ResolutionID
			}
			switch checkpoint {
			case "consumed":
				f.exec(t, `UPDATE schedule_reopen_checkpoints SET reopened_change_id='consumed' WHERE schedule_id=?`, sc.ID)
			case "conflict":
				f.exec(t, `UPDATE schedule_reopen_checkpoints SET blocked_code='reopen_conflict' WHERE schedule_id=?`, sc.ID)
			case "unsupported":
				f.exec(t, `UPDATE schedule_reopen_checkpoints SET blocked_code='reopen_not_supported' WHERE schedule_id=?`, sc.ID)
			case "bad version":
				f.exec(t, `UPDATE schedule_reopen_checkpoints SET snapshot_version=2 WHERE schedule_id=?`, sc.ID)
			}
			f.exec(t, `INSERT INTO schedule_gates(id,workspace_id,blocked_item_id,blocker_item_id,created_at) VALUES(?,?,?,?,?)`, newID(), f.ws.ID, item.ID, other.ID, "x")
			want := "reopen_dependency"
			if checkpoint == "none" {
				want = "reopen_unavailable"
			}
			before := reopenState(t, f.s)
			for _, requested := range []string{id, newID()} {
				_, err := f.s.reopenOccurrenceAt(f.ws.ID, item.ID, ReopenInput{ResolutionID: requested}, rt("2026-10-30T10:01:00Z"))
				var refusal *ReopenError
				if !errors.As(err, &refusal) || refusal.Code != want {
					t.Errorf("got %v want %s", err, want)
				}
			}
			view, err := f.s.GetCommitment(f.ws.ID, item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if view.LastResolution.ReopenCode != want {
				t.Errorf("GET %s want %s", view.LastResolution.ReopenCode, want)
			}
			if !reflect.DeepEqual(before, reopenState(t, f.s)) {
				t.Fatal("refusal changed state")
			}
		})
	}
}
