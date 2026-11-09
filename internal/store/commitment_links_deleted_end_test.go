package store

import (
	"errors"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// A link whose parent or child item is soft-deleted (TASK-4418). Deleting the
// link must work; changing it, and any cascade through it, must not reach the
// deleted end. The items keep their links across a restore.

func (f *linkFix) softDelete(t *testing.T, i int) {
	t.Helper()
	if err := f.s.DeleteItem(f.items[i].ID); err != nil {
		t.Fatal(err)
	}
}

func (f *linkFix) onlyLinkID(t *testing.T) string {
	t.Helper()
	var id string
	if err := f.s.db.QueryRow(f.s.q(`SELECT id FROM schedule_links`)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// schedStamp is everything a cascade could write to one schedule and its managed reminder.
func (f *linkFix) schedStamp(t *testing.T, i int) string {
	t.Helper()
	var nextAt, over, upd, rem *string
	if err := f.s.db.QueryRow(f.s.q(`SELECT next_at, next_override_at, updated_at, (SELECT COALESCE(remind_at,'') FROM item_reminders WHERE schedule_id=item_schedules.id) FROM item_schedules WHERE id=?`), f.scheds[i].ID).Scan(&nextAt, &over, &upd, &rem); err != nil {
		t.Fatal(err)
	}
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	return str(nextAt) + "|" + str(over) + "|" + str(upd) + "|" + str(rem) + "|" +
		string(rune('0'+f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=?`, f.scheds[i].ID))) +
		string(rune('0'+f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides WHERE schedule_id=?`, f.scheds[i].ID)))
}

func TestDeletingALinkWhoseEndIsSoftDeletedSucceeds(t *testing.T) {
	t.Parallel()
	for name, deleted := range map[string][]int{"child": {1}, "parent": {0}, "both": {0, 1}} {
		deleted := deleted
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newLinkFix(t, 3)
			f.link(t, 0, 1, "target", "target", oneDay)
			f.link(t, 1, 2, "target", "target", oneDay)
			for _, i := range deleted {
				f.softDelete(t, i)
			}
			var id string
			if err := f.s.db.QueryRow(f.s.q(`SELECT id FROM schedule_links WHERE parent_schedule_id=?`), f.scheds[0].ID).Scan(&id); err != nil {
				t.Fatal(err)
			}
			if err := f.s.DeleteScheduleLink(f.ws.ID, id, f.allEdit()); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM schedule_links`); n != 1 {
				t.Fatalf("%d links left, want only the other one", n)
			}
		})
	}
}

func TestUpdatingALinkWithASoftDeletedEndIsNotFoundLikeAnUnreadableEnd(t *testing.T) {
	t.Parallel()
	for name, dead := range map[string]int{"child": 1, "parent": 0} {
		dead := dead
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newLinkFix(t, 2)
			f.link(t, 0, 1, "target", "target", oneDay)
			id := f.onlyLinkID(t)
			f.softDelete(t, dead)
			upd := models.ScheduleLinkUpdate{Offset: &models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: 2 * 86400}}
			_, errDeleted := f.s.UpdateScheduleLink(f.ws.ID, id, upd, f.allEdit())
			unreadable := f.allEdit()
			unreadable.Readable[f.items[dead].ID] = false
			_, errUnreadable := f.s.UpdateScheduleLink(f.ws.ID, id, upd, unreadable)
			if !errors.Is(errDeleted, ErrScheduleLinkNotFound) || errDeleted != errUnreadable {
				t.Fatalf("deleted end: %v, unreadable end: %v", errDeleted, errUnreadable)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM schedule_links WHERE offset_elapsed=86400`); n != 1 {
				t.Fatal("the refused update wrote")
			}
		})
	}
}

func TestAListStillShowsALinkWithASoftDeletedEnd(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.link(t, 0, 1, "target", "target", oneDay)
	f.softDelete(t, 1)
	got, err := f.s.ListScheduleLinks(f.ws.ID, f.items[0].ID, f.allEdit())
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestARestoreKeepsTheLinksAndMakesThemWritableAgain(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.link(t, 0, 1, "target", "target", oneDay)
	id := f.onlyLinkID(t)
	f.softDelete(t, 1)
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_links`); n != 1 {
		t.Fatalf("soft delete left %d links, want it to keep the one", n)
	}
	if _, err := f.s.RestoreItem(f.items[1].ID); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_links`); n != 1 {
		t.Fatalf("restore left %d links", n)
	}
	upd := models.ScheduleLinkUpdate{CancelPolicy: sp(models.LinkCancelChildOccurrence)}
	if _, err := f.s.UpdateScheduleLink(f.ws.ID, id, upd, f.allEdit()); err != nil {
		t.Fatalf("update after restore: %v", err)
	}
}

// Delete takes the authority a live link does, and answers an end the caller
// cannot read exactly as it answers a link that is not there.
func TestDeletingALinkWithASoftDeletedEndKeepsTheAuthorityAndLeaksNothing(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.link(t, 0, 1, "target", "target", oneDay)
	id := f.onlyLinkID(t)
	f.softDelete(t, 1)
	missing := f.s.DeleteScheduleLink(f.ws.ID, "no-such-link", f.allEdit())
	if !errors.Is(missing, ErrScheduleLinkNotFound) {
		t.Fatalf("missing: %v", missing)
	}
	none := models.CommitmentAccess{Readable: map[string]bool{}, Editable: map[string]bool{}}
	one := f.access() // readable everywhere, editable nowhere
	one.Readable[f.items[1].ID] = false
	readOnly := f.access()
	for name, acc := range map[string]models.CommitmentAccess{
		"reads neither end":     none,
		"reads only the parent": one,
		"reads only the child":  {Readable: map[string]bool{f.items[1].ID: true}, Editable: map[string]bool{f.items[1].ID: true}},
	} {
		err := f.s.DeleteScheduleLink(f.ws.ID, id, acc)
		if err != missing || err.Error() != missing.Error() {
			t.Fatalf("%s: %v, want the very error of a missing link: %v", name, err, missing)
		}
	}
	// Reads both ends but may edit only the live one: the live link's refusal.
	readOnly.Editable[f.items[0].ID] = true
	if err := f.s.DeleteScheduleLink(f.ws.ID, id, readOnly); !errors.Is(err, ErrLinkEditRequired) {
		t.Fatalf("edit on one end: %v", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_links`); n != 1 {
		t.Fatalf("a refused delete removed the link (%d left)", n)
	}
	if err := f.s.DeleteScheduleLink(f.ws.ID, id, f.allEdit()); err != nil {
		t.Fatal(err)
	}
}

// A soft-deleted child takes no part in a cascade: its schedule, reminder,
// overrides and journal stay as they were, and the delete of its link works.
func TestACascadeLeavesASoftDeletedChildUntouchedAndItsLinkCanBeDeleted(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 4)
	f.link(t, 0, 1, "target", "target", oneDay)
	f.link(t, 0, 2, "target", "target", oneDay)
	f.link(t, 2, 3, "target", "target", oneDay) // below the live child
	var deadLink string
	if err := f.s.db.QueryRow(f.s.q(`SELECT id FROM schedule_links WHERE child_schedule_id=?`), f.scheds[1].ID).Scan(&deadLink); err != nil {
		t.Fatal(err)
	}
	f.softDelete(t, 1)
	before := f.schedStamp(t, 1)
	res, err := f.move(0, "target", "2030-03-10T09:00:00Z", nil)
	if err != nil || len(res.Changes) != 3 {
		t.Fatalf("cascade: %+v %v", res, err)
	}
	if got := f.schedStamp(t, 1); got != before {
		t.Fatalf("the soft-deleted child changed:\n%s\n%s", before, got)
	}
	if got := f.role(t, 3, "target"); got != "2030-03-12T09:00:00Z" {
		t.Fatalf("the live descendant=%s", got)
	}
	if err := f.s.DeleteScheduleLink(f.ws.ID, deadLink, f.allEdit()); err != nil {
		t.Fatalf("delete of the dead end's link: %v", err)
	}
	if f.schedStamp(t, 1) != before {
		t.Fatal("deleting the link touched the soft-deleted schedule")
	}
}

// With one pooled connection a read made inside the transaction would wait on
// the connection the transaction holds. The delete reads no terminal state, and
// the update loads it before Begin; both must finish on a soft-deleted end.
func TestLinkWritesWithASoftDeletedEndFinishOnOneConnection(t *testing.T) {
	f := newLinkFix(t, 3)
	f.link(t, 0, 1, "target", "target", oneDay)
	f.link(t, 0, 2, "target", "target", oneDay)
	var dead string
	if err := f.s.db.QueryRow(f.s.q(`SELECT id FROM schedule_links WHERE child_schedule_id=?`), f.scheds[1].ID).Scan(&dead); err != nil {
		t.Fatal(err)
	}
	var live string
	if err := f.s.db.QueryRow(f.s.q(`SELECT id FROM schedule_links WHERE child_schedule_id=?`), f.scheds[2].ID).Scan(&live); err != nil {
		t.Fatal(err)
	}
	f.softDelete(t, 1)
	f.s.db.SetMaxOpenConns(1)
	done := make(chan error, 1)
	go func() {
		upd := models.ScheduleLinkUpdate{Offset: &models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: 2 * 86400}}
		if _, err := f.s.UpdateScheduleLink(f.ws.ID, dead, upd, f.allEdit()); !errors.Is(err, ErrScheduleLinkNotFound) {
			done <- err
			return
		}
		if _, err := f.s.UpdateScheduleLink(f.ws.ID, live, upd, f.allEdit()); err != nil {
			done <- err
			return
		}
		done <- f.s.DeleteScheduleLink(f.ws.ID, dead, f.allEdit())
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a link write hung on a single connection")
	}
}
