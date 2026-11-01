package store

import (
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4104 D2-a evidence: import recomputes next_at and the managed remind_at
// from the carried wall, override and lead, with the bundle's next_at only a
// hint. The lead's bar: for a VALID bundle that must be indistinguishable from
// carrying them verbatim. This drives every schedule state through the real
// store paths (create, the fire pass, resolveOccurrenceAt, each with an injected
// clock), exports, imports into a fresh workspace and compares.
//
// Not driven through a path: "acked". A managed reminder row cannot be
// acknowledged (AckReminder refuses it, schedule_id IS NOT NULL); resolving the
// occurrence done is the real acknowledgement and re-arms the row, so
// acked_at is asserted equal (always absent) and the fired-acked row is
// fire-then-resolve-done.

type matrixRow struct {
	name     string
	ny       bool // recipient is in America/New_York (floating) and the schedule is pinned there
	created  string
	create   models.ScheduleCreate // Tz is filled per variant
	drive    func(m *matrixRun) time.Time
	wantWall string // fixture check: the outstanding wall the state must have reached
	wantAt   string // pinned only: and its instant
	pinnedOn bool   // false: a pinned-only row is not generated for floating
	floatOn  bool
}

type matrixRun struct {
	t        *testing.T
	f        *schedFixture
	item     *models.Item
	view     *time.Location
	floating bool
	ts       time.Time // last action
}

func (m *matrixRun) sched() *models.ItemSchedule { return m.f.reload(m.t, m.item.ID) }

// dueOf is the instant the fire pass fires the outstanding occurrence: the
// override if set, else the occurrence, less the lead.
func dueOf(sc *models.ItemSchedule, view *time.Location) time.Time {
	at, ok, err := OutstandingInstant(sc, view)
	if err != nil || !ok {
		panic("dueOf: no outstanding occurrence")
	}
	if sc.NextOverrideAt != nil {
		at = rt(*sc.NextOverrideAt)
	}
	return at.Add(-time.Duration(sc.LeadMinutes) * time.Minute)
}

func (m *matrixRun) fireOurs(at time.Time) {
	m.t.Helper()
	for _, r := range m.f.fire(m.t, at.UTC().Format(time.RFC3339)) {
		if r.ItemID == m.item.ID {
			m.ts = at
			return
		}
	}
	m.t.Fatalf("fixture: fire pass at %s did not fire the item", at.Format(time.RFC3339))
}

func (m *matrixRun) resolve(in ResolveInput, at time.Time) {
	m.t.Helper()
	sc := m.sched()
	x, ok, err := OutstandingInstant(sc, m.view)
	if err != nil || !ok {
		m.t.Fatalf("fixture: no outstanding occurrence to resolve (%v)", err)
	}
	in.OccurrenceAt, in.View, in.Actor = x, m.view, "user"
	m.f.mustResolve(m.t, m.item, in, at.UTC().Format(time.RFC3339))
	m.ts = at
}

func (m *matrixRun) fireAndDone() {
	m.t.Helper()
	m.fireOurs(dueOf(m.sched(), m.view))
	m.resolve(ResolveInput{State: "done"}, m.ts.Add(time.Minute))
}

func driveNone(m *matrixRun) time.Time { return rt("2026-10-29T13:00:00Z") }

func driveUntilWall(m *matrixRun, wall, at string) {
	for i := 0; i < 8; i++ {
		sc := m.sched()
		if sc.NextWall != nil && *sc.NextWall == wall && (at == "" || deref(sc.NextAt) == at) {
			return
		}
		m.fireAndDone()
	}
	sc := m.sched()
	m.t.Fatalf("fixture: never reached wall %s at %q; next_wall=%s next_at=%s", wall, at, deref(sc.NextWall), deref(sc.NextAt))
}

var matrixRows = []matrixRow{
	{name: "never-fired", created: "2026-10-29T12:00:00Z", pinnedOn: true, floatOn: true, drive: driveNone},
	{name: "fired-acked", created: "2026-10-29T12:00:00Z", pinnedOn: true, floatOn: true, drive: func(m *matrixRun) time.Time {
		m.fireAndDone()
		return m.ts.Add(time.Minute)
	}},
	{name: "fired-unacked", created: "2026-10-29T12:00:00Z", pinnedOn: true, floatOn: true, drive: func(m *matrixRun) time.Time {
		m.fireOurs(dueOf(m.sched(), m.view))
		return m.ts.Add(time.Minute)
	}},
	{name: "snoozed", created: "2026-10-29T12:00:00Z", pinnedOn: true, floatOn: true, drive: func(m *matrixRun) time.Time {
		m.fireOurs(dueOf(m.sched(), m.view))
		until := rt("2026-10-30T10:00:00Z")
		m.resolve(ResolveInput{State: "snooze", Until: &until}, m.ts.Add(5*time.Minute))
		return m.ts.Add(time.Minute)
	}},
	{name: "moved", created: "2026-10-29T12:00:00Z", pinnedOn: true, floatOn: true, drive: func(m *matrixRun) time.Time {
		m.fireOurs(dueOf(m.sched(), m.view))
		until := rt("2026-10-30T14:00:00Z")
		m.resolve(ResolveInput{State: "move", Until: &until}, m.ts.Add(5*time.Minute))
		return m.ts.Add(time.Minute)
	}},
	{name: "moved-before-fire", created: "2026-10-29T12:00:00Z", pinnedOn: true, floatOn: true, drive: func(m *matrixRun) time.Time {
		until := rt("2026-10-30T14:00:00Z")
		m.resolve(ResolveInput{State: "move", Until: &until}, rt("2026-10-29T13:00:00Z"))
		return rt("2026-10-29T13:01:00Z")
	}},
	{name: "ended", created: "2026-10-29T12:00:00Z", pinnedOn: true, floatOn: true,
		create: models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", LeadMinutes: 15},
		drive: func(m *matrixRun) time.Time {
			m.fireAndDone()
			if sc := m.sched(); sc.EndedAt == nil {
				m.t.Fatalf("fixture: one-shot not ended: %+v", sc)
			}
			return m.ts.Add(time.Minute)
		}},
	{name: "completion-anchor-acked", created: "2026-10-29T12:00:00Z", pinnedOn: true, floatOn: true,
		create: models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Rrule: "FREQ=DAILY;INTERVAL=3", Anchor: "completion", LeadMinutes: 15},
		drive: func(m *matrixRun) time.Time {
			m.fireOurs(dueOf(m.sched(), m.view))
			m.resolve(ResolveInput{State: "done"}, m.ts.Add(37*time.Minute)) // done late: the next one is re-anchored
			if m.sched().AnchorWall == nil {
				m.t.Fatalf("fixture: completion anchor not set after done")
			}
			return m.ts.Add(time.Minute)
		}},
	{name: "completion-anchor-fired-unacked", created: "2026-10-29T12:00:00Z", pinnedOn: true, floatOn: true,
		create: models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Rrule: "FREQ=DAILY;INTERVAL=3", Anchor: "completion", LeadMinutes: 15},
		drive: func(m *matrixRun) time.Time {
			m.fireAndDone() // acked once: anchored, then the re-anchored occurrence fires and waits
			m.fireOurs(dueOf(m.sched(), m.view))
			return m.ts.Add(time.Minute)
		}},
	{name: "dst-fallback-first-half", ny: true, created: "2026-11-01T04:00:00Z", pinnedOn: true, floatOn: true,
		create: models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-01T00:30", Rrule: "FREQ=HOURLY"},
		drive: func(m *matrixRun) time.Time {
			driveUntilWall(m, "2026-11-01T01:30:00", "")
			return m.ts.Add(time.Minute)
		}},
	{name: "dst-fallback-second-half", ny: true, created: "2026-11-01T04:00:00Z", pinnedOn: true, wantAt: "2026-11-01T06:30:00Z",
		create: models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-01T00:30", Rrule: "FREQ=HOURLY"},
		drive: func(m *matrixRun) time.Time {
			driveUntilWall(m, "2026-11-01T01:30:00", "2026-11-01T06:30:00Z")
			return m.ts.Add(time.Minute)
		}},
	{name: "dst-fallback-second-half-fired-unacked", ny: true, created: "2026-11-01T04:00:00Z", pinnedOn: true,
		create: models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-01T00:30", Rrule: "FREQ=HOURLY"},
		drive: func(m *matrixRun) time.Time {
			driveUntilWall(m, "2026-11-01T01:30:00", "2026-11-01T06:30:00Z")
			m.fireOurs(dueOf(m.sched(), m.view))
			return m.ts.Add(time.Minute)
		}},
	{name: "dst-springforward-gap", ny: true, created: "2026-03-07T00:00:00Z", pinnedOn: true, floatOn: true,
		create: models.ScheduleCreate{Kind: "check", Dtstart: "2026-03-07T02:30", Rrule: "FREQ=DAILY"},
		drive: func(m *matrixRun) time.Time {
			driveUntilWall(m, "2026-03-08T02:30:00", "")
			return m.ts.Add(time.Minute)
		}},
}

func TestCarryRoundTripEveryScheduleState(t *testing.T) {
	t.Parallel()
	for _, row := range matrixRows {
		for _, floating := range []bool{false, true} {
			if (floating && !row.floatOn) || (!floating && !row.pinnedOn) {
				continue
			}
			variant := "pinned"
			if floating {
				variant = "floating"
			}
			t.Run(row.name+"/"+variant, func(t *testing.T) { t.Parallel(); runMatrixRow(t, row, floating) })
		}
	}
}

func runMatrixRow(t *testing.T, row matrixRow, floating bool) {
	f := newSchedFixture(t)
	in := row.create
	if in.Kind == "" {
		in = models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Rrule: "FREQ=DAILY", LeadMinutes: 15}
	}
	view := time.UTC
	zone := "UTC"
	if row.ny {
		zone = "America/New_York"
		view, _ = time.LoadLocation(zone)
	}
	if floating {
		in.Floating = true
	} else {
		in.Tz = zone
	}
	item := f.another(t, "Matrix item")
	var nyUser *models.User
	if floating && row.ny {
		nyUser = f.user(t, "ny-"+strings.ReplaceAll(row.name, "/", "-")+"@example.com", zone)
		f.assign(t, item.ID, nyUser.ID)
	}
	if _, err := f.s.createScheduleAt(f.ws.ID, item.ID, in, rt(row.created)); err != nil {
		t.Fatalf("create: %v", err)
	}
	m := &matrixRun{t: t, f: f, item: item, view: view, floating: floating}
	importNow := row.drive(m)

	src := f.reload(t, item.ID)
	if row.wantWall != "" && deref(src.NextWall) != row.wantWall {
		t.Fatalf("fixture: source next_wall %s want %s", deref(src.NextWall), row.wantWall)
	}
	if !floating && row.wantAt != "" && deref(src.NextAt) != row.wantAt {
		t.Fatalf("fixture: source next_at %s want %s", deref(src.NextAt), row.wantAt)
	}
	t.Logf("source: next_at=%s next_wall=%s override=%s anchor_wall=%s ended=%s fired=%v",
		deref(src.NextAt), deref(src.NextWall), deref(src.NextOverrideAt), deref(src.AnchorWall), deref(src.EndedAt), src.ReminderFired)

	bundle := f.exportBundle(t)
	dst, rep := f.importBundle(t, bundle, "Matrix copy")
	if len(rep.DroppedSchedules) != 0 {
		t.Fatalf("[drop] dropped %+v", rep.DroppedSchedules)
	}
	dItem := f.itemByTitle(t, dst.ID, "Matrix item")
	if nyUser != nil {
		var prior string
		_ = f.s.db.QueryRow(f.s.q(`SELECT COALESCE(assigned_user_id, '') FROM items WHERE id = ?`), dItem.ID).Scan(&prior)
		if prior != nyUser.ID {
			t.Logf("note: import does not carry the assignee (was %q); re-assigned the NY recipient on the imported item so the zone is the same on both sides", prior)
			f.exec(t, `UPDATE items SET assigned_user_id = ? WHERE id = ?`, nyUser.ID, dItem.ID)
		}
	}
	got, err := f.s.GetSchedule(dst.ID, dItem.ID)
	if err != nil || got == nil {
		t.Fatalf("[drop] schedule lost on import: %v", err)
	}
	t.Logf("import:  next_at=%s next_wall=%s override=%s anchor_wall=%s ended=%s fired=%v",
		deref(got.NextAt), deref(got.NextWall), deref(got.NextOverrideAt), deref(got.AnchorWall), deref(got.EndedAt), got.ReminderFired)

	// (a)
	for _, c := range []struct {
		name string
		a, b *string
	}{
		{"next_at", src.NextAt, got.NextAt}, {"next_wall", src.NextWall, got.NextWall},
		{"next_override_at", src.NextOverrideAt, got.NextOverrideAt}, {"anchor_wall", src.AnchorWall, got.AnchorWall},
		{"ended_at", src.EndedAt, got.EndedAt},
	} {
		if (c.a == nil) != (c.b == nil) || deref(c.a) != deref(c.b) {
			t.Errorf("[a] %s src=%q imported=%q", c.name, deref(c.a), deref(c.b))
		}
	}
	// (b)
	if (src.ReminderID == nil) != (got.ReminderID == nil) {
		t.Errorf("[b] managed reminder present: src=%v imported=%v", src.ReminderID != nil, got.ReminderID != nil)
	} else if src.ReminderID != nil {
		sr := f.reminder(t, *src.ReminderID)
		dr, derr := f.s.GetReminder(dst.ID, *got.ReminderID)
		if derr != nil || dr == nil {
			t.Fatalf("[b] imported managed reminder unreadable: %v %v", dr, derr)
		}
		if sr.RemindAt != dr.RemindAt {
			t.Errorf("[b] remind_at src=%s imported=%s", sr.RemindAt, dr.RemindAt)
		}
		if deref(sr.FiredAt) != deref(dr.FiredAt) {
			t.Errorf("[b] fired_at src=%q imported=%q", deref(sr.FiredAt), deref(dr.FiredAt))
		}
		if deref(sr.AckedAt) != deref(dr.AckedAt) {
			t.Errorf("[b] acked_at src=%q imported=%q", deref(sr.AckedAt), deref(dr.AckedAt))
		}
	}
	// (c)
	sx, sok, serr := OutstandingInstant(src, view)
	dx, dok, derr := OutstandingInstant(got, view)
	if serr != nil || derr != nil || sok != dok || !sx.Equal(dx) {
		t.Errorf("[c] OutstandingInstant src=%s/%v/%v imported=%s/%v/%v", sx.Format(time.RFC3339), sok, serr, dx.Format(time.RFC3339), dok, derr)
	}

	// The remaining checks only mean something if (a)-(c) held, but run
	// regardless: a divergence there is evidence too.
	ours := func(at time.Time) int {
		n := 0
		for _, r := range f.fire(t, at.UTC().Format(time.RFC3339)) {
			if r.WorkspaceID == dst.ID {
				n++
			}
		}
		return n
	}
	// (d) the import instant fires nothing.
	if n := ours(importNow); n != 0 {
		t.Errorf("[d] fire pass at import instant %s fired %d imported reminder(s), want 0", importNow.Format(time.RFC3339), n)
	}
	// (e)
	if src.EndedAt != nil {
		if n := ours(importNow.Add(72 * time.Hour)); n != 0 {
			t.Errorf("[e] ended schedule fired %d after import", n)
		}
		return
	}
	if src.ReminderFired {
		// Fired and unacked: still pending, not re-armed, however long we wait.
		for _, d := range []time.Duration{time.Hour, 26 * time.Hour, 80 * time.Hour} {
			if n := ours(importNow.Add(d)); n != 0 {
				t.Errorf("[e] pending fired occurrence re-fired %d at +%s", n, d)
			}
		}
		after, _ := f.s.GetSchedule(dst.ID, dItem.ID)
		if after == nil || !after.ReminderFired || deref(after.NextAt) != deref(got.NextAt) {
			t.Errorf("[e] pending occurrence changed: before next_at=%s fired=%v after %+v", deref(got.NextAt), got.ReminderFired, after)
		}
		return
	}
	due := dueOf(src, view)
	if n := ours(due.Add(-time.Second)); n != 0 {
		t.Errorf("[e] fired %d one second before due %s", n, due.Format(time.RFC3339))
	}
	if n := ours(due); n != 1 {
		t.Errorf("[e] fire pass at due %s fired %d, want exactly 1", due.Format(time.RFC3339), n)
	}
	if n := ours(due.Add(time.Minute)); n != 0 {
		t.Errorf("[e] fired %d again a minute after due", n)
	}
}
