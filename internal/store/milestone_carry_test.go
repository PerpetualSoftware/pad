package store

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4358: milestone members in export/import, copy/move and the schema.

// seedMilestone gives f's item a forecast (set twice), a child, a waiver and a
// resolved occurrence with evidence.
func (m *msFix) seedFull(t *testing.T) (child, waived *models.Item) {
	t.Helper()
	child, waived = m.child(t, m.declared, "kid"), m.child(t, m.declared, "waived")
	m.sel(t, child)
	if err := m.s.SetMilestoneChild(m.ws.ID, m.ms.ID, waived.ID, "waiver", "out of scope", "u1"); err != nil {
		t.Fatal(err)
	}
	m.setStatus(t, child, "shipped")
	if err := m.s.SetForecast(m.ws.ID, m.ms.ID, "u1", "user", "2026-12-01", "first"); err != nil {
		t.Fatal(err)
	}
	if err := m.s.SetForecast(m.ws.ID, m.ms.ID, "u2", "agent", "2026-12-20", "second"); err != nil {
		t.Fatal(err)
	}
	m.resolveDone(t)
	return child, waived
}

// RED if export omits a member or import fails to remap or store one.
func TestMilestoneExportImportRoundTrip(t *testing.T) {
	t.Parallel()
	m := newMsFix(t)
	m.seedFull(t)
	data := m.exportBundle(t)
	var x models.ScheduleExport
	for _, sx := range data.Schedules {
		if sx.ItemID == m.ms.ID {
			x = sx
		}
	}
	if x.Forecast == nil || len(x.ForecastEvents) != 2 || len(x.Children) != 2 || len(x.CompletionEvidence) != 2 {
		t.Fatalf("export lacks members: %+v", x)
	}
	if data.Version != models.WorkspaceExportVersion {
		t.Fatalf("bundle version moved to %d", data.Version)
	}
	ws, rep := m.importBundle(t, data, "Round trip")
	if len(rep.DroppedSchedules) != 0 {
		t.Fatalf("drops: %+v", rep.DroppedSchedules)
	}
	dst := m.itemByTitle(t, ws.ID, m.ms.Title)
	c, err := m.s.GetCommitment(ws.ID, dst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Forecast == nil || c.Forecast.Value != "2026-12-20" || c.Forecast.SetBy != "u2" || c.Forecast.SetByKind != "agent" || len(c.Forecast.History) != 2 {
		t.Fatalf("forecast: %+v", c.Forecast)
	}
	if c.Milestone.Total != 2 || c.Milestone.Waivers != 1 || len(c.Milestone.Evidence) != 2 || result(c) != "completed" || *c.Milestone.EvidenceChanged {
		t.Fatalf("milestone: %+v", c.Milestone)
	}
	kid := m.itemByTitle(t, ws.ID, "kid")
	if n := m.count(t, `SELECT COUNT(*) FROM milestone_children WHERE child_item_id=? AND workspace_id=?`, kid.ID, ws.ID); n != 1 {
		t.Fatalf("child not remapped into the new workspace: %d", n)
	}
}

// RED if an orphaned child is kept or silent: it is dropped and reported.
func TestMilestoneImportDropsOrphanChildren(t *testing.T) {
	t.Parallel()
	m := newMsFix(t)
	kid, _ := m.seedFull(t)
	data := m.exportBundle(t)
	items := data.Items[:0]
	for _, it := range data.Items {
		if it.ID != kid.ID {
			items = append(items, it)
		}
	}
	data.Items = items
	ws, rep := m.importBundle(t, data, "Orphan")
	found := false
	for _, d := range rep.DroppedSchedules {
		found = found || d.Reason == "children:orphan"
	}
	if !found {
		t.Fatalf("no children:orphan report: %+v", rep.DroppedSchedules)
	}
	dst := m.itemByTitle(t, ws.ID, m.ms.Title)
	c, err := m.s.GetCommitment(ws.ID, dst.ID)
	if err != nil || c.Milestone.Total != 1 {
		t.Fatalf("surviving children: %+v %v", c.Milestone, err)
	}
}

// RED if import trusts the bundle's text.
func TestMilestoneImportRevalidates(t *testing.T) {
	t.Parallel()
	m := newMsFix(t)
	m.seedFull(t)
	data := m.exportBundle(t)
	for i := range data.Schedules {
		if data.Schedules[i].ItemID == m.ms.ID {
			data.Schedules[i].Forecast.Value = "someday"
			data.Schedules[i].Children[0].Kind = "bogus"
			data.Schedules[i].CompletionEvidence[0].CloseState = "bogus"
		}
	}
	ws, rep := m.importBundle(t, data, "Bad")
	reasons := map[string]bool{}
	for _, d := range rep.DroppedSchedules {
		reasons[d.Reason] = true
	}
	for _, want := range []string{"forecast:invalid", "children:invalid", "completion_evidence:invalid"} {
		if !reasons[want] {
			t.Errorf("missing drop %s in %+v", want, rep.DroppedSchedules)
		}
	}
	dst := m.itemByTitle(t, ws.ID, m.ms.Title)
	c, _ := m.s.GetCommitment(ws.ID, dst.ID)
	if c.Forecast != nil || c.Milestone.Total != 0 {
		t.Fatalf("invalid members stored: %+v %+v", c.Forecast, c.Milestone)
	}
}

// RED if a v1 bundle carrying a new member is accepted.
func TestMilestoneV1BundleWithNewMembersIsRefused(t *testing.T) {
	t.Parallel()
	for _, member := range []string{"forecast", "forecast_events", "children", "completion_evidence"} {
		t.Run(member, func(t *testing.T) {
			m := newMsFix(t)
			b := bundleObject(t, m.exportBundle(t))
			b["version"] = 1
			delete(b, "schedule_changes")
			sx := b["schedules"].([]any)[0].(map[string]any)
			delete(sx, "id")
			delete(sx, "roles")
			delete(sx, "overrides")
			delete(sx, "occurrences")
			sx[member] = []any{}
			if member == "forecast" {
				sx[member] = map[string]any{"value": "2026-12-01", "set_by": "u", "set_by_kind": "user", "set_at": "2026-11-01T00:00:00Z"}
			}
			n := m.count(t, `SELECT COUNT(*) FROM workspaces`)
			if _, _, err := m.s.ImportWorkspaceWithReport(bundleDecode(t, b), "V1", "", "api"); err == nil {
				t.Fatalf("v1 bundle with %s accepted", member)
			}
			if m.count(t, `SELECT COUNT(*) FROM workspaces`) != n {
				t.Fatal("husk workspace")
			}
		})
	}
}

// RED if the carry drops silently or counts hidden children; a copy and a
// cross-workspace move report on preview and commit alike.
func TestMilestoneCopyAndMoveDropForecastAndChildren(t *testing.T) {
	for _, move := range []bool{false, true} {
		name := "copy"
		if move {
			name = "move"
		}
		t.Run(name, func(t *testing.T) {
			c := newSchedCopy(t)
			sc := c.scheduleFuture(t, daily())
			a := createTestItem(t, c.s, c.wsA.ID, c.colA.ID, "a", "")
			hidden := createTestItem(t, c.s, c.wsA.ID, c.colA.ID, "hidden", "")
			for _, it := range []*models.Item{a, hidden} {
				if err := c.s.SetMilestoneChild(c.wsA.ID, c.src.ID, it.ID, "child", "", "u1"); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.s.SetForecast(c.wsA.ID, c.src.ID, "u1", "user", "2026-12-01", ""); err != nil {
				t.Fatal(err)
			}
			canRead := func(id string) bool { return id != hidden.ID }
			preview, err := c.s.PreviewScheduleCarryFor(c.s.db, c.wsA.ID, c.src.ID, "", c.actor, move, copyNow, canRead)
			if err != nil {
				t.Fatal(err)
			}
			if preview.ForecastDropped != 1 || preview.ChildrenDropped != 1 {
				t.Fatalf("preview counts: %+v (hidden child must not be counted)", preview)
			}
			all, _ := c.s.PreviewScheduleCarry(c.s.db, c.wsA.ID, c.src.ID, "", c.actor, move, copyNow)
			if all.ChildrenDropped != 2 {
				t.Fatalf("an unfiltered preview should count both: %+v", all)
			}
			req := c.req()
			req.SourceItemID, req.ArchiveSource, req.scheduleNow, req.SourceChildReadable = c.src.ID, move, copyNow, canRead
			res := c.copy(t, req)
			if res.Schedule != preview {
				t.Fatalf("commit differs from preview:\n%+v\n%+v", res.Schedule, preview)
			}
			dst := c.sched(t, c.wsB.ID, res.Item.ID)
			if dst == nil || dst.ID == sc.ID {
				t.Fatalf("destination schedule: %+v", dst)
			}
			for _, q := range []string{`SELECT COUNT(*) FROM schedule_forecasts WHERE schedule_id=?`, `SELECT COUNT(*) FROM milestone_children WHERE schedule_id=?`, `SELECT COUNT(*) FROM milestone_completion_evidence WHERE schedule_id=?`} {
				if n := c.count(t, q, dst.ID); n != 0 {
					t.Errorf("%s: destination kept %d", q, n)
				}
			}
		})
	}
}

// A same-workspace move keeps the item, so the forecast and selection stay.
func TestMilestoneSameWorkspaceMoveKeepsForecastAndSelection(t *testing.T) {
	t.Parallel()
	m := newMsFix(t)
	a := m.child(t, m.declared, "a")
	m.sel(t, a)
	if err := m.s.SetForecast(m.ws.ID, m.ms.ID, "u1", "user", "2026-12-01", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.s.MoveItem(m.ms.ID, m.fallback.ID, `{"status":"open"}`); err != nil {
		t.Fatal(err)
	}
	c := m.read(t, nil)
	if c.Forecast == nil || c.Milestone.Total != 1 {
		t.Fatalf("after a same-workspace move: %+v %+v", c.Forecast, c.Milestone)
	}
}

// Migration shape: the partial unique index, the waiver CHECK and the PG twin.
func TestMilestoneMigrationShape(t *testing.T) {
	t.Parallel()
	lite, err := migrationsFS.ReadFile("migrations/170_commitment_milestone.sql")
	if err != nil {
		t.Fatal(err)
	}
	pg, err := pgMigrationsFS.ReadFile("pgmigrations/133_commitment_milestone.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{string(lite), string(pg)} {
		for _, want := range []string{"CREATE TABLE IF NOT EXISTS schedule_forecasts", "CREATE TABLE IF NOT EXISTS schedule_forecast_events",
			"CREATE TABLE IF NOT EXISTS milestone_children", "CREATE TABLE IF NOT EXISTS milestone_completion_evidence",
			"CHECK (kind = 'child' OR reason <> '')", "WHERE removed_at IS NULL", "UNIQUE (schedule_id, occurrence_key, child_item_id)"} {
			if !strings.Contains(text, want) {
				t.Errorf("migration lacks %q", want)
			}
		}
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "--") && strings.Contains(line, "'") {
				t.Errorf("apostrophe in a comment: %q", line)
			}
		}
	}
	if !strings.HasPrefix(string(pg), "SET LOCAL lock_timeout = '5s';") {
		t.Error("the Postgres migration must start with the lock_timeout")
	}
}

// RED if a NUL reaches a free-text column: the write door and the triggers
// both refuse it.
func TestMilestoneNULGuards(t *testing.T) {
	t.Parallel()
	m := newMsFix(t)
	a := m.child(t, m.declared, "a")
	if err := m.s.SetForecast(m.ws.ID, m.ms.ID, "u1", "user", "2026-12-01", "x\x00y"); !hasField(err, "source", "format") {
		t.Fatalf("source NUL: %v", err)
	}
	if err := m.s.SetMilestoneChild(m.ws.ID, m.ms.ID, a.ID, "waiver", "a\x00b", "u1"); !hasField(err, "reason", "format") {
		t.Fatalf("reason NUL: %v", err)
	}
	if m.s.dialect.Driver() != DriverSQLite {
		return
	}
	for _, q := range []struct{ sql string }{
		{`INSERT INTO schedule_forecasts (schedule_id,workspace_id,item_id,value,set_by_kind,set_at,source) VALUES ('s','w','i','2026-01-01','user','t',char(120,0,121))`},
		{`INSERT INTO milestone_children (id,workspace_id,schedule_id,child_item_id,kind,reason,added_at) VALUES ('i','w','s','c','waiver',char(120,0,121),'t')`},
	} {
		_, err := m.s.db.Exec(q.sql)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "nul") {
			t.Errorf("trigger did not refuse a NUL: %v", err)
		}
	}
}

func TestMilestoneBundleJSONShapeIsAdditive(t *testing.T) {
	t.Parallel()
	m := newMsFix(t)
	raw, err := json.Marshal(m.exportBundle(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"forecast"`) || strings.Contains(string(raw), `"children"`) {
		t.Fatal("a schedule with no forecast or selection must export none of the new members")
	}
}
