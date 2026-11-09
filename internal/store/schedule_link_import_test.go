package store

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Time links in the workspace bundle, import side (TASK-4353, PR 2b): see
// schedule_link_carry_test.go for the fixtures.

// linkTuples reads a workspace's links and overrides as strings with no ids,
// the ends named by item title, so a restore and its source compare equal.
func (f *schedFixture) linkTuples(t *testing.T, wsID string) []string {
	t.Helper()
	rows, err := f.s.db.Query(f.s.q(`SELECT l.id, pi.title, l.parent_role, ci.title, l.child_role, l.offset_mode, l.offset_elapsed, l.offset_days, l.offset_clock, l.offset_all_day, l.offset_zone, l.cancel_policy, l.created_at, l.updated_at
 FROM schedule_links l JOIN item_schedules ps ON ps.id=l.parent_schedule_id JOIN items pi ON pi.id=ps.item_id
 JOIN item_schedules cs ON cs.id=l.child_schedule_id JOIN items ci ON ci.id=cs.item_id
 WHERE l.workspace_id=? ORDER BY pi.title, l.parent_role, ci.title, l.child_role`), wsID)
	if err != nil {
		t.Fatal(err)
	}
	type rec struct{ id, tuple string }
	var recs []rec
	for rows.Next() {
		var id, pt, pr, ct, cr, mode, zone, policy, created, updated string
		var elapsed int64
		var days, clock, allDay int
		if err := rows.Scan(&id, &pt, &pr, &ct, &cr, &mode, &elapsed, &days, &clock, &allDay, &zone, &policy, &created, &updated); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, rec{id, fmt.Sprintf("%s.%s -> %s.%s %s/%d/%d/%d/%d/%s %s %s %s", pt, pr, ct, cr, mode, elapsed, days, clock, allDay, zone, policy, created, updated)})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	var out []string
	for _, r := range recs {
		out = append(out, r.tuple)
		orows, err := f.s.db.Query(f.s.q(`SELECT occurrence_key, COALESCE(offset_mode,'-'), offset_elapsed, offset_days, offset_clock, offset_all_day, offset_zone, COALESCE(no_longer_needed_at,'-'), created_at, updated_at FROM schedule_link_overrides WHERE link_id=? ORDER BY occurrence_key`), r.id)
		if err != nil {
			t.Fatal(err)
		}
		for orows.Next() {
			var key, mode, zone, gone, created, updated string
			var elapsed int64
			var days, clock, allDay int
			if err := orows.Scan(&key, &mode, &elapsed, &days, &clock, &allDay, &zone, &gone, &created, &updated); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("  override %s %s/%d/%d/%d/%d/%s gone=%s %s %s", key, mode, elapsed, days, clock, allDay, zone, gone, created, updated))
		}
		if err := orows.Err(); err != nil {
			t.Fatal(err)
		}
		orows.Close()
	}
	return out
}

// dateTuples is every schedule's stored dates and its managed reminder, by item title.
func (f *schedFixture) dateTuples(t *testing.T, wsID string) []string {
	t.Helper()
	rows, err := f.s.db.Query(f.s.q(`SELECT i.title, COALESCE(sc.next_at,''), COALESCE(sc.next_wall,''), COALESCE(sc.next_override_at,''), COALESCE(r.remind_at,'')
 FROM item_schedules sc JOIN items i ON i.id=sc.item_id LEFT JOIN item_reminders r ON r.schedule_id=sc.id WHERE sc.workspace_id=? ORDER BY i.title`), wsID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a, b, c, d, e string
		if err := rows.Scan(&a, &b, &c, &d, &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, strings.Join([]string{a, b, c, d, e}, " | "))
	}
	return out
}

// changeTypes counts a workspace's journal rows by type.
func (f *schedFixture) changeTypes(t *testing.T, wsID string) map[string]int {
	t.Helper()
	rows, err := f.s.db.Query(f.s.q(`SELECT change_type, COUNT(*) FROM schedule_changes WHERE workspace_id=? GROUP BY change_type`), wsID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var typ string
		var n int
		if err := rows.Scan(&typ, &n); err != nil {
			t.Fatal(err)
		}
		out[typ] = n
	}
	return out
}

// linkJSON renders a bundle's links with the schedule ids replaced by item titles.
func linkJSON(t *testing.T, b *models.WorkspaceExport) []string {
	t.Helper()
	byID := map[string]string{}
	for title, id := range bundleScheduleIDs(t, b) {
		byID[id] = title
	}
	var out []string
	for _, l := range b.ScheduleLinks {
		l.ParentScheduleID, l.ChildScheduleID = byID[l.ParentScheduleID], byID[l.ChildScheduleID]
		raw, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(raw))
	}
	sort.Strings(out)
	return out
}

func TestBundleLinks_RoundTripIsLossless(t *testing.T) {
	f := newLinkBundleFix(t)
	f.richLinks(t)
	src := f.exportBundle(t)
	ws, rep := f.importBundle(t, src, "Restored links")
	if len(rep.DroppedSchedules) != 0 {
		t.Fatalf("dropped: %+v", rep.DroppedSchedules)
	}
	want, got := f.linkTuples(t, f.ws.ID), f.linkTuples(t, ws.ID)
	if len(want) != 6 || !reflect.DeepEqual(want, got) {
		t.Fatalf("links differ after the round trip:\n src %q\n dst %q", want, got)
	}
	// New ids, in the destination workspace, on both ends.
	srcIDs := map[string]bool{}
	for _, l := range src.ScheduleLinks {
		srcIDs[l.ParentScheduleID], srcIDs[l.ChildScheduleID] = true, true
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_links l JOIN item_schedules p ON p.id=l.parent_schedule_id JOIN item_schedules c ON c.id=l.child_schedule_id WHERE l.workspace_id=? AND p.workspace_id=? AND c.workspace_id=?`, ws.ID, ws.ID, ws.ID); n != 3 {
		t.Fatalf("%d links of the new workspace join schedules of the new workspace, want 3", n)
	}
	for id := range srcIDs {
		if f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE id=? AND workspace_id=?`, id, ws.ID) != 0 {
			t.Fatalf("a link end kept its source id %s", id)
		}
	}
	// The restored workspace exports the same links, ends renamed by item.
	again, err := f.s.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(linkJSON(t, src), linkJSON(t, jsonRoundTrip(t, again))) {
		t.Fatalf("re-export differs:\n src %q\n dst %q", linkJSON(t, src), linkJSON(t, jsonRoundTrip(t, again)))
	}
	if again.Version != 3 {
		t.Fatalf("version=%d", again.Version)
	}
}

func TestBundleLinks_ImportNeverMovesADate(t *testing.T) {
	f := newLinkBundleFix(t)
	// Dates that disagree with the link on purpose: the bundle's dates are the truth.
	f.linkRaw(t, 0, 1, "target", "target", models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: 5 * 86400})
	f.linkRaw(t, 1, 2, "target", "target", civilNY)
	c2 := f.reload(t, f.items[2].ID)
	f.linkOverrideRow(t, f.storedLinkID(t, 1, 2), OccurrenceKey(c2, *c2.NextAt, *c2.NextWall), &minusHr, "", "2030-03-01T10:00:00Z", "2030-03-01T10:00:00Z")
	src := f.exportBundle(t)
	before := f.dateTuples(t, f.ws.ID)
	srcTypes := f.changeTypes(t, f.ws.ID)
	ws, rep := f.importBundle(t, src, "Restored without moving")
	if len(rep.DroppedSchedules) != 0 {
		t.Fatalf("dropped: %+v", rep.DroppedSchedules)
	}
	if got := f.dateTuples(t, ws.ID); !reflect.DeepEqual(before, got) {
		t.Fatalf("an import moved a date:\n src %q\n dst %q", before, got)
	}
	got := f.changeTypes(t, ws.ID)
	if got["move"] != srcTypes["move"] {
		t.Fatalf("move rows: src %d, dst %d", srcTypes["move"], got["move"])
	}
	for typ, n := range got {
		if typ != "import" && n != srcTypes[typ] {
			t.Fatalf("journal rows of type %q: src %d, dst %d", typ, srcTypes[typ], n)
		}
	}
	if n, m := f.count(t, `SELECT COUNT(*) FROM item_reminders WHERE workspace_id=? AND schedule_id IS NOT NULL`, ws.ID), f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE workspace_id=?`, ws.ID); n != m {
		t.Fatalf("%d managed reminders for %d schedules", n, m)
	}
}

func TestBundleLinks_ImportedLinksDriveALiveCascade(t *testing.T) {
	f := newLinkBundleFix(t)
	f.link(t, 0, 1, "target", "target", oneDay)
	f.link(t, 1, 2, "target", "target", oneDay)
	ws, _ := f.importBundle(t, f.exportBundle(t), "Restored and moving")
	var items []*models.Item
	acc := models.CommitmentAccess{Readable: map[string]bool{}, Editable: map[string]bool{}}
	for _, title := range []string{"P", "C1", "C2"} {
		it := f.itemByTitle(t, ws.ID, title)
		items = append(items, it)
		acc.Readable[it.ID], acc.Editable[it.ID] = true, true
	}
	res, err := f.s.MoveLinkedDate(ws.ID, models.LinkedMoveInput{ItemID: items[0].ID, Role: "target", To: "2030-03-20T09:00:00Z", Actor: "user", Access: acc})
	if err != nil || len(res.Changes) != 3 {
		t.Fatalf("a move on the restored parent did not reach its children: %+v %v", res, err)
	}
	c, err := f.s.GetCommitment(ws.ID, items[2].ID)
	if err != nil || c.Roles.Target == nil || *c.Roles.Target != "2030-03-22T09:00:00Z" {
		t.Fatalf("restored grandchild target: %+v %v", c, err)
	}
}

func TestBundleLinks_OldBundlesImportUnchanged(t *testing.T) {
	f := newLinkBundleFix(t)
	f.link(t, 0, 1, "target", "target", oneDay)
	// A version 1 bundle: no roles, ids, overrides or links.
	v1 := f.exportBundle(t)
	v1.ScheduleLinks = nil
	stripV2(v1)
	ws, rep := f.importBundle(t, v1, "From v1")
	if len(rep.DroppedSchedules) != 0 || f.count(t, `SELECT COUNT(*) FROM schedule_links WHERE workspace_id=?`, ws.ID) != 0 || f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE workspace_id=?`, ws.ID) != 4 {
		t.Fatalf("v1 import: %+v", rep)
	}
	// A version 2 bundle, with the member a v2 export never wrote.
	m := bundleObject(t, f.exportBundle(t))
	delete(m, "schedule_links")
	m["version"] = 2
	ws, rep = f.importBundle(t, bundleDecode(t, m), "From v2")
	if len(rep.DroppedSchedules) != 0 || f.count(t, `SELECT COUNT(*) FROM schedule_links WHERE workspace_id=?`, ws.ID) != 0 || f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE workspace_id=?`, ws.ID) != 4 {
		t.Fatalf("v2 import: %+v", rep)
	}
	// A current bundle of a workspace with no links writes no links member.
	g := newSchedFixture(t)
	g.create(t, dailyUTC)
	if _, ok := bundleObject(t, g.exportBundle(t))["schedule_links"]; ok {
		t.Fatal("a workspace with no links exports a schedule_links member")
	}
}

func TestBundleLinks_OlderBundlesCannotCarryLinks(t *testing.T) {
	f := newLinkBundleFix(t)
	f.link(t, 0, 1, "target", "target", oneDay)
	for _, c := range []struct {
		name    string
		mutate  func(m map[string]any)
		wantErr string
	}{
		{"v1 with links", func(m map[string]any) { m["version"] = 1 }, "contains version"},
		{"v2 with links", func(m map[string]any) { m["version"] = 2 }, "contains version 3 schedule members"},
		{"v2 with an explicit empty member", func(m map[string]any) { m["version"] = 2; m["schedule_links"] = []any{} }, "contains version 3 schedule members"},
		{"v2 with an explicit null member", func(m map[string]any) { m["version"] = 2; m["schedule_links"] = nil }, "contains version 3 schedule members"},
		{"newer than supported", func(m map[string]any) { m["version"] = 4 }, "unsupported export version: 4"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := bundleObject(t, f.exportBundle(t))
			c.mutate(m)
			n := f.count(t, `SELECT COUNT(*) FROM workspaces`)
			_, _, err := f.s.ImportWorkspaceWithReport(bundleDecode(t, m), "Refused", "", "api")
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err=%v, want %q", err, c.wantErr)
			}
			if f.count(t, `SELECT COUNT(*) FROM workspaces`) != n {
				t.Fatal("husk workspace")
			}
		})
	}
	// A bundle built by a Go caller is judged the same way as a decoded one.
	b, err := f.s.ExportWorkspace(f.ws.Slug)
	if err != nil {
		t.Fatal(err)
	}
	b.Version = 2
	if _, _, err := f.s.ImportWorkspaceWithReport(b, "Refused", "", "api"); err == nil || !strings.Contains(err.Error(), "contains version 3 schedule members") {
		t.Fatalf("a Go-built v2 bundle with links: err=%v", err)
	}
}

// linkObject is one bundle link, for bundles edited by hand.
func linkObject(parent, child, pr, cr string, off map[string]any) map[string]any {
	return map[string]any{"parent_schedule_id": parent, "parent_role": pr, "child_schedule_id": child, "child_role": cr, "offset": off,
		"cancel_policy": "waiting_on", "created_at": "2030-03-01T10:00:00Z", "updated_at": "2030-03-01T10:00:00Z"}
}

func elapsedOffset(secs int64) map[string]any {
	return map[string]any{"mode": "elapsed", "elapsed_secs": secs}
}

func goodOverride() map[string]any {
	return map[string]any{"occurrence_key": "2030-03-09T09:00:00Z|2030-03-09T09:00:00", "no_longer_needed_at": "2030-03-04T12:00:00Z",
		"created_at": "2030-03-04T12:00:00Z", "updated_at": "2030-03-04T12:00:00Z"}
}

// importWithLinks replaces a bundle's links and imports it.
func importWithLinks(t *testing.T, f *linkFix, mutate func(m map[string]any, ids map[string]string), links ...map[string]any) (*models.Workspace, ImportReport) {
	t.Helper()
	b := f.exportBundle(t)
	ids := bundleScheduleIDs(t, b)
	m := bundleObject(t, b)
	delete(m, "schedule_links")
	if mutate != nil {
		mutate(m, ids)
	}
	if len(links) > 0 {
		var xs []any
		for _, l := range links {
			xs = append(xs, l)
		}
		m["schedule_links"] = xs
	}
	return f.importBundle(t, bundleDecode(t, m), "Restored "+t.Name())
}

func TestBundleLinks_ImportRefusesWhatALiveWriteRefuses(t *testing.T) {
	f := newLinkBundleFix(t)
	ids := bundleScheduleIDs(t, f.exportBundle(t))
	p, c1, c2, d := ids["P"], ids["C1"], ids["C2"], ids["D"]
	good := func() map[string]any { return linkObject(p, c1, "target", "target", elapsedOffset(86400)) }
	with := func(l map[string]any, k string, v any) map[string]any { l[k] = v; return l }
	offsetWith := func(off map[string]any) map[string]any {
		return with(linkObject(p, c2, "target", "target", nil), "offset", off)
	}
	for _, c := range []struct {
		name string
		bad  map[string]any
		want string
	}{
		{"a parent that is not in the bundle", linkObject("no-such-schedule", c2, "target", "target", elapsedOffset(1)), "links:orphan"},
		{"a child that is not in the bundle", linkObject(p, "no-such-schedule", "target", "target", elapsedOffset(1)), "links:orphan"},
		{"a link to itself", linkObject(c2, c2, "target", "target", elapsedOffset(1)), "links:self"},
		{"a cycle", linkObject(c1, p, "target", "target", elapsedOffset(1)), "links:cycle"},
		{"a second incoming edge for the child role", linkObject(d, c1, "target", "target", elapsedOffset(1)), "links:duplicate_incoming"},
		{"an unknown parent role", linkObject(p, c2, "bogus", "target", elapsedOffset(1)), "links:enum"},
		{"an unknown child role", linkObject(p, c2, "target", "occupied_end", elapsedOffset(1)), "links:enum"},
		{"an unknown cancel policy", with(linkObject(p, c2, "target", "target", elapsedOffset(1)), "cancel_policy", "explode"), "links:enum"},
		{"a child role the commitment does not have", linkObject(p, c2, "target", "earliest", elapsedOffset(1)), "links:role"},
		{"a parent role the commitment does not have", linkObject(p, c2, "deadline", "target", elapsedOffset(1)), "links:role"},
		{"an unknown offset mode", offsetWith(map[string]any{"mode": "weird"}), "links:offset"},
		{"an elapsed offset with a zone", offsetWith(map[string]any{"mode": "elapsed", "elapsed_secs": 5, "zone": "UTC"}), "links:offset"},
		{"a civil offset with no zone", offsetWith(map[string]any{"mode": "civil", "days": 1}), "links:offset"},
		{"a civil offset in a zone that does not exist", offsetWith(map[string]any{"mode": "civil", "days": 1, "zone": "Mars/Phobos"}), "links:offset"},
		{"a clock past the end of the day", offsetWith(map[string]any{"mode": "civil", "days": 1, "zone": "UTC", "clock_secs": 90000}), "links:offset"},
		{"an all-day offset with a clock", offsetWith(map[string]any{"mode": "civil", "days": 1, "zone": "UTC", "clock_secs": 60, "all_day": true}), "links:offset"},
		{"an elapsed offset past the supported span", offsetWith(elapsedOffset(400 * 366 * 86400)), "links:offset"},
		{"a civil offset no parent date could take", offsetWith(map[string]any{"mode": "civil", "days": 100000, "zone": "UTC"}), "links:offset"},
		{"an elapsed offset that lands past 2200", offsetWith(elapsedOffset(200 * 365 * 86400)), "links:offset"},
		{"an elapsed offset that lands before 1970", offsetWith(elapsedOffset(-70 * 365 * 86400)), "links:offset"},
		{"a civil offset that lands past 2200", offsetWith(map[string]any{"mode": "civil", "days": 80000, "zone": "UTC"}), "links:offset"},
		{"a created_at that is not a time", with(linkObject(p, c2, "target", "target", elapsedOffset(1)), "created_at", "yesterday"), "links:invalid"},
		{"an updated_at that is empty", with(linkObject(p, c2, "target", "target", elapsedOffset(1)), "updated_at", ""), "links:invalid"},
	} {
		t.Run(c.name, func(t *testing.T) {
			bad := c.bad
			bad["overrides"] = []any{goodOverride()}
			l2 := linkObject(c1, c2, "target", "target", elapsedOffset(3600))
			ws, rep := importWithLinks(t, f, nil, good(), l2, bad)
			if rep.DroppedSchedules == nil || len(rep.DroppedSchedules) != 1 || rep.DroppedSchedules[0].Reason != c.want {
				t.Fatalf("report=%+v, want exactly one drop %q", rep.DroppedSchedules, c.want)
			}
			if len(rep.DroppedSchedules[0].Item) == 0 && c.want != "links:orphan" {
				t.Fatalf("the drop names no item: %+v", rep.DroppedSchedules[0])
			}
			if got := f.count(t, `SELECT COUNT(*) FROM schedule_links WHERE workspace_id=?`, ws.ID); got != 2 {
				t.Fatalf("%d links imported, want the 2 valid ones", got)
			}
			if got := f.count(t, `SELECT COUNT(*) FROM schedule_link_overrides o JOIN schedule_links l ON l.id=o.link_id WHERE l.workspace_id=?`, ws.ID); got != 0 {
				t.Fatalf("%d overrides imported, want none: they go with the dropped link", got)
			}
		})
	}
}

func TestBundleLinks_ImportReportsLinksOfASkippedSchedule(t *testing.T) {
	f := newLinkBundleFix(t)
	f.link(t, 0, 1, "target", "target", oneDay)
	f.link(t, 1, 2, "target", "target", oneDay)
	f.linkOverrideRow(t, f.storedLinkID(t, 0, 1), "2030-03-09T09:00:00Z|2030-03-09T09:00:00", nil, "2030-03-04T12:00:00Z", "2030-03-04T12:00:00Z", "2030-03-04T12:00:00Z")
	b := f.exportBundle(t)
	var c1Item string
	for _, it := range b.Items {
		if it.Title == "C1" {
			c1Item = it.ID
		}
	}
	for i := range b.Schedules {
		if b.Schedules[i].ItemID == c1Item {
			b.Schedules[i].Rrule = "FREQ=HOURLY;BYSETPOS=1" // the import refuses this schedule
		}
	}
	ws, rep := f.importBundle(t, b, "Restored without C1's schedule")
	var links []models.ImportScheduleDrop
	for _, d := range rep.DroppedSchedules {
		if strings.HasPrefix(d.Reason, "links:") {
			links = append(links, d)
		}
	}
	if len(links) != 2 || links[0].Reason != "links:orphan" || links[1].Reason != "links:orphan" {
		t.Fatalf("report=%+v, want both links of the skipped schedule reported as orphan", rep.DroppedSchedules)
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_links WHERE workspace_id=?`, ws.ID) != 0 || f.count(t, `SELECT COUNT(*) FROM schedule_link_overrides o JOIN schedule_links l ON l.id=o.link_id WHERE l.workspace_id=?`, ws.ID) != 0 {
		t.Fatal("links of a skipped schedule were imported")
	}
}

func TestBundleLinks_ImportDropsABadOverrideAndKeepsTheLink(t *testing.T) {
	f := newLinkBundleFix(t)
	ids := bundleScheduleIDs(t, f.exportBundle(t))
	key := func(k string) map[string]any { o := goodOverride(); o["occurrence_key"] = k; return o }
	neither := goodOverride()
	delete(neither, "no_longer_needed_at")
	badOffset := goodOverride()
	badOffset["occurrence_key"] = "2030-03-10T09:00:00Z|2030-03-10T09:00:00"
	badOffset["offset"] = map[string]any{"mode": "civil", "days": 1}
	badStamp := key("2030-03-11T09:00:00Z|2030-03-11T09:00:00")
	badStamp["created_at"] = "later"
	badGone := key("2030-03-12T09:00:00Z|2030-03-12T09:00:00")
	badGone["no_longer_needed_at"] = "soon"
	l := linkObject(ids["P"], ids["C1"], "target", "target", elapsedOffset(86400))
	l["overrides"] = []any{goodOverride(), key("not a key"), neither, badOffset, badStamp, badGone, goodOverride()}
	ws, rep := importWithLinks(t, f, nil, l)
	var reasons []string
	for _, d := range rep.DroppedSchedules {
		reasons = append(reasons, d.Reason)
	}
	sort.Strings(reasons)
	want := []string{"link_overrides:duplicate", "link_overrides:invalid", "link_overrides:invalid", "link_overrides:invalid", "link_overrides:invalid", "link_overrides:invalid"}
	if !reflect.DeepEqual(reasons, want) {
		t.Fatalf("reasons=%q, want %q", reasons, want)
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_links WHERE workspace_id=?`, ws.ID) != 1 || f.count(t, `SELECT COUNT(*) FROM schedule_link_overrides o JOIN schedule_links l ON l.id=o.link_id WHERE l.workspace_id=?`, ws.ID) != 1 {
		t.Fatal("want the link and only its one good override")
	}
}

func TestBundleLinks_ImportChecksTheFiftyAndSixteenLimits(t *testing.T) {
	t.Run("hops", func(t *testing.T) {
		f := newLinkFix(t, 19)
		var links []map[string]any
		var sched []string
		for _, sc := range f.scheds {
			sched = append(sched, sc.ID)
		}
		for i := 0; i < 17; i++ {
			links = append(links, linkObject(sched[i], sched[i+1], "target", "target", elapsedOffset(60)))
		}
		ws, rep := importWithLinks(t, f, nil, links...)
		if len(rep.DroppedSchedules) != 1 || rep.DroppedSchedules[0].Reason != "links:limit" {
			t.Fatalf("report=%+v, want the 17th hop refused", rep.DroppedSchedules)
		}
		if n := f.count(t, `SELECT COUNT(*) FROM schedule_links WHERE workspace_id=?`, ws.ID); n != 16 {
			t.Fatalf("%d links imported, want 16", n)
		}
	})
	t.Run("affected", func(t *testing.T) {
		f := newLinkFix(t, 53)
		var links []map[string]any
		for i := 1; i <= 51; i++ {
			links = append(links, linkObject(f.scheds[0].ID, f.scheds[i].ID, "target", "target", elapsedOffset(60)))
		}
		ws, rep := importWithLinks(t, f, nil, links...)
		if len(rep.DroppedSchedules) != 1 || rep.DroppedSchedules[0].Reason != "links:limit" {
			t.Fatalf("report=%+v, want the 51st dependent refused", rep.DroppedSchedules)
		}
		if n := f.count(t, `SELECT COUNT(*) FROM schedule_links WHERE workspace_id=?`, ws.ID); n != 50 {
			t.Fatalf("%d links imported, want 50", n)
		}
	})
}

func TestBundleLinks_ImportIsAtomicWithTheSchedules(t *testing.T) {
	f := newLinkBundleFix(t)
	if f.s.dialect.Driver() != DriverSQLite {
		t.Skip("SQLite fault injection")
	}
	f.richLinks(t)
	b := f.exportBundle(t)
	f.exec(t, `CREATE TRIGGER link_override_fail BEFORE INSERT ON schedule_link_overrides WHEN NEW.occurrence_key LIKE '2030-03-09%' BEGIN SELECT RAISE(ABORT,'override refused'); END`)
	ws0, sc0, ln0, ov0, rm0 := f.count(t, `SELECT COUNT(*) FROM workspaces`), f.count(t, `SELECT COUNT(*) FROM item_schedules`), f.count(t, `SELECT COUNT(*) FROM schedule_links`), f.count(t, `SELECT COUNT(*) FROM schedule_link_overrides`), f.count(t, `SELECT COUNT(*) FROM item_reminders`)
	_, _, err := f.s.ImportWorkspaceWithReport(b, "Atomic", "", "api")
	if err == nil || !strings.Contains(err.Error(), "override refused") {
		t.Fatalf("err=%v", err)
	}
	if f.count(t, `SELECT COUNT(*) FROM workspaces`) != ws0 || f.count(t, `SELECT COUNT(*) FROM item_schedules`) != sc0 || f.count(t, `SELECT COUNT(*) FROM schedule_links`) != ln0 ||
		f.count(t, `SELECT COUNT(*) FROM schedule_link_overrides`) != ov0 || f.count(t, `SELECT COUNT(*) FROM item_reminders`) != rm0 {
		t.Fatal("a failed link write left part of the import behind")
	}
}

// A parent with no outstanding occurrence has no date to apply an offset to, so
// the check falls back to whether ANY supported date could take it.
func TestBundleLinks_ImportChecksAnOffsetWhenTheParentHasNoDate(t *testing.T) {
	f := newLinkBundleFix(t)
	ended := func(m map[string]any, ids map[string]string) {
		for _, v := range m["schedules"].([]any) {
			x := v.(map[string]any)
			if x["id"] == ids["P"] {
				x["ended_at"] = "2030-03-05T00:00:00Z"
				delete(x, "next_at")
				delete(x, "next_wall")
				delete(x, "reminder")
			}
		}
	}
	ids := bundleScheduleIDs(t, f.exportBundle(t))
	reach := linkObject(ids["P"], ids["C1"], "target", "target", map[string]any{"mode": "civil", "days": 100000, "zone": "UTC"})
	fine := linkObject(ids["P"], ids["C2"], "target", "target", elapsedOffset(86400))
	ws, rep := importWithLinks(t, f, ended, reach, fine)
	if len(rep.DroppedSchedules) != 1 || rep.DroppedSchedules[0].Reason != "links:offset" {
		t.Fatalf("report=%+v, want only the offset no date could take refused", rep.DroppedSchedules)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_links WHERE workspace_id=?`, ws.ID); n != 1 {
		t.Fatalf("%d links imported, want the one with a usable offset", n)
	}
}

// An override key is the child's occurrence key, and a floating child keys by
// wall alone, so rows written while it was pinned can meet on one key: the later
// edit wins and the other is reported.
func TestBundleLinks_ImportKeysOverridesByTheChildsFloatingPolicy(t *testing.T) {
	f := newLinkBundleFix(t)
	ids := bundleScheduleIDs(t, f.exportBundle(t))
	floating := func(m map[string]any, ids map[string]string) {
		for _, v := range m["schedules"].([]any) {
			if x := v.(map[string]any); x["id"] == ids["C1"] {
				x["floating"] = true
				delete(x, "tz")
				delete(x, "next_at")
				delete(x, "reminder")
				x["dtstart"] = "2030-03-05T09:00:00"
			}
		}
	}
	older, newer := goodOverride(), goodOverride()
	older["occurrence_key"], older["updated_at"], older["no_longer_needed_at"] = "2030-03-05T09:00:00Z|2030-03-05T09:00:00", "2030-03-04T12:00:00Z", "2030-03-04T12:00:00Z"
	newer["occurrence_key"], newer["updated_at"], newer["no_longer_needed_at"] = "2030-03-05T09:00:00", "2030-03-06T12:00:00Z", "2030-03-06T12:00:00Z"
	l := linkObject(ids["P"], ids["C1"], "target", "target", elapsedOffset(86400))
	l["overrides"] = []any{older, newer}
	ws, rep := importWithLinks(t, f, floating, l)
	if len(rep.DroppedSchedules) != 1 || rep.DroppedSchedules[0].Reason != "link_overrides:duplicate" {
		t.Fatalf("report=%+v, want the one duplicate", rep.DroppedSchedules)
	}
	tuples := f.linkTuples(t, ws.ID)
	if len(tuples) != 2 || !strings.Contains(tuples[1], "override 2030-03-05T09:00:00 -/0/0/0/0/ gone=2030-03-06T12:00:00Z") {
		t.Fatalf("want one override keyed by the wall holding the later edit, got %q", tuples)
	}
}

// Every offset column an import stores fits the column and what a live write
// accepts: offset_days is a 32-bit INTEGER on Postgres, and a value past it used
// to fail the statement there and roll the whole import back (SQLite stored it
// silently). Such a link, or such an override row, is dropped before any insert.
func TestBundleLinks_ImportBoundsEveryStoredOffsetColumn(t *testing.T) {
	const past = 3_000_000_000
	f := newLinkBundleFix(t)
	ids := bundleScheduleIDs(t, f.exportBundle(t))
	bad := map[string]map[string]any{
		"elapsed days past int32":      {"mode": "elapsed", "elapsed_secs": 60, "days": past},
		"elapsed days below int32":     {"mode": "elapsed", "elapsed_secs": 60, "days": -past},
		"civil days past int32":        {"mode": "civil", "days": past, "zone": "UTC"},
		"civil days below int32":       {"mode": "civil", "days": -past, "zone": "UTC"},
		"elapsed seconds past the cap": {"mode": "elapsed", "elapsed_secs": 400 * 366 * 86400},
		"clock past the day":           {"mode": "civil", "days": 1, "zone": "UTC", "clock_secs": 86400},
		"negative clock":               {"mode": "civil", "days": 1, "zone": "UTC", "clock_secs": -1},
	}
	const outOfColumn = `offset_days > 2147483647 OR offset_days < -2147483648 OR offset_clock < 0 OR offset_clock >= 86400`
	for name, off := range bad {
		t.Run("link/"+name, func(t *testing.T) {
			ws, rep := importWithLinks(t, f, nil, linkObject(ids["P"], ids["C1"], "target", "target", off))
			if len(rep.DroppedSchedules) != 1 || rep.DroppedSchedules[0].Reason != "links:offset" {
				t.Fatalf("report=%+v, want one links:offset", rep.DroppedSchedules)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM schedule_links WHERE workspace_id=?`, ws.ID); n != 0 {
				t.Fatalf("%d links stored", n)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM schedule_links WHERE workspace_id=? AND (`+outOfColumn+`)`, ws.ID); n != 0 {
				t.Fatalf("%d stored links carry an offset the column cannot hold", n)
			}
		})
		t.Run("override/"+name, func(t *testing.T) {
			l := linkObject(ids["P"], ids["C1"], "target", "target", elapsedOffset(60))
			o := goodOverride()
			o["offset"] = off
			l["overrides"] = []any{o}
			ws, rep := importWithLinks(t, f, nil, l)
			if len(rep.DroppedSchedules) != 1 || rep.DroppedSchedules[0].Reason != "link_overrides:invalid" {
				t.Fatalf("report=%+v, want one link_overrides:invalid", rep.DroppedSchedules)
			}
			if f.count(t, `SELECT COUNT(*) FROM schedule_links WHERE workspace_id=?`, ws.ID) != 1 ||
				f.count(t, `SELECT COUNT(*) FROM schedule_link_overrides o JOIN schedule_links l ON l.id=o.link_id WHERE l.workspace_id=?`, ws.ID) != 0 {
				t.Fatal("want the link kept and its bad override not stored")
			}
			if n := f.count(t, `SELECT COUNT(*) FROM schedule_link_overrides WHERE link_id IN (SELECT id FROM schedule_links WHERE workspace_id=?) AND (`+outOfColumn+`)`, ws.ID); n != 0 {
				t.Fatalf("%d stored overrides carry an offset the column cannot hold", n)
			}
		})
	}
	// The largest value the column holds still imports, in both tables.
	t.Run("the column's limit is accepted", func(t *testing.T) {
		edge := map[string]any{"mode": "elapsed", "elapsed_secs": 60, "days": math.MaxInt32}
		l := linkObject(ids["P"], ids["C1"], "target", "target", edge)
		o := goodOverride()
		o["offset"] = map[string]any{"mode": "elapsed", "elapsed_secs": 60, "days": math.MinInt32}
		l["overrides"] = []any{o}
		ws, rep := importWithLinks(t, f, nil, l)
		if len(rep.DroppedSchedules) != 0 {
			t.Fatalf("report=%+v", rep.DroppedSchedules)
		}
		if f.count(t, `SELECT COUNT(*) FROM schedule_links WHERE workspace_id=? AND offset_days=2147483647`, ws.ID) != 1 ||
			f.count(t, `SELECT COUNT(*) FROM schedule_link_overrides WHERE offset_days=-2147483648`) != 1 {
			t.Fatal("the column limits were not stored")
		}
	})
}

// Only a version 1 bundle with NOTHING but schedule_links reaches the version 3
// gate (any other v1 bundle with schedules is refused first as carrying version 2
// members), so this is what pins that the gate covers version 1 as well as 2.
func TestBundleLinks_AVersion1BundleWithOnlyLinksIsRefused(t *testing.T) {
	f := newLinkBundleFix(t)
	f.link(t, 0, 1, "target", "target", oneDay)
	for name, member := range map[string]any{"a link": []any{linkObject("a", "b", "target", "target", elapsedOffset(60))}, "an empty member": []any{}, "null": nil} {
		t.Run(name, func(t *testing.T) {
			v1 := f.exportBundle(t)
			v1.ScheduleLinks = nil
			stripV2(v1)
			m := bundleObject(t, v1)
			m["schedule_links"] = member
			data := bundleDecode(t, m)
			if data.HasV2ScheduleMembers() {
				t.Fatal("the premise failed: this bundle must carry no version 2 member")
			}
			n := f.count(t, `SELECT COUNT(*) FROM workspaces`)
			_, _, err := f.s.ImportWorkspaceWithReport(data, "Refused", "", "api")
			if err == nil || !strings.Contains(err.Error(), "export version 1 contains version 3 schedule members") {
				t.Fatalf("err=%v", err)
			}
			if f.count(t, `SELECT COUNT(*) FROM workspaces`) != n {
				t.Fatal("husk workspace")
			}
		})
	}
}

// An override is checked against the parent's current date as the link is: a
// later linked move would otherwise fail on the edge.
func TestBundleLinks_ImportChecksOverrideOffsetsAgainstTheParentDate(t *testing.T) {
	f := newLinkBundleFix(t)
	ids := bundleScheduleIDs(t, f.exportBundle(t))
	// P's outstanding target is the last second the package supports.
	last := func(m map[string]any, ids map[string]string) {
		for _, v := range m["schedules"].([]any) {
			if x := v.(map[string]any); x["id"] == ids["P"] {
				x["dtstart"], x["next_wall"], x["next_at"] = "2200-12-31T23:59:59", "2200-12-31T23:59:59", "2200-12-31T23:59:59Z"
			}
		}
	}
	l := linkObject(ids["P"], ids["C1"], "target", "target", elapsedOffset(0))
	later, same := goodOverride(), goodOverride()
	later["occurrence_key"], later["offset"] = "2030-03-10T09:00:00Z|2030-03-10T09:00:00", elapsedOffset(60)
	same["occurrence_key"], same["offset"] = "2030-03-11T09:00:00Z|2030-03-11T09:00:00", elapsedOffset(0)
	l["overrides"] = []any{later, same}
	ws, rep := importWithLinks(t, f, last, l)
	if len(rep.DroppedSchedules) != 1 || rep.DroppedSchedules[0].Reason != "link_overrides:invalid" {
		t.Fatalf("report=%+v, want only the +60s override dropped", rep.DroppedSchedules)
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_links WHERE workspace_id=?`, ws.ID) != 1 ||
		f.count(t, `SELECT COUNT(*) FROM schedule_link_overrides o JOIN schedule_links l ON l.id=o.link_id WHERE l.workspace_id=?`, ws.ID) != 1 {
		t.Fatal("want the link and the one override that applies")
	}
}

// The cascade also refuses a derived date that reads as a year past 2200 in the
// CHILD's own zone (commitment_cascade.go planLinkedTx), whatever zone the
// offset used. A link or override that would trip it on the next linked move is
// dropped at import. 2200-12-31T20:00Z is still 2200 in UTC, 2201-01-01 05:00 in Tokyo.
func TestBundleLinks_ImportMirrorsTheChildZoneYearBackstop(t *testing.T) {
	f := newLinkBundleFix(t)
	ids := bundleScheduleIDs(t, f.exportBundle(t))
	edit := func(m map[string]any, ids map[string]string) {
		for _, v := range m["schedules"].([]any) {
			switch x := v.(map[string]any); x["id"] {
			case ids["P"]:
				x["dtstart"], x["next_wall"], x["next_at"] = "2200-12-31T20:00:00", "2200-12-31T20:00:00", "2200-12-31T20:00:00Z"
			case ids["C1"]:
				x["tz"] = "Asia/Tokyo"
			}
		}
	}
	t.Run("link", func(t *testing.T) {
		ws, rep := importWithLinks(t, f, edit, linkObject(ids["P"], ids["C1"], "target", "target", elapsedOffset(0)))
		if len(rep.DroppedSchedules) != 1 || rep.DroppedSchedules[0].Reason != "links:offset" {
			t.Fatalf("report=%+v, want links:offset", rep.DroppedSchedules)
		}
		if f.count(t, `SELECT COUNT(*) FROM schedule_links WHERE workspace_id=?`, ws.ID) != 0 {
			t.Fatal("the link was stored")
		}
	})
	t.Run("override", func(t *testing.T) {
		l := linkObject(ids["P"], ids["C1"], "target", "target", elapsedOffset(-86400))
		o := goodOverride()
		o["offset"] = elapsedOffset(0)
		l["overrides"] = []any{o}
		ws, rep := importWithLinks(t, f, edit, l)
		if len(rep.DroppedSchedules) != 1 || rep.DroppedSchedules[0].Reason != "link_overrides:invalid" {
			t.Fatalf("report=%+v, want link_overrides:invalid", rep.DroppedSchedules)
		}
		if f.count(t, `SELECT COUNT(*) FROM schedule_links WHERE workspace_id=?`, ws.ID) != 1 {
			t.Fatal("the link was not kept")
		}
	})
}

// A parent with no outstanding occurrence is an expected non-error (there is no
// date to check); a failure to read the parent's dates is a real error and the
// import fails with it, instead of being swallowed (on Postgres a failed SELECT
// poisons the transaction and the next statement would fail with 25P02).
func TestBundleLinks_ImportSurfacesAFailureToReadTheParentsDates(t *testing.T) {
	f := newLinkBundleFix(t)
	if f.s.dialect.Driver() != DriverSQLite {
		t.Skip("SQLite fault injection")
	}
	src := f.exportBundle(t)
	ids := bundleScheduleIDs(t, src)
	link := linkObject(ids["P"], ids["C1"], "target", "target", elapsedOffset(60))
	ended := func(m map[string]any) {
		for _, v := range m["schedules"].([]any) {
			if x := v.(map[string]any); x["id"] == ids["P"] {
				x["ended_at"] = "2030-03-05T00:00:00Z"
				delete(x, "next_at")
				delete(x, "next_wall")
				delete(x, "reminder")
			}
		}
	}
	// Reading a parent's outstanding dates reads its occurrence overrides.
	f.exec(t, `ALTER TABLE schedule_occurrence_overrides RENAME TO schedule_occurrence_overrides_hidden`)
	try := func(mutate func(map[string]any)) (*models.Workspace, ImportReport, error) {
		m := bundleObject(t, src)
		m["schedule_links"] = []any{link}
		if mutate != nil {
			mutate(m)
		}
		return f.s.ImportWorkspaceWithReport(bundleDecode(t, m), "Reading", "", "api")
	}
	t.Run("no outstanding occurrence is not an error", func(t *testing.T) {
		ws, rep, err := try(ended)
		if err != nil || len(rep.DroppedSchedules) != 0 || f.count(t, `SELECT COUNT(*) FROM schedule_links WHERE workspace_id=?`, ws.ID) != 1 {
			t.Fatalf("report=%+v err=%v", rep.DroppedSchedules, err)
		}
	})
	t.Run("a failed read fails the import", func(t *testing.T) {
		n := f.count(t, `SELECT COUNT(*) FROM workspaces`)
		_, _, err := try(nil)
		if err == nil || !strings.Contains(err.Error(), "schedule_occurrence_overrides") {
			t.Fatalf("err=%v, want the failed read of the parent's overrides", err)
		}
		if f.count(t, `SELECT COUNT(*) FROM workspaces`) != n {
			t.Fatal("husk workspace")
		}
	})
}
