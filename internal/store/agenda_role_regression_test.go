package store

import (
	"github.com/PerpetualSoftware/pad/internal/models"
	"testing"
	"time"
)

func TestAgendaOutstandingLeavesMissedCountingToConsumers(t *testing.T) {
	f := newSchedFixture(t)
	_, sc := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, "2031-03-01T00:00:00Z")
	e, err := f.s.AgendaOutstanding(sc, time.UTC, rt("2031-03-10T12:00:00Z"))
	if err != nil || e == nil {
		t.Fatalf("outstanding: %v %v", e, err)
	}
	if e.MissedCount != 0 || e.Stale {
		t.Fatalf("lightweight read walked missed occurrences: %+v", e)
	}
	if e.Roles.Target == nil || !e.Overdue {
		t.Fatalf("outstanding placement lost: %+v", e)
	}
}

func TestAgendaCorruptOverrideKeyIsUnavailable(t *testing.T) {
	for _, key := range []string{"broken-wall", "broken-instant|2031-03-20T09:00:00"} {
		t.Run(key, func(t *testing.T) {
			f := newSchedFixture(t)
			_, sc := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-20T09:00", Tz: "UTC"}, "2031-03-01T00:00:00Z")
			_, err := f.s.db.Exec(f.s.q(`INSERT INTO schedule_occurrence_overrides(id,schedule_id,occurrence_key,role,value_at,created_at,updated_at) VALUES(?,?,?,'target','2031-03-10T09:00:00Z',?,?)`), "corrupt", sc.ID, key, "2031-03-01T00:00:00Z", "2031-03-01T00:00:00Z")
			if err != nil {
				t.Fatal(err)
			}
			x, err := f.s.ExpandAgendaCut(sc, time.UTC, rt("2031-03-10T00:00:00Z"), rt("2031-03-11T00:00:00Z"), rt("2031-03-10T12:00:00Z"), time.Now().Add(time.Minute), time.Now)
			if err != nil || x.Unavailable != "internal" {
				t.Fatalf("corrupt key silently lost: %+v err=%v", x, err)
			}
		})
	}
}
