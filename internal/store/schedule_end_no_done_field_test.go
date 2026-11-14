package store

import (
	"errors"
	"reflect"
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

func TestResolveLastOccurrenceDoneFilter(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, schema, settings, status string
		guarded, dismissed             bool
	}{
		{"select terminal", `{"fields":[{"key":"status","type":"select","options":["open","done"]}]}`, `{}`, "open", true, false},
		{"select reference", `{"fields":[{"key":"status","type":"select","options":["draft","published"]}]}`, `{}`, "draft", false, true},
		{"text open", `{"fields":[{"key":"status","type":"text"}]}`, `{}`, "open", true, false},
		{"text closed", `{"fields":[{"key":"status","type":"text"}]}`, `{}`, "done", false, false},
		{"no status", `{"fields":[]}`, `{}`, "open", false, true},
		{"bad settings", `{"fields":[{"key":"status","type":"select","options":["open","done"]}]}`, `{"board_group_by":42}`, "open", true, false},
		{"bad schema open", `{"fields":42}`, `{}`, "open", true, false},
		{"bad schema closed", `{"fields":42}`, `{}`, "done", false, false},
	} {
		for _, state := range []string{"done", "skip"} {
			t.Run(tc.name+"/"+state, func(t *testing.T) {
				f := newSchedFixture(t)
				item, sc := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC"}, "2026-10-29T12:00:00Z")
				f.exec(t, `UPDATE collections SET schema=?, settings=? WHERE id=?`, tc.schema, tc.settings, item.CollectionID)
				f.closeItem(t, item, tc.status)
				in := ResolveInput{OccurrenceAt: f.outstanding(t, item), State: state, Actor: "user"}
				before := f.reload(t, item.ID)
				counts := f.endGuardCounts(t, item, sc)
				res, err := f.resolve(t, item, in, "2026-10-30T09:05:00Z")
				if tc.guarded {
					var end *ErrScheduleWouldEnd
					if !errors.As(err, &end) {
						t.Fatalf("resolve = %v, want ErrScheduleWouldEnd", err)
					}
					if !reflect.DeepEqual(before, f.reload(t, item.ID)) || counts != f.endGuardCounts(t, item, sc) {
						t.Fatal("refusal wrote changes")
					}
					in.EndSchedule = true
					res = f.mustResolve(t, item, in, "2026-10-30T09:05:00Z")
				} else if err != nil {
					t.Fatal(err)
				}
				after := f.reload(t, item.ID)
				if res.EndedItemOpen != tc.guarded || after.EndedAt == nil || (after.EndedDismissedAt != nil) != tc.dismissed {
					t.Fatalf("ending = %+v, item_still_open=%v", after, res.EndedItemOpen)
				}
				if tc.dismissed && *after.EndedAt != *after.EndedDismissedAt {
					t.Fatal("dismissal differs from ending")
				}
			})
		}
	}
}
