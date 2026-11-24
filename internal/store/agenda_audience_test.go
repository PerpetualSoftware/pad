package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestListAgendaWorkAudienceAll(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	u, err := f.s.CreateUser(models.UserCreate{Email: "audience-all@example.com", Name: "Audience All", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	userID := u.ID
	if _, err := f.s.db.Exec(f.s.q(`UPDATE items SET assigned_user_id = ? WHERE id = ?`), userID, f.item.ID); err != nil {
		t.Fatal(err)
	}
	f.create(t, models.ScheduleCreate{
		Kind:     "check",
		Dtstart:  "2031-03-10T09:00",
		Tz:       "UTC",
		Audience: "agent",
	})

	humanRows, _, err := f.s.ListAgendaWork(f.ws.ID, PendingReminderScope{}, userID, "human", 100, true)
	if err != nil {
		t.Fatalf("ListAgendaWork(human): %v", err)
	}
	for _, c := range humanRows {
		if c.ItemID == f.item.ID {
			t.Fatalf("agent-only scheduled item returned for audience=human: %+v", c)
		}
	}

	allRows, _, err := f.s.ListAgendaWork(f.ws.ID, PendingReminderScope{}, userID, "all", 100, true)
	if err != nil {
		t.Fatalf("ListAgendaWork(all): %v", err)
	}
	found := false
	for _, c := range allRows {
		if c.ItemID == f.item.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("agent-only scheduled item missing for audience=all; got %+v", allRows)
	}
}
