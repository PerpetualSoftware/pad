package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestResolveLastOccurrenceWithoutDoneField(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"done", "skip"} {
		t.Run(state, func(t *testing.T) {
			f := newSchedFixture(t)
			f.exec(t, `UPDATE collections SET schema='{"fields":[]}' WHERE id=?`, f.item.CollectionID)
			item, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC"}, "2026-10-29T12:00:00Z")
			res := f.mustResolve(t, item, ResolveInput{OccurrenceAt: f.outstanding(t, item), State: state, Actor: "user"}, "2026-10-30T09:05:00Z")
			sc := f.reload(t, item.ID)
			if res.EndedItemOpen || sc.EndedAt == nil || sc.EndedDismissedAt == nil || *sc.EndedAt != *sc.EndedDismissedAt {
				t.Fatalf("ending = %+v, item_still_open=%v", sc, res.EndedItemOpen)
			}
		})
	}
}
