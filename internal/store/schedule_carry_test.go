package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// TASK-4104 D2: schedules travel with the workspace bundle (export, import and
// `pad db migrate-to-pg`, which rides them). Runs on SQLite in make test and on
// Postgres in make test-pg.

// jsonRoundTrip puts a bundle through its wire form, as the HTTP and migrate
// doors do, so a field the JSON drops or reshapes shows here.
func jsonRoundTrip(t *testing.T, in *models.WorkspaceExport) *models.WorkspaceExport {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out models.WorkspaceExport
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func (f *schedFixture) exportBundle(t *testing.T) *models.WorkspaceExport {
	t.Helper()
	data, err := f.s.ExportWorkspace(f.ws.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	return jsonRoundTrip(t, data)
}

func (f *schedFixture) importBundle(t *testing.T, data *models.WorkspaceExport, name string) (*models.Workspace, ImportReport) {
	t.Helper()
	ws, rep, err := f.s.ImportWorkspaceWithReport(data, name, "", "api")
	if err != nil {
		t.Fatalf("ImportWorkspaceWithReport: %v", err)
	}
	return ws, rep
}

func (f *schedFixture) itemByTitle(t *testing.T, wsID, title string) *models.Item {
	t.Helper()
	items, err := f.s.ListItems(wsID, models.ItemListParams{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range items {
		if items[i].Title == title {
			return &items[i]
		}
	}
	t.Fatalf("no item %q in %s", title, wsID)
	return nil
}

func (f *schedFixture) managedRows(t *testing.T, wsID, scheduleID string) int {
	t.Helper()
	return f.count(t, `SELECT COUNT(*) FROM item_reminders WHERE workspace_id = ? AND schedule_id = ?`, wsID, scheduleID)
}

type occTuple struct{ At, Wall, State, Actor, ResolvedAt, Comment string }

// occs reads a schedule's history, with the comment id replaced by the comment's
// body so ids that were re-minted compare equal.
func (f *schedFixture) occs(t *testing.T, scheduleID string) []occTuple {
	t.Helper()
	rows, err := f.s.db.Query(f.s.q(`
		SELECT o.occurrence_at, o.occurrence_wall, o.state, o.actor, o.resolved_at, COALESCE(c.body, '')
		FROM schedule_occurrences o LEFT JOIN comments c ON c.id = o.comment_id
		WHERE o.schedule_id = ? ORDER BY o.occurrence_at`), scheduleID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []occTuple
	for rows.Next() {
		var o occTuple
		if err := rows.Scan(&o.At, &o.Wall, &o.State, &o.Actor, &o.ResolvedAt, &o.Comment); err != nil {
			t.Fatal(err)
		}
		out = append(out, o)
	}
	return out
}

func TestExportImportRoundTripSchedules(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	u := f.user(t, "carry-rt@example.com", "")
	const created = "2026-10-29T12:00:00Z"
	mk := func(name string, in models.ScheduleCreate) *models.Item {
		item := f.another(t, name)
		if _, err := f.s.createScheduleAt(f.ws.ID, item.ID, in, rt(created)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return item
	}
	mk("pinned", models.ScheduleCreate{Kind: "deadline_hard", Dtstart: "2026-10-30T09:00", Tz: "America/New_York",
		Rrule: "FREQ=DAILY", RDates: []string{"2026-11-10T12:00"}, ExDates: []string{"2026-11-03T09:00"}, Audience: "human", LeadMinutes: 30})
	mk("floating armed", models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-03T09:00", Floating: true, Rrule: "FREQ=DAILY"})
	mk("all day", models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02", Tz: "UTC", Rrule: "FREQ=WEEKLY", LeadMinutes: 60})
	mk("window", models.ScheduleCreate{Kind: "window", Dtstart: "2026-11-04T09:00", Dtend: "2026-11-04T17:00", Tz: "UTC", Audience: "agent"})
	completion := mk("completion", models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC", Rrule: "FREQ=DAILY;INTERVAL=3", Anchor: "completion"})
	snoozed := mk("snoozed", dailyUTC)
	ended := mk("ended", models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC"})
	mk("floating fired", models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Floating: true, Rrule: "FREQ=DAILY"})

	if n := len(f.fire(t, "2026-10-30T09:00:00Z")); n != 4 {
		t.Fatalf("fired %d, want completion, snoozed, ended and floating fired", n)
	}
	f.mustResolve(t, completion, ResolveInput{OccurrenceAt: rt("2026-10-30T09:00:00Z"), State: "done", Actor: "user",
		Comment: f.comment(completion, u.ID, "completed it")}, "2026-10-30T09:10:00Z")
	until := rt("2026-10-30T11:00:00Z")
	f.mustResolve(t, snoozed, ResolveInput{OccurrenceAt: rt("2026-10-30T09:00:00Z"), State: "snooze", Until: &until, Actor: "user"}, "2026-10-30T09:10:00Z")
	f.mustResolve(t, ended, ResolveInput{OccurrenceAt: rt("2026-10-30T09:00:00Z"), State: "done", Actor: "agent",
		Comment: f.comment(ended, u.ID, "all done")}, "2026-10-30T09:10:00Z")

	src := f.exportBundle(t)
	if len(src.Schedules) != 8 {
		t.Fatalf("exported %d schedules, want 8", len(src.Schedules))
	}
	dst, rep := f.importBundle(t, jsonRoundTrip(t, src), "Carry copy")
	if len(rep.DroppedSchedules) != 0 {
		t.Fatalf("dropped %+v", rep.DroppedSchedules)
	}

	live, endedN := 0, 0
	for _, title := range []string{"pinned", "floating armed", "all day", "window", "completion", "snoozed", "ended", "floating fired"} {
		a, err := f.s.GetSchedule(f.ws.ID, f.itemByTitle(t, f.ws.ID, title).ID)
		if err != nil || a == nil {
			t.Fatalf("%s: source schedule %v %v", title, a, err)
		}
		di := f.itemByTitle(t, dst.ID, title)
		b, err := f.s.GetSchedule(dst.ID, di.ID)
		if err != nil || b == nil {
			t.Fatalf("%s: schedule lost on import (%v)", title, err)
		}
		if b.ID == a.ID || b.ItemID != di.ID || b.WorkspaceID != dst.ID {
			t.Errorf("%s: ids not re-minted: %+v", title, b)
		}
		ac, bc := *a, *b
		for _, c := range []*models.ItemSchedule{&ac, &bc} {
			c.ID, c.WorkspaceID, c.ItemID, c.ReminderID = "", "", "", nil
		}
		if !reflect.DeepEqual(ac, bc) {
			aj, _ := json.Marshal(ac)
			bj, _ := json.Marshal(bc)
			t.Errorf("%s: schedule differs after import\n src %s\n dst %s", title, aj, bj)
		}
		// Exactly one managed row per live schedule, none for an ended one.
		want := 1
		if a.EndedAt != nil {
			want, endedN = 0, endedN+1
		} else {
			live++
		}
		if n := f.managedRows(t, dst.ID, b.ID); n != want {
			t.Errorf("%s: %d managed reminder rows, want %d", title, n, want)
		}
		if want == 1 {
			ar := f.reminder(t, *a.ReminderID)
			br, err := f.s.GetReminder(dst.ID, *b.ReminderID)
			if err != nil || br == nil {
				t.Fatalf("%s: managed row %v %v", title, br, err)
			}
			if ar.RemindAt != br.RemindAt || deref(ar.FiredAt) != deref(br.FiredAt) || br.AckedAt != nil {
				t.Errorf("%s: managed row source %s/%s dst %s/%s/%v", title, ar.RemindAt, deref(ar.FiredAt), br.RemindAt, deref(br.FiredAt), br.AckedAt)
			}
		}
		if ao, bo := f.occs(t, a.ID), f.occs(t, b.ID); !reflect.DeepEqual(ao, bo) {
			t.Errorf("%s: history differs\n src %+v\n dst %+v", title, ao, bo)
		}
	}
	if live != 7 || endedN != 1 {
		t.Fatalf("fixture drifted: %d live, %d ended", live, endedN)
	}
	// History carries with its comment remapped, not copied.
	cs, _ := f.s.GetSchedule(dst.ID, f.itemByTitle(t, dst.ID, "completion").ID)
	if occ := f.occs(t, cs.ID); len(occ) != 1 || occ[0].Comment != "completed it" {
		t.Errorf("completion history after import: %+v", occ)
	}
	// Re-export equals the first export, modulo ids.
	again, err := f.s.ExportWorkspace(dst.Slug)
	if err != nil {
		t.Fatal(err)
	}
	norm := func(d *models.WorkspaceExport) map[string]models.ScheduleExport {
		titles := map[string]string{}
		for _, it := range d.Items {
			titles[it.ID] = it.Title
		}
		out := map[string]models.ScheduleExport{}
		for _, sc := range jsonRoundTrip(t, d).Schedules {
			sc.ItemID = titles[sc.ItemID]
			for i := range sc.Occurrences {
				sc.Occurrences[i].CommentID = ""
			}
			out[sc.ItemID] = sc
		}
		return out
	}
	if a, b := norm(src), norm(again); !reflect.DeepEqual(a, b) {
		t.Errorf("re-export differs from the first export:\n%+v\n%+v", a, b)
	}
}

func TestImportWithoutScheduleMemberIsUnchanged(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	data := f.exportBundle(t)
	b, _ := json.Marshal(data)
	if strings.Contains(string(b), `"schedules"`) {
		t.Fatalf("a workspace with no schedule exports a schedules member: %s", b)
	}
	// And a bundle written before the member existed imports as it always did.
	dst, rep := f.importBundle(t, data, "Old bundle")
	if len(rep.DroppedSchedules) != 0 {
		t.Fatalf("dropped %+v", rep.DroppedSchedules)
	}
	f.itemByTitle(t, dst.ID, f.item.Title)
	if n := f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE workspace_id = ?`, dst.ID); n != 0 {
		t.Errorf("%d schedules from a bundle with none", n)
	}
}

// validSchedExport is a schedule that imports, for the malformed cases to break
// one field of.
func validSchedExport(itemID string) models.ScheduleExport {
	return models.ScheduleExport{
		ItemID: itemID, Kind: "check", Dtstart: "2026-11-02T09:00:00", Tz: "UTC", Rrule: "FREQ=DAILY",
		Anchor: "calendar", Audience: "both",
		NextAt: "2026-11-02T09:00:00Z", NextWall: "2026-11-02T09:00:00",
		CreatedAt: "2026-10-29T12:00:00Z", UpdatedAt: "2026-10-29T12:00:00Z",
		Reminder: &models.ScheduleReminderExport{RemindAt: "2026-11-02T09:00:00Z"},
	}
}

func TestImportDropsMalformedSchedules(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	cases := []struct {
		name string
		mut  func(*models.ScheduleExport)
	}{
		{"non-compiling rule", func(s *models.ScheduleExport) { s.Rrule = "FREQ=HOURLY;BYSETPOS=1" }},
		{"unknown zone", func(s *models.ScheduleExport) { s.Tz = "Mars/Base" }},
		{"floating with a zone", func(s *models.ScheduleExport) { s.Floating = true }},
		{"pinned with no zone", func(s *models.ScheduleExport) { s.Tz = "" }},
		{"window without dtend", func(s *models.ScheduleExport) { s.Kind = "window" }},
		{"dtend on a check", func(s *models.ScheduleExport) { s.Dtend = "2026-11-02T10:00:00" }},
		{"completion with BYDAY", func(s *models.ScheduleExport) { s.Anchor = "completion"; s.Rrule = "FREQ=WEEKLY;BYDAY=MO" }},
		{"bad kind", func(s *models.ScheduleExport) { s.Kind = "urgent" }},
		{"bad audience", func(s *models.ScheduleExport) { s.Audience = "everyone" }},
		{"lead out of range", func(s *models.ScheduleExport) { s.LeadMinutes = 99999 }},
		{"bad dtstart", func(s *models.ScheduleExport) { s.Dtstart = "next tuesday" }},
		{"live schedule with no next_wall", func(s *models.ScheduleExport) { s.NextWall = "" }},
		{"garbage next_wall", func(s *models.ScheduleExport) { s.NextWall = "soon" }},
		{"garbage fired_at", func(s *models.ScheduleExport) { s.Reminder.FiredAt = "never" }},
		{"garbage override", func(s *models.ScheduleExport) { s.NextOverrideAt = "tomorrow" }},
		{"garbage anchor_wall", func(s *models.ScheduleExport) { s.AnchorWall = "x" }},
		{"ended with an outstanding occurrence", func(s *models.ScheduleExport) { s.EndedAt = "2026-10-30T09:00:00Z" }},
		{"bad occurrence state", func(s *models.ScheduleExport) {
			s.Occurrences = []models.ScheduleOccurrenceExport{{OccurrenceAt: "2026-11-01T09:00:00Z", OccurrenceWall: "2026-11-01T09:00:00", State: "weird", ResolvedAt: "2026-11-01T09:01:00Z"}}
		}},
		{"NUL in an occurrence actor", func(s *models.ScheduleExport) {
			s.Occurrences = []models.ScheduleOccurrenceExport{{OccurrenceAt: "2026-11-01T09:00:00Z", OccurrenceWall: "2026-11-01T09:00:00", State: "done", Actor: "us\x00er", ResolvedAt: "2026-11-01T09:01:00Z"}}
		}},
		{"rdates entry is a number", func(s *models.ScheduleExport) { s.RDates = []any{"2026-11-05T09:00:00", float64(5)} }},
		{"rdates entry is not a wall", func(s *models.ScheduleExport) { s.RDates = []any{"whenever"} }},
		{"exdates entry is an object", func(s *models.ScheduleExport) { s.ExDates = []any{map[string]any{"a": 1}} }},
		{"exdates entry is not a wall", func(s *models.ScheduleExport) { s.ExDates = []any{"2026-13-45T00:00:00"} }},
	}
	for i := range cases {
		f.another(t, "bad "+cases[i].name)
	}
	good := f.another(t, "good")
	data := f.exportBundle(t)
	byTitle := map[string]string{}
	for _, it := range data.Items {
		byTitle[it.Title] = it.ID
	}
	for _, c := range cases {
		s := validSchedExport(byTitle["bad "+c.name])
		c.mut(&s)
		data.Schedules = append(data.Schedules, s)
	}
	data.Schedules = append(data.Schedules, validSchedExport(byTitle[good.Title]), validSchedExport("ghost-item"))

	// Through the wire form: a number in rdates must reach the importer.
	dst, rep := f.importBundle(t, jsonRoundTrip(t, data), "Dropped")
	if got, want := len(rep.DroppedSchedules), len(cases)+1; got != want {
		t.Fatalf("dropped %d schedules, want %d: %+v", got, want, rep.DroppedSchedules)
	}
	for _, d := range rep.DroppedSchedules {
		if d.Reason == "" || strings.ContainsAny(d.Reason, "\x00") {
			t.Errorf("drop reason %+v", d)
		}
	}
	for _, c := range cases {
		it := f.itemByTitle(t, dst.ID, "bad "+c.name) // the item imported
		if sc, err := f.s.GetSchedule(dst.ID, it.ID); err != nil || sc != nil {
			t.Errorf("%s: a schedule was stored: %+v %v", c.name, sc, err)
		}
	}
	if sc, _ := f.s.GetSchedule(dst.ID, f.itemByTitle(t, dst.ID, "good").ID); sc == nil {
		t.Error("the valid schedule beside the malformed ones was not imported")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE workspace_id = ?`, dst.ID); n != 1 {
		t.Errorf("%d schedules stored, want 1", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_reminders WHERE workspace_id = ? AND schedule_id IS NOT NULL`, dst.ID); n != 1 {
		t.Errorf("%d managed rows stored, want 1", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrences o JOIN item_schedules s ON s.id = o.schedule_id WHERE s.workspace_id = ?`, dst.ID); n != 0 {
		t.Errorf("%d occurrence rows from dropped schedules", n)
	}
}

// BUG-4193's D2 rule: rdates/exdates are re-marshalled from the parsed values,
// never copied from the bundle, and the rule is stored in its canonical form.
func TestImportStoresCanonicalTiming(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	it := f.another(t, "canon")
	data := f.exportBundle(t)
	var id string
	for _, x := range data.Items {
		if x.Title == "canon" {
			id = x.ID
		}
	}
	s := validSchedExport(id)
	s.Rrule = "RRULE:FREQ=DAILY;INTERVAL=1"
	want, err := schedule.NormalizeRule(s.Rrule)
	if err != nil {
		t.Fatalf("fixture rule: %v", err)
	}
	s.RDates = []any{"2026-11-05T09:00", "2026-11-06T09:00:00"}
	s.ExDates = []any{"2026-11-03T09:00"}
	data.Schedules = append(data.Schedules, s)
	dst, rep := f.importBundle(t, jsonRoundTrip(t, data), "Canon")
	if len(rep.DroppedSchedules) != 0 {
		t.Fatalf("dropped %+v", rep.DroppedSchedules)
	}
	sc, _ := f.s.GetSchedule(dst.ID, f.itemByTitle(t, dst.ID, it.Title).ID)
	if sc == nil || sc.Rrule == nil || *sc.Rrule != want {
		t.Fatalf("stored rule %v, want canonical %q", sc, want)
	}
	var raw, rawEx string
	if err := f.s.db.QueryRow(f.s.q(`SELECT rdates, exdates FROM item_schedules WHERE id = ?`), sc.ID).Scan(&raw, &rawEx); err != nil {
		t.Fatal(err)
	}
	if raw != `["2026-11-05T09:00:00","2026-11-06T09:00:00"]` || rawEx != `["2026-11-03T09:00:00"]` {
		t.Errorf("stored rdates %s exdates %s, want canonical re-marshalled walls", raw, rawEx)
	}
}

// The imported schedule fires on the next tick, exactly once.
func TestImportedScheduleFiresOnce(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	item := f.another(t, "fires")
	if _, err := f.s.createScheduleAt(f.ws.ID, item.ID, models.ScheduleCreate{Kind: "deadline_hard", Dtstart: "2026-11-02T09:00", Tz: "UTC", Rrule: "FREQ=DAILY", LeadMinutes: 30}, rt("2026-10-29T12:00:00Z")); err != nil {
		t.Fatal(err)
	}
	dst, _ := f.importBundle(t, f.exportBundle(t), "Fires")
	of := func(fired []*models.Reminder) (n int) {
		for _, r := range fired {
			if r.WorkspaceID == dst.ID {
				n++
			}
		}
		return n
	}
	if n := of(f.fire(t, "2026-11-02T08:29:59Z")); n != 0 {
		t.Fatalf("fired %d before the lead", n)
	}
	if n := of(f.fire(t, "2026-11-02T08:30:00Z")); n != 1 {
		t.Fatalf("fired %d on the first tick, want 1", n)
	}
	if n := of(f.fire(t, "2026-11-02T08:31:00Z")); n != 0 {
		t.Fatalf("fired %d on the second tick, want 0", n)
	}
	sc, _ := f.s.GetSchedule(dst.ID, f.itemByTitle(t, dst.ID, "fires").ID)
	if sc == nil || !sc.ReminderFired {
		t.Fatalf("schedule after fire: %+v", sc)
	}
}
