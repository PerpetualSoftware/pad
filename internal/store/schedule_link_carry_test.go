package store

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Time links in the workspace bundle (TASK-4353, PR 2b). Export carries the
// links and their per-occurrence overrides; import remaps both ends through the
// schedule map, re-checks every link as a live write does, drops and reports a
// bad one, and never moves a date. Each test runs on SQLite in make test and on
// Postgres in make test-pg.

var (
	civilNY = models.ScheduleLinkOffset{Mode: "civil", Days: 2, ClockSecs: 9*3600 + 1800, Zone: "America/New_York"}
	minusHr = models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: -3600}
)

// newLinkBundleFix is four commitments titled P, C1, C2 and D, so a restored
// workspace can be read by name.
func newLinkBundleFix(t *testing.T) *linkFix {
	t.Helper()
	f := newLinkFix(t, 4)
	for i, title := range []string{"P", "C1", "C2", "D"} {
		f.exec(t, `UPDATE items SET title=? WHERE id=?`, title, f.items[i].ID)
	}
	return f
}

// linkOverrideRow stores an override with fixed stamps, bypassing the writers
// so the test owns every column.
func (f *schedFixture) linkOverrideRow(t *testing.T, linkID, key string, off *models.ScheduleLinkOffset, gone, created, updated string) {
	t.Helper()
	var mode any
	o := models.ScheduleLinkOffset{}
	if off != nil {
		o, mode = *off, off.Mode
	}
	allDay := 0
	if o.AllDay {
		allDay = 1
	}
	f.exec(t, `INSERT INTO schedule_link_overrides (id,link_id,occurrence_key,offset_mode,offset_elapsed,offset_days,offset_clock,offset_all_day,offset_zone,no_longer_needed_at,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		newID(), linkID, key, mode, o.ElapsedSecs, o.Days, o.ClockSecs, allDay, o.Zone, nilIfEmpty(gone), created, updated)
}

// storedLink reads the one link between two items' schedules.
func (f *linkFix) storedLinkID(t *testing.T, parent, child int) string {
	t.Helper()
	var id string
	if err := f.s.db.QueryRow(f.s.q(`SELECT id FROM schedule_links WHERE parent_schedule_id=? AND child_schedule_id=?`), f.scheds[parent].ID, f.scheds[child].ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// richLinks writes the links and overrides every round-trip test starts from:
// a civil link and an elapsed one with a cancel policy, both with overrides.
func (f *linkFix) richLinks(t *testing.T) {
	t.Helper()
	f.link(t, 0, 1, "target", "target", civilNY)
	f.link(t, 1, 2, "target", "target", minusHr)
	policy := models.LinkCancelChildOccurrence
	if _, err := f.s.UpdateScheduleLink(f.ws.ID, f.storedLinkID(t, 1, 2), models.ScheduleLinkUpdate{CancelPolicy: &policy}, f.allEdit()); err != nil {
		t.Fatal(err)
	}
	london := models.ScheduleLinkOffset{Mode: "civil", Days: -1, ClockSecs: 8 * 3600, Zone: "Europe/London"}
	twoHrs := models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: 7200}
	c1, c2 := f.reload(t, f.items[1].ID), f.reload(t, f.items[2].ID)
	f.linkOverrideRow(t, f.storedLinkID(t, 0, 1), OccurrenceKey(c1, *c1.NextAt, *c1.NextWall), &london, "", "2030-03-01T10:00:00.123456789Z", "2030-03-02T11:00:00.987654321Z")
	f.linkOverrideRow(t, f.storedLinkID(t, 1, 2), OccurrenceKey(c2, *c2.NextAt, *c2.NextWall), nil, "2030-03-03T12:00:00.5Z", "2030-03-03T12:00:00.5Z", "2030-03-03T12:00:00.5Z")
	f.linkOverrideRow(t, f.storedLinkID(t, 1, 2), "2030-03-09T09:00:00Z|2030-03-09T09:00:00", &twoHrs, "2030-03-04T12:00:00Z", "2030-03-04T12:00:00Z", "2030-03-04T12:30:00Z")
	// A deadline link, stored without the move a write makes.
	f.exec(t, `UPDATE item_schedules SET roles_origin='native', deadline_days=2, deadline_secs=0, deadline_behavior='stays_owed' WHERE id IN (?,?)`, f.scheds[0].ID, f.scheds[3].ID)
	f.linkRaw(t, 0, 3, "deadline", "deadline", models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: 172800})
}

// bundleScheduleIDs maps item title to source schedule id in a bundle.
func bundleScheduleIDs(t *testing.T, b *models.WorkspaceExport) map[string]string {
	t.Helper()
	titles := map[string]string{}
	for _, it := range b.Items {
		titles[it.ID] = it.Title
	}
	out := map[string]string{}
	for _, sc := range b.Schedules {
		out[titles[sc.ItemID]] = sc.ID
	}
	return out
}

func TestExportVersionIsThree(t *testing.T) {
	f := newSchedFixture(t)
	f.create(t, dailyUTC)
	if got := f.exportBundle(t).Version; got != 3 {
		t.Fatalf("export version=%d, want 3: a reader that predates links must refuse the bundle, not drop them", got)
	}
}

func TestBundleLinks_ExportCarriesLinksAndOverrides(t *testing.T) {
	f := newLinkBundleFix(t)
	f.richLinks(t)
	b := f.exportBundle(t)
	ids := bundleScheduleIDs(t, b)
	if len(b.ScheduleLinks) != 3 {
		t.Fatalf("bundle carries %d links, want 3", len(b.ScheduleLinks))
	}
	byChild := map[string]models.ScheduleLinkExport{}
	for _, l := range b.ScheduleLinks {
		byChild[l.ChildScheduleID+"."+l.ChildRole] = l
	}
	l1 := byChild[ids["C1"]+".target"]
	if l1.ParentScheduleID != ids["P"] || l1.ParentRole != "target" || !reflect.DeepEqual(l1.Offset, civilNY) || l1.CancelPolicy != models.LinkWaitingOn || len(l1.Overrides) != 1 {
		t.Fatalf("civil link: %+v", l1)
	}
	if o := l1.Overrides[0]; o.Offset == nil || o.Offset.Zone != "Europe/London" || o.Offset.Days != -1 || o.NoLongerNeededAt != "" || o.CreatedAt != "2030-03-01T10:00:00.123456789Z" || o.UpdatedAt != "2030-03-02T11:00:00.987654321Z" {
		t.Fatalf("offset override: %+v", o)
	}
	l2 := byChild[ids["C2"]+".target"]
	if l2.ParentScheduleID != ids["C1"] || !reflect.DeepEqual(l2.Offset, minusHr) || l2.CancelPolicy != models.LinkCancelChildOccurrence || len(l2.Overrides) != 2 {
		t.Fatalf("elapsed link: %+v", l2)
	}
	var gone, both int
	for _, o := range l2.Overrides {
		switch {
		case o.Offset == nil && o.NoLongerNeededAt == "2030-03-03T12:00:00.5Z":
			gone++
		case o.Offset != nil && o.Offset.ElapsedSecs == 7200 && o.NoLongerNeededAt == "2030-03-04T12:00:00Z":
			both++
		}
	}
	if gone != 1 || both != 1 {
		t.Fatalf("no-longer-needed overrides: %+v", l2.Overrides)
	}
	if l3 := byChild[ids["D"]+".deadline"]; l3.ParentRole != "deadline" || l3.ParentScheduleID != ids["P"] {
		t.Fatalf("deadline link: %+v", l3)
	}
	if b.OmittedScheduleLinks != 0 {
		t.Fatalf("omitted=%d", b.OmittedScheduleLinks)
	}
}

func TestBundleLinks_ExportOmitsLinksWithAnEndOutsideTheBundle(t *testing.T) {
	f := newLinkBundleFix(t)
	f.link(t, 0, 1, "target", "target", oneDay)
	f.link(t, 1, 2, "target", "target", oneDay)
	f.linkOverrideRow(t, f.storedLinkID(t, 1, 2), "2030-03-09T09:00:00Z|2030-03-09T09:00:00", nil, "2030-03-04T12:00:00Z", "2030-03-04T12:00:00Z", "2030-03-04T12:00:00Z")
	deleted := f.reload(t, f.items[2].ID).ID
	if err := f.s.DeleteItem(f.items[2].ID); err != nil {
		t.Fatal(err)
	}
	data, err := f.s.ExportWorkspace(f.ws.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.ScheduleLinks) != 1 || data.OmittedScheduleLinks != 1 {
		t.Fatalf("links=%d omitted=%d, want 1 and 1", len(data.ScheduleLinks), data.OmittedScheduleLinks)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	// The count is the Go field OmittedScheduleLinks, which marshals under that
	// exact name unless its tag says "-": neither the key nor its value (1, the
	// one link left out) may be in the bundle.
	for _, leak := range []string{deleted, "2030-03-09T09:00:00", "OmittedScheduleLinks", `"omitted`, `"Omitted`} {
		if bytes.Contains(raw, []byte(leak)) {
			t.Fatalf("the bundle names %q for a link left out: %s", leak, raw)
		}
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatal(err)
	}
	for key := range members {
		if strings.Contains(strings.ToLower(key), "omitted") {
			t.Fatalf("the bundle has a member %q that counts the links left out", key)
		}
	}

	// The rule is on the set of schedules in the bundle, so a narrower set (a
	// subset export) keeps only the links whose BOTH ends are in it.
	q := f.s.db
	for name, c := range map[string]struct {
		in             []int
		links, omitted int
	}{
		"both ends":       {[]int{0, 1}, 1, 1},
		"child only":      {[]int{1}, 0, 2},
		"parent only":     {[]int{0}, 0, 2},
		"neither":         {[]int{3}, 0, 2},
		"the whole chain": {[]int{0, 1, 2}, 2, 0},
	} {
		set := map[string]bool{}
		for _, i := range c.in {
			set[f.scheds[i].ID] = true
		}
		links, omitted, err := f.s.exportScheduleLinksQ(q, f.ws.ID, set)
		if err != nil {
			t.Fatal(err)
		}
		if len(links) != c.links || omitted != c.omitted {
			t.Errorf("%s: links=%d omitted=%d, want %d and %d", name, len(links), omitted, c.links, c.omitted)
		}
	}
}
