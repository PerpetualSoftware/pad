package store

import (
	"github.com/PerpetualSoftware/pad/internal/models"
	"testing"
)

func TestTASK4423_R05Notices(t *testing.T) {
	f := newLinkFix(t, 3)
	recipient := notifyUser(t, f.s, "graph-recipient@example.com")
	if err := f.s.AddWorkspaceMember(f.ws.ID, recipient.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	addDevice(t, f.s, recipient.ID)
	for _, it := range f.items {
		f.exec(t, `UPDATE items SET assigned_user_id=? WHERE id=?`, recipient.ID, it.ID)
	}
	f.linkRaw(t, 0, 1, "target", "target", oneDay)
	f.linkRaw(t, 0, 2, "target", "target", oneDay)
	if _, err := f.move(0, "target", "2030-04-01T09:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE kind='commitment_moved' AND user_id=?`, recipient.ID); n != 1 {
		t.Fatalf("move notices=%d", n)
	}
}
func TestTASK4423_R05History(t *testing.T) {
	f := newLinkFix(t, 2)
	f.linkRaw(t, 0, 1, "target", "target", oneDay)
	if _, err := f.move(0, "target", "2030-04-01T09:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	c, err := f.s.GetCommitmentFor(f.ws.ID, f.items[1].ID, func(id string) bool { return id == f.items[1].ID })
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Journal) == 0 {
		t.Fatal("dependent move missing from History")
	}
	for _, ch := range c.Journal {
		if ch.RootChangeID != "" || ch.ParentChangeID != nil {
			t.Fatal("hidden lineage exposed")
		}
	}
}
func TestTASK4423_R11SkipAuthority(t *testing.T) {
	for _, mode := range []string{"parent-only", "child-edit", "no-deadline", "nil"} {
		t.Run(mode, func(t *testing.T) {
			f := newLinkFix(t, 2)
			f.cancelLink(t, 0, 1, models.LinkCancelChildOccurrence)
			if mode != "no-deadline" {
				f.deadline(t, 1, "stays_owed")
			}
			acc := f.access(0)
			if mode == "child-edit" {
				acc = f.allEdit()
			}
			in := ResolveInput{OccurrenceAt: rt(*f.reload(t, f.items[0].ID).NextAt), State: "skip", Actor: "user", MaySkipBeforeDeadline: true}
			if mode != "nil" {
				in.LinkAccess = acc
			}
			if _, err := f.s.ResolveOccurrence(f.ws.ID, f.items[0].ID, in); err != nil {
				t.Fatal(err)
			}
			want := mode == "child-edit" || mode == "no-deadline"
			if got := f.noLongerNeeded(t, 1); got != want {
				t.Fatalf("marked=%v want %v", got, want)
			}
			if !want {
				f.eval(t, farFuture, 0)
				if f.misses(t, 1) != 1 {
					t.Fatal("protected deadline did not record miss")
				}
			}
		})
	}
}
