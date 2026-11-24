package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// A floating snooze or move named from another zone stores the same state as
// the same resolution named in the recipient's zone. The recipient is in Los
// Angeles, so the 2nd's 09:00 is 17:00Z; a Tokyo caller reads it as 00:00Z.

type floatingResolveState struct {
	NextAt, NextWall, Override, Remind string
	Occs                               []occTuple
}

func (f *schedFixture) floatingResolveState(t *testing.T, item *models.Item) floatingResolveState {
	t.Helper()
	sc, err := f.s.GetSchedule(f.ws.ID, item.ID)
	if err != nil || sc == nil {
		t.Fatalf("GetSchedule: %v %v", sc, err)
	}
	st := floatingResolveState{Occs: f.occs(t, sc.ID)}
	if sc.NextAt != nil {
		st.NextAt = *sc.NextAt
	}
	if sc.NextWall != nil {
		st.NextWall = *sc.NextWall
	}
	if sc.NextOverrideAt != nil {
		st.Override = *sc.NextOverrideAt
	}
	if r := f.managed(t, sc); r != nil {
		st.Remind = r.RemindAt
	}
	for i := range st.Occs {
		st.Occs[i].ResolvedAt = "" // the two runs share a clock, but keep the row shape only
	}
	return st
}

func TestFloatingSnoozeOrMoveNamedFromAnotherZoneStoresTheRecipientReading(t *testing.T) {
	t.Parallel()
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	const recipientAt, callerAt = "2026-11-02T17:00:00Z", "2026-11-02T00:00:00Z"
	cases := []struct {
		name, state, until, now string
	}{
		{"snooze ahead", "snooze", "2026-11-03T12:00:00Z", "2026-11-01T12:00:00Z"},
		{"move ahead", "move", "2026-11-03T12:00:00Z", "2026-11-01T12:00:00Z"},
		// Past for the caller's reading, ahead for the recipient's.
		{"move into the past", "move", "2026-11-02T09:00:00Z", "2026-11-02T10:00:00Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newSchedFixture(t)
			la := f.user(t, "f4446-"+c.state+"-"+c.now[8:10]+"@example.com", "America/Los_Angeles")
			run := func(at string, view *time.Location) floatingResolveState {
				item, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Floating: true, Rrule: "FREQ=DAILY"}, "2026-10-29T12:00:00Z")
				f.assign(t, item.ID, la.ID)
				until := rt(c.until)
				f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt(at), State: c.state, Until: &until, View: view, Actor: "user"}, c.now)
				return f.floatingResolveState(t, item)
			}
			want := run(recipientAt, nil)
			got := run(callerAt, tokyo)
			if got.NextAt != want.NextAt || got.NextWall != want.NextWall || got.Override != want.Override || got.Remind != want.Remind {
				t.Errorf("schedule from Tokyo = %+v\nfrom the recipient = %+v", got, want)
			}
			if len(got.Occs) != len(want.Occs) {
				t.Fatalf("history from Tokyo = %+v\nfrom the recipient = %+v", got.Occs, want.Occs)
			}
			for i := range got.Occs {
				if got.Occs[i] != want.Occs[i] {
					t.Errorf("history row %d from Tokyo = %+v, from the recipient = %+v", i, got.Occs[i], want.Occs[i])
				}
			}
		})
	}
}
