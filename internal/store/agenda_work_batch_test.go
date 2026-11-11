package store

import (
	"database/sql"
	"fmt"
	"github.com/PerpetualSoftware/pad/internal/models"
	"testing"
)

type agendaQueryCounter struct {
	Queryer
	queries, singles int
}

func (q *agendaQueryCounter) Query(query string, args ...any) (*sql.Rows, error) {
	q.queries++
	return q.Queryer.Query(query, args...)
}
func (q *agendaQueryCounter) QueryRow(query string, args ...any) *sql.Row {
	q.singles++
	return q.Queryer.QueryRow(query, args...)
}

func TestAgendaWorkDetailsBatch(t *testing.T) {
	f := newSchedFixture(t)
	// Assert the batching seam at runtime so the baseline fails behaviourally,
	// even before the helper exists; the counter covers both SQL read methods.
	batch, ok := any(f.s).(interface {
		agendaWorkDetailsQ(Queryer, string, []string) (map[string]*models.ItemSchedule, map[string]bool, error)
	})
	if !ok {
		t.Fatal("agenda work has no batched schedule/gate reader")
	}
	var ids []string
	for n := 0; n < 30; n++ {
		ids = append(ids, createTestItem(t, f.s, f.ws.ID, f.item.CollectionID, fmt.Sprintf("Unscheduled %d", n), "").ID)
	}
	q := &agendaQueryCounter{Queryer: f.s.db}
	schedules, gated, err := batch.agendaWorkDetailsQ(q, f.ws.ID, ids)
	if err != nil || len(schedules) != 0 || len(gated) != 0 || q.queries != 2 || q.singles != 0 {
		t.Fatalf("ungated unscheduled: schedules=%d gates=%d query=%d single=%d err=%v", len(schedules), len(gated), q.queries, q.singles, err)
	}
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-10T09:00", Tz: "UTC"})
	if _, err := f.s.CreateScheduleGate(f.ws.ID, ids[0], f.item.ID, models.CommitmentAccess{Readable: map[string]bool{ids[0]: true, f.item.ID: true}, Editable: map[string]bool{ids[0]: true, f.item.ID: true}}); err != nil {
		t.Fatal(err)
	}
	ids = append(ids, f.item.ID)
	q.queries, q.singles = 0, 0
	schedules, gated, err = batch.agendaWorkDetailsQ(q, f.ws.ID, ids)
	if err != nil || len(schedules) != 1 || schedules[f.item.ID].ID != sc.ID || len(gated) != 1 || !gated[ids[0]] || q.queries != 2 || q.singles != 0 {
		t.Fatalf("mixed batch: schedules=%v gated=%v query=%d single=%d err=%v", schedules, gated, q.queries, q.singles, err)
	}
	q.queries, q.singles = 0, 0
	schedules, gated, err = batch.agendaWorkDetailsQ(q, "other-workspace", ids)
	if err != nil || len(schedules)+len(gated) != 0 {
		t.Fatalf("workspace isolation: %v %v %v", schedules, gated, err)
	}
	q.queries, q.singles = 0, 0
	_, _, err = batch.agendaWorkDetailsQ(q, f.ws.ID, nil)
	if err != nil || q.queries+q.singles != 0 {
		t.Fatalf("empty batch queried: %+v err=%v", q, err)
	}
}
