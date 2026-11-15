package store

import (
	"slices"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestListSchedulesForAgendaWorkspaceWithoutSchedules(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	_, scheduled := f.mk(t, dailyFromOct, "2026-10-01T00:00:00Z")
	wsB := createTestWorkspace(t, f.s, "Without schedules")
	colB := createTestCollection(t, f.s, wsB.ID, "Tasks")
	createTestItem(t, f.s, wsB.ID, colB.ID, "Unscheduled", "")
	from, to := rt("2026-10-01T00:00:00Z"), rt("2026-10-08T00:00:00Z")

	got, truncated, err := f.s.ListSchedulesForAgenda(wsB.ID, PendingReminderScope{}, from, to, 100)
	if err != nil || truncated || len(got) != 0 {
		t.Fatalf("workspace B: rows=%d truncated=%v err=%v, want no rows", len(got), truncated, err)
	}
	got, truncated, err = f.s.ListSchedulesForAgenda(f.ws.ID, PendingReminderScope{}, from, to, 100)
	if err != nil || truncated {
		t.Fatalf("workspace A: truncated=%v err=%v", truncated, err)
	}
	assertAgendaScheduleIDs(t, got, []string{scheduled.ID})
}

func TestListSchedulesForAgendaItemFilterRows(t *testing.T) {
	t.Parallel()
	w := newCalWorld(t)
	open := w.sched(t, nil, "open", false)
	w.sched(t, nil, "done", false)
	doneWithHistory := w.sched(t, nil, "done", true)
	deleted := w.sched(t, nil, "open", false)
	if err := w.s.DeleteItem(deleted.ID); err != nil {
		t.Fatal(err)
	}
	outsideScope := w.sched(t, w.custom, "open", false)
	from, to := rt("2026-10-01T00:00:00Z"), rt("2026-10-08T00:00:00Z")

	for _, tc := range []struct {
		name  string
		scope PendingReminderScope
		want  []*models.Item
	}{
		{"unrestricted", PendingReminderScope{}, []*models.Item{open, doneWithHistory, outsideScope}},
		{"collection scope", PendingReminderScope{CollectionIDs: []string{w.item.CollectionID}}, []*models.Item{open, doneWithHistory}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var want []string
			for _, item := range tc.want {
				sc, err := w.s.GetSchedule(w.ws.ID, item.ID)
				if err != nil || sc == nil {
					t.Fatalf("GetSchedule(%s): schedule=%v err=%v", item.ID, sc, err)
				}
				want = append(want, sc.ID)
			}
			slices.Sort(want)
			got, truncated, err := w.s.ListSchedulesForAgenda(w.ws.ID, tc.scope, from, to, 100)
			if err != nil || truncated {
				t.Fatalf("agenda: truncated=%v err=%v", truncated, err)
			}
			assertAgendaScheduleIDs(t, got, want)
		})
	}
}

func TestListSchedulesForAgendaLimitTruncates(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	var want []string
	for i := 0; i < 3; i++ {
		_, sc := f.mk(t, dailyFromOct, "2026-10-01T00:00:00Z")
		want = append(want, sc.ID)
	}
	slices.Sort(want)
	got, truncated, err := f.s.ListSchedulesForAgenda(f.ws.ID, PendingReminderScope{}, rt("2026-10-01T00:00:00Z"), rt("2026-10-08T00:00:00Z"), 2)
	if err != nil || !truncated {
		t.Fatalf("agenda limit 2: truncated=%v err=%v, want truncated", truncated, err)
	}
	assertAgendaScheduleIDs(t, got, want[:2])
}

func assertAgendaScheduleIDs(t *testing.T, rows []AgendaSchedule, want []string) {
	t.Helper()
	var got []string
	for _, row := range rows {
		got = append(got, row.Schedule.ID)
	}
	if !slices.Equal(got, want) {
		t.Errorf("schedule IDs = %v, want %v in order", got, want)
	}
}
