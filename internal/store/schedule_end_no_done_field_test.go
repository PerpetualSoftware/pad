package store

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/items"
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

func TestResolveLastOccurrenceSelectWithoutOptions(t *testing.T) {
	t.Parallel()
	const schemaJSON = `{"fields":[{"key":"status","type":"select","options":[]}]}`
	var schema models.CollectionSchema
	if err := models.UnmarshalItemFieldSchema([]byte(schemaJSON), &schema); err != nil {
		t.Fatal(err)
	}
	for _, value := range append([]string{"open"}, models.DefaultTerminalStatuses...) {
		if err := items.ValidateFields(map[string]any{"status": value}, schema); err != nil {
			t.Fatalf("select without options must accept %q: %v", value, err)
		}
	}
	for _, state := range []string{"done", "skip"} {
		t.Run(state, func(t *testing.T) {
			f := newSchedFixture(t)
			item, sc := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC"}, "2026-10-29T12:00:00Z")
			f.exec(t, `UPDATE collections SET schema=? WHERE id=?`, schemaJSON, item.CollectionID)
			in := ResolveInput{OccurrenceAt: f.outstanding(t, item), State: state, Actor: "user"}
			before, counts := f.reload(t, item.ID), f.endGuardCounts(t, item, sc)
			_, err := f.resolve(t, item, in, "2026-10-30T09:05:00Z")
			var end *ErrScheduleWouldEnd
			if !errors.As(err, &end) {
				t.Fatalf("resolve = %v, want ErrScheduleWouldEnd", err)
			}
			if end.DoneField != "status" || len(end.CloseOptions) == 0 || end.CloseOptions[0] != "done" {
				t.Fatalf("close options = %+v", end)
			}
			if !reflect.DeepEqual(before, f.reload(t, item.ID)) || counts != f.endGuardCounts(t, item, sc) {
				t.Fatal("refusal wrote changes")
			}
			in.EndSchedule = true
			res := f.mustResolve(t, item, in, "2026-10-30T09:05:00Z")
			if !res.EndedItemOpen || res.Schedule.EndedAt == nil || res.Schedule.EndedDismissedAt != nil {
				t.Fatalf("ending = %+v", res)
			}
		})
	}
}

func TestResolveLastOccurrenceDoneFilter(t *testing.T) {
	t.Parallel()
	defaultOptions := []string{"done", "completed", "resolved", "fixed", "implemented", "archived", "deprecated"}
	for _, tc := range []struct {
		name, schema, settings, status string
		guarded, dismissed             bool
		options                        []string
	}{
		{"select terminal", `{"fields":[{"key":"status","type":"select","options":["open","Done","cancelled"]}]}`, `{}`, "open", true, false, []string{"Done"}},
		{"select abandoning only", `{"fields":[{"key":"status","type":"select","options":["open","Cancelled"]}]}`, `{}`, "open", true, false, nil},
		{"select no terminal", `{"fields":[{"key":"status","type":"select","options":["draft","published"]}]}`, `{}`, "draft", false, true, nil},
		{"select no options", `{"fields":[{"key":"status","type":"select"}]}`, `{}`, "open", true, false, defaultOptions},
		{"select custom terminals no options", `{"fields":[{"key":"status","type":"select","terminal_options":["shipped","dropped"],"abandoned_options":["dropped"]}]}`, `{}`, "open", true, false, []string{"shipped"}},
		{"text open", `{"fields":[{"key":"status","type":"text"}]}`, `{}`, "open", true, false, defaultOptions},
		{"url", `{"fields":[{"key":"status","type":"url"}]}`, `{}`, "open", true, false, defaultOptions},
		{"number", `{"fields":[{"key":"status","type":"number"}]}`, `{}`, "open", false, true, nil},
		{"checkbox", `{"fields":[{"key":"status","type":"checkbox"}]}`, `{}`, "open", false, true, nil},
		{"date", `{"fields":[{"key":"status","type":"date"}]}`, `{}`, "open", false, true, nil},
		{"multi select", `{"fields":[{"key":"status","type":"multi_select","options":["open","done"]}]}`, `{}`, "open", false, true, nil},
		{"relation", `{"fields":[{"key":"status","type":"relation"}]}`, `{}`, "open", false, true, nil},
		{"multi relation", `{"fields":[{"key":"status","type":"multi_relation"}]}`, `{}`, "open", false, true, nil},
		{"json", `{"fields":[{"key":"status","type":"json"}]}`, `{}`, "open", false, true, nil},
		{"unknown type", `{"fields":[{"key":"status","type":"unknown"}]}`, `{}`, "open", false, true, nil},
		{"text closed", `{"fields":[{"key":"status","type":"text"}]}`, `{}`, "done", false, false, defaultOptions},
		{"no status", `{"fields":[]}`, `{}`, "open", false, true, nil},
		{"bad settings", `{"fields":[{"key":"status","type":"select","options":["open","done"]}]}`, `{"board_group_by":42}`, "open", true, false, []string{"done"}},
		{"bad schema open", `{"fields":42}`, `{}`, "open", true, false, defaultOptions},
		{"bad schema closed", `{"fields":42}`, `{}`, "done", false, false, defaultOptions},
	} {
		for _, state := range []string{"done", "skip"} {
			t.Run(tc.name+"/"+state, func(t *testing.T) {
				f := newSchedFixture(t)
				item, sc := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC"}, "2026-10-29T12:00:00Z")
				f.exec(t, `UPDATE collections SET schema=?, settings=? WHERE id=?`, tc.schema, tc.settings, item.CollectionID)
				f.closeItem(t, item, tc.status)
				filter := resolveCollectionDoneFilter(item.CollectionID, tc.schema, tc.settings)
				if got := filter.closeOptions(); !slices.Equal(got, tc.options) {
					t.Errorf("close_options = %v, want %v", got, tc.options)
				}
				candidates, _, err := f.s.ListAgendaWork(f.ws.ID, PendingReminderScope{}, "", "", 100, false)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, candidate := range candidates {
					if candidate.ItemID == item.ID {
						found = true
						if candidate.CanCloseItem != tc.guarded {
							t.Errorf("CanCloseItem = %v, want %v", candidate.CanCloseItem, tc.guarded)
						}
					}
				}
				if found != (tc.status != "done") {
					t.Fatalf("agenda candidate present = %v", found)
				}

				in := ResolveInput{OccurrenceAt: f.outstanding(t, item), State: state, Actor: "user"}
				before := f.reload(t, item.ID)
				counts := f.endGuardCounts(t, item, sc)
				res, err := f.resolve(t, item, in, "2026-10-30T09:05:00Z")
				if tc.guarded {
					var end *ErrScheduleWouldEnd
					if !errors.As(err, &end) {
						t.Fatalf("resolve = %v, want ErrScheduleWouldEnd", err)
					}
					if !slices.Equal(end.CloseOptions, tc.options) {
						t.Errorf("refusal close_options = %v, want %v", end.CloseOptions, tc.options)
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
