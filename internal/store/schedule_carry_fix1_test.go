package store

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4104 D2 r1 fixes: the import's schedule loop and comment map.

func (f *schedFixture) bundleWith(t *testing.T, titles ...string) (*models.WorkspaceExport, map[string]string) {
	t.Helper()
	for _, title := range titles {
		f.another(t, title)
	}
	data := f.exportBundle(t)
	ids := map[string]string{}
	for _, it := range data.Items {
		ids[it.Title] = it.ID
	}
	return data, ids
}

// r1-2: an anchor_wall whose all-day form differs from dtstart's is dropped.
func TestImportDropsAnchorWallAllDayMismatch(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	data, ids := f.bundleWith(t, "flip")
	s := validSchedExport(ids["flip"])
	s.AnchorWall = "2026-11-02"
	data.Schedules = append(data.Schedules, s)
	_, rep := f.importBundle(t, data, "Flip")
	if len(rep.DroppedSchedules) != 1 || rep.DroppedSchedules[0].Reason != "anchor_wall:combination" {
		t.Fatalf("dropped %+v, want one anchor_wall:combination", rep.DroppedSchedules)
	}
}

// HQ: a schedule dropped for its own reason must not make the item's next
// schedule read as a duplicate.
func TestImportSecondScheduleAfterDroppedFirst(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	data, ids := f.bundleWith(t, "twice")
	bad := validSchedExport(ids["twice"])
	bad.Rrule = "FREQ=HOURLY;BYSETPOS=1"
	data.Schedules = append(data.Schedules, bad, validSchedExport(ids["twice"]))
	dst, rep := f.importBundle(t, data, "Twice")
	if len(rep.DroppedSchedules) != 1 || strings.Contains(rep.DroppedSchedules[0].Reason, "duplicate") {
		t.Fatalf("dropped %+v, want only the first, for its own reason", rep.DroppedSchedules)
	}
	if sc, _ := f.s.GetSchedule(dst.ID, f.itemByTitle(t, dst.ID, "twice").ID); sc == nil {
		t.Error("the valid second schedule was not imported")
	}
}

// r1-3: an empty comment id binds nothing, and a comment_id naming another
// item's comment reads as no comment.
func TestImportCommentMapGuards(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, commentID, exportedID string }{
		{"empty id", "", ""},
		{"cross item", "c-other", "c-other"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newSchedFixture(t)
			data, ids := f.bundleWith(t, "mine", "other")
			owner := ids["other"]
			data.Comments = append(data.Comments, models.CommentExport{ID: c.exportedID, ItemID: owner, Author: "a", Body: "stray", CreatedBy: "user", Source: "web",
				CreatedAt: "2026-10-29T12:00:00Z", UpdatedAt: "2026-10-29T12:00:00Z"})
			s := validSchedExport(ids["mine"])
			s.Occurrences = []models.ScheduleOccurrenceExport{{OccurrenceAt: "2026-11-01T09:00:00Z", OccurrenceWall: "2026-11-01T09:00:00", State: "done",
				CommentID: c.commentID, ResolvedAt: "2026-11-01T09:01:00Z"}}
			data.Schedules = append(data.Schedules, s)
			dst, rep := f.importBundle(t, data, "Guards")
			if len(rep.DroppedSchedules) != 0 {
				t.Fatalf("dropped %+v", rep.DroppedSchedules)
			}
			sc, _ := f.s.GetSchedule(dst.ID, f.itemByTitle(t, dst.ID, "mine").ID)
			if sc == nil {
				t.Fatal("no schedule")
			}
			got := f.occs(t, sc.ID)
			if len(got) != 1 || got[0].Comment != "" {
				t.Errorf("occurrences %+v, want one with no comment", got)
			}
		})
	}
}
