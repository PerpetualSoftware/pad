package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4237: the bootstrap agenda's store half. SQLite in make test, Postgres in
// make test-pg.

// bootFixture is a schedule fixture with two accounts, so the recipient rule
// (the item's assignee, else its creator) has someone to exclude.
type bootFixture struct {
	*schedFixture
	me, other *models.User
}

func newBootFixture(t *testing.T) *bootFixture {
	t.Helper()
	f := &bootFixture{schedFixture: newSchedFixture(t)}
	f.me = createTestUser(t, f.s, "boot-me@example.com", "Me", "password-password-1")
	f.other = createTestUser(t, f.s, "boot-other@example.com", "Other", "password-password-1")
	return f
}

// mkFor creates a schedule on a fresh item created by userID.
func (f *bootFixture) mkFor(t *testing.T, userID string, in models.ScheduleCreate, created string) (*models.Item, *models.ItemSchedule) {
	t.Helper()
	item, sc := f.mk(t, in, created)
	f.exec(t, `UPDATE items SET created_by_user_id = ? WHERE id = ?`, userID, item.ID)
	return item, sc
}

func (f *bootFixture) read(t *testing.T, agent bool, loc *time.Location, now string) *BootstrapAgendaSet {
	t.Helper()
	set, err := f.s.BootstrapAgenda(BootstrapAgendaParams{WorkspaceID: f.ws.ID, UserID: f.me.ID, Agent: agent, Loc: loc, Now: rt(now)})
	if err != nil {
		t.Fatalf("BootstrapAgenda: %v", err)
	}
	return set
}

func refsOf(rows []BootstrapAgendaRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ItemRef
	}
	return out
}

// An unresolved occurrence stays in every read; done, skipped and moved
// (to a later day) take it out.
func TestBootstrapAgendaCarryForward(t *testing.T) {
	t.Parallel()
	f := newBootFixture(t)
	daily := models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-20T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}
	done, _ := f.mkFor(t, f.me.ID, daily, "2026-10-20T00:00:00Z")
	skipped, _ := f.mkFor(t, f.me.ID, daily, "2026-10-20T00:00:00Z")
	moved, _ := f.mkFor(t, f.me.ID, daily, "2026-10-20T00:00:00Z")
	kept, _ := f.mkFor(t, f.me.ID, daily, "2026-10-20T00:00:00Z")

	const now = "2026-10-29T12:00:00Z"
	if got := len(f.read(t, false, time.UTC, now).Carried); got != 4 {
		t.Fatalf("before any resolve, carried = %d, want 4", got)
	}
	f.mustResolve(t, done, ResolveInput{OccurrenceAt: rt("2026-10-20T09:00:00Z"), State: "done", Actor: "user"}, now)
	f.mustResolve(t, skipped, ResolveInput{OccurrenceAt: rt("2026-10-20T09:00:00Z"), State: "skip", Actor: "user"}, now)
	until := rt("2026-11-03T09:00:00Z")
	f.mustResolve(t, moved, ResolveInput{OccurrenceAt: rt("2026-10-20T09:00:00Z"), State: "move", Until: &until, Actor: "user"}, now)

	for i := 0; i < 2; i++ { // twice: a read resolves nothing, so nothing re-fires or drops
		set := f.read(t, false, time.UTC, now)
		if len(set.Carried) != 1 || set.Carried[0].ItemRef != kept.Ref {
			t.Fatalf("read %d: carried = %v, want only %s", i+1, refsOf(set.Carried), kept.Ref)
		}
		set.Carried[0].ReadMissed(time.UTC, rt(now))
		if set.Carried[0].MissedCount != 9 {
			t.Errorf("missed_count = %d, want 9 (the 21st through the 29th)", set.Carried[0].MissedCount)
		}
		if len(set.DueToday) != 0 || len(set.Stale) != 0 {
			t.Errorf("due today %v, stale %v: want none", refsOf(set.DueToday), refsOf(set.Stale))
		}
	}
	// A snooze re-fires; it does not resolve, so the occurrence stays carried.
	snooze, _ := f.mkFor(t, f.me.ID, daily, "2026-10-20T00:00:00Z")
	sUntil := rt("2026-11-03T09:00:00Z")
	f.mustResolve(t, snooze, ResolveInput{OccurrenceAt: rt("2026-10-20T09:00:00Z"), State: "snooze", Until: &sUntil, Actor: "user"}, now)
	if got := refsOf(f.read(t, false, time.UTC, now).Carried); len(got) != 2 {
		t.Errorf("after a snooze, carried = %v, want kept and the snoozed one", got)
	}
}

// A moved occurrence is deferred: it is due again at its new time.
func TestBootstrapAgendaMovedComesBackWhenDue(t *testing.T) {
	t.Parallel()
	f := newBootFixture(t)
	item, _ := f.mkFor(t, f.me.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-20T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, "2026-10-20T00:00:00Z")
	until := rt("2026-10-29T15:00:00Z")
	f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-10-20T09:00:00Z"), State: "move", Until: &until, Actor: "user"}, "2026-10-29T08:00:00Z")

	if set := f.read(t, false, time.UTC, "2026-10-29T08:00:00Z"); len(set.DueToday) != 1 || len(set.Carried) != 0 {
		t.Fatalf("moved to later today: due today %v, carried %v", refsOf(set.DueToday), refsOf(set.Carried))
	}
	if set := f.read(t, false, time.UTC, "2026-10-30T08:00:00Z"); len(set.Carried) != 1 || len(set.DueToday) != 0 {
		t.Fatalf("a day after its new time: due today %v, carried %v", refsOf(set.DueToday), refsOf(set.Carried))
	}
}

// A far-future occurrence moved into today keeps next_at at its seed instant
// and reads at next_override_at, so the near-term bound must admit either.
func TestBootstrapAgendaFarFutureMovedIntoToday(t *testing.T) {
	t.Parallel()
	f := newBootFixture(t)
	item, _ := f.mkFor(t, f.me.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-12-20T09:00", Tz: "UTC"}, "2026-10-20T00:00:00Z")
	until := rt("2026-10-29T15:00:00Z")
	f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-12-20T09:00:00Z"), State: "move", Until: &until, Actor: "user"}, "2026-10-29T08:00:00Z")

	if set := f.read(t, false, time.UTC, "2026-10-29T08:00:00Z"); len(set.DueToday) != 1 || len(set.Carried) != 0 {
		t.Fatalf("moved from December into today: due today %v, carried %v", refsOf(set.DueToday), refsOf(set.Carried))
	}
}

// Past 14 days an occurrence leaves the list and is only counted.
func TestBootstrapAgendaStaleAfterFourteenDays(t *testing.T) {
	t.Parallel()
	f := newBootFixture(t)
	weekly := models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-14T09:00", Tz: "UTC", Rrule: "FREQ=WEEKLY"}
	old, _ := f.mkFor(t, f.me.ID, weekly, "2026-10-14T00:00:00Z")
	edge, _ := f.mkFor(t, f.me.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-15T09:00", Tz: "UTC"}, "2026-10-14T00:00:00Z")

	// old is due 10-14 09:00, edge 10-15 09:00. At 10-29 09:00 old is 15 days
	// past due and edge is 14 days exactly.
	set := f.read(t, false, time.UTC, "2026-10-29T09:00:00Z")
	if got := refsOf(set.Stale); len(got) != 1 || got[0] != old.Ref {
		t.Fatalf("stale = %v, want only %s", got, old.Ref)
	}
	if got := refsOf(set.Carried); len(got) != 1 || got[0] != edge.Ref {
		t.Fatalf("carried = %v, want only %s (14 days exactly is not past 14 days)", got, edge.Ref)
	}
}

// The audience picks the agenda by the caller's kind, the recipient by the user.
func TestBootstrapAgendaAudienceAndRecipient(t *testing.T) {
	t.Parallel()
	f := newBootFixture(t)
	mk := func(userID, audience string) *models.Item {
		item, _ := f.mkFor(t, userID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-28T09:00", Tz: "UTC", Audience: audience}, "2026-10-28T00:00:00Z")
		return item
	}
	agentOnly, humanOnly, both := mk(f.me.ID, "agent"), mk(f.me.ID, "human"), mk(f.me.ID, "both")
	mk(f.other.ID, "both") // somebody else's
	assigned := mk(f.other.ID, "both")
	f.exec(t, `UPDATE items SET assigned_user_id = ? WHERE id = ?`, f.me.ID, assigned.ID) // created by other, held by me
	away := mk(f.me.ID, "both")
	f.exec(t, `UPDATE items SET assigned_user_id = ? WHERE id = ?`, f.other.ID, away.ID) // created by me, held by other

	const now = "2026-10-29T12:00:00Z"
	want := func(agent bool, items ...*models.Item) {
		t.Helper()
		got := map[string]bool{}
		for _, r := range f.read(t, agent, time.UTC, now).Carried {
			got[r.ItemRef] = true
		}
		if len(got) != len(items) {
			t.Fatalf("agent=%v: carried %v, want %d rows", agent, got, len(items))
		}
		for _, it := range items {
			if !got[it.Ref] {
				t.Errorf("agent=%v: %s missing from %v", agent, it.Ref, got)
			}
		}
	}
	want(true, agentOnly, both, assigned)
	want(false, humanOnly, both, assigned)
}

// "Today" is the caller's day, not the server's.
func TestBootstrapAgendaTodayIsTheCallersDay(t *testing.T) {
	t.Parallel()
	f := newBootFixture(t)
	f.mkFor(t, f.me.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-28T20:00", Tz: "UTC"}, "2026-10-28T00:00:00Z")
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	// 03:00Z on the 29th is 20:00 on the 28th in Los Angeles: the 20:00Z
	// occurrence (13:00 there) is today's. In UTC it is yesterday's.
	const now = "2026-10-29T03:00:00Z"
	if set := f.read(t, false, la, now); len(set.DueToday) != 1 || len(set.Carried) != 0 {
		t.Errorf("Los Angeles: due today %d, carried %d, want 1 and 0", len(set.DueToday), len(set.Carried))
	}
	if set := f.read(t, false, time.UTC, now); len(set.DueToday) != 0 || len(set.Carried) != 1 {
		t.Errorf("UTC: due today %d, carried %d, want 0 and 1", len(set.DueToday), len(set.Carried))
	}
}

// An all-day occurrence belongs to its date in the caller's zone, whatever zone
// the schedule is pinned to.
func TestBootstrapAgendaAllDayByDate(t *testing.T) {
	t.Parallel()
	f := newBootFixture(t)
	f.mkFor(t, f.me.ID, models.ScheduleCreate{Kind: "deadline_soft", Dtstart: "2026-10-29", Tz: "Pacific/Auckland"}, "2026-10-28T00:00:00Z")
	la, _ := time.LoadLocation("America/Los_Angeles")
	for _, c := range []struct {
		now            string
		today, carried int
	}{
		{"2026-10-29T18:00:00Z", 1, 0}, // the 29th in Los Angeles
		{"2026-10-30T18:00:00Z", 0, 1}, // the 30th
		{"2026-10-28T18:00:00Z", 0, 0}, // the 28th: not yet
	} {
		set := f.read(t, false, la, c.now)
		if len(set.DueToday) != c.today || len(set.Carried) != c.carried {
			t.Errorf("at %s: due today %d, carried %d, want %d and %d", c.now, len(set.DueToday), len(set.Carried), c.today, c.carried)
		}
	}
}

// A resolved one-shot (it ended), a future occurrence and an item that is done
// are not on the agenda; the one open, past occurrence beside them is.
func TestBootstrapAgendaLeavesOutTheRest(t *testing.T) {
	t.Parallel()
	f := newBootFixture(t)
	f.mkFor(t, f.me.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "UTC"}, "2026-10-28T00:00:00Z")
	over, _ := f.mkFor(t, f.me.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-28T09:00", Tz: "UTC"}, "2026-10-28T00:00:00Z")
	f.mustResolve(t, over, ResolveInput{OccurrenceAt: rt("2026-10-28T09:00:00Z"), State: "done", Actor: "user"}, "2026-10-29T08:00:00Z")
	archived, _ := f.mkFor(t, f.me.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-28T09:00", Tz: "UTC"}, "2026-10-28T00:00:00Z")
	f.exec(t, `UPDATE items SET deleted_at = ? WHERE id = ?`, "2026-10-28T10:00:00Z", archived.ID)
	open, _ := f.mkFor(t, f.me.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-28T09:00", Tz: "UTC"}, "2026-10-28T00:00:00Z")

	set := f.read(t, false, time.UTC, "2026-10-29T12:00:00Z")
	if got := refsOf(set.Carried); len(got) != 1 || got[0] != open.Ref || len(set.DueToday)+len(set.Stale) != 0 {
		t.Fatalf("carried %v, due today %v, stale %v: want only %s carried", got, refsOf(set.DueToday), refsOf(set.Stale), open.Ref)
	}
}

// A restricted caller's scope bounds the agenda: a collection they cannot see,
// or an empty grant, lists nothing.
func TestBootstrapAgendaRespectsScope(t *testing.T) {
	t.Parallel()
	f := newBootFixture(t)
	item, _ := f.mkFor(t, f.me.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-28T09:00", Tz: "UTC"}, "2026-10-28T00:00:00Z")
	read := func(scope PendingReminderScope) int {
		set, err := f.s.BootstrapAgenda(BootstrapAgendaParams{WorkspaceID: f.ws.ID, UserID: f.me.ID, Scope: scope, Loc: time.UTC, Now: rt("2026-10-29T12:00:00Z")})
		if err != nil {
			t.Fatal(err)
		}
		return len(set.Carried)
	}
	if got := read(PendingReminderScope{}); got != 1 {
		t.Fatalf("unrestricted = %d, want 1", got)
	}
	if got := read(PendingReminderScope{CollectionIDs: []string{item.CollectionID}}); got != 1 {
		t.Errorf("its own collection = %d, want 1", got)
	}
	if got := read(PendingReminderScope{CollectionIDs: []string{"not-a-collection"}}); got != 0 {
		t.Errorf("another collection = %d, want 0", got)
	}
	if got := read(PendingReminderScope{CollectionIDs: []string{}}); got != 0 {
		t.Errorf("no visible collection = %d, want 0", got)
	}
	if got := read(PendingReminderScope{CollectionIDs: []string{"not-a-collection"}, ItemIDs: []string{item.ID}}); got != 1 {
		t.Errorf("a granted item = %d, want 1", got)
	}
}

// Oldest first, then by ref.
func TestBootstrapAgendaOrder(t *testing.T) {
	t.Parallel()
	f := newBootFixture(t)
	var want []string
	for _, d := range []string{"2026-10-26", "2026-10-24", "2026-10-25"} {
		item, _ := f.mkFor(t, f.me.ID, models.ScheduleCreate{Kind: "check", Dtstart: d + "T09:00", Tz: "UTC"}, "2026-10-23T00:00:00Z")
		want = append(want, item.Ref)
	}
	got := refsOf(f.read(t, false, time.UTC, "2026-10-29T12:00:00Z").Carried)
	if len(got) != 3 || got[0] != want[1] || got[1] != want[2] || got[2] != want[0] {
		t.Fatalf("carried = %v, want oldest first (%s, %s, %s)", got, want[1], want[2], want[0])
	}
}

// A snooze changes reminder timing only; the sparse target override survives.
func TestBootstrapAgendaSnoozePreservesMovedTarget(t *testing.T) {
	t.Parallel()
	f := newBootFixture(t)
	item, _ := f.mkFor(t, f.me.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-20T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, "2026-10-20T00:00:00Z")
	next := rt("2026-11-10T09:00:00Z")
	f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-10-20T09:00:00Z"), State: "move", Until: &next, Actor: "user"}, "2026-10-29T08:00:00Z")
	if set := f.read(t, false, time.UTC, "2026-10-29T12:00:00Z"); len(set.Carried)+len(set.DueToday) != 0 {
		t.Fatalf("moved to next week: carried %d, due today %d, want none", len(set.Carried), len(set.DueToday))
	}
	snooze := rt("2026-10-30T09:00:00Z")
	f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-10-20T09:00:00Z"), State: "snooze", Until: &snooze, Actor: "user"}, "2026-10-29T09:00:00Z")
	if set := f.read(t, false, time.UTC, "2026-10-29T12:00:00Z"); len(set.Carried)+len(set.DueToday) != 0 {
		t.Fatalf("snooze resurrected the original target: %+v", set)
	}
}
