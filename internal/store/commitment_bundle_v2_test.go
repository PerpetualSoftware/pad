package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func bundleObject(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func bundleDecode(t *testing.T, v any) *models.WorkspaceExport {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out models.WorkspaceExport
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func nativeRolesObject() map[string]any {
	return map[string]any{"target_days": 2, "target_secs": 1800, "earliest_days": -1, "earliest_secs": -600,
		"deadline_days": 3, "deadline_secs": 7200, "deadline_behavior": "lapses", "occupied_days": 0,
		"occupied_secs": 5400, "outcome": "Certificate renewed", "roles_origin": "native"}
}

func nativeBundle(t *testing.T) (*schedFixture, map[string]any, map[string]any) {
	t.Helper()
	f := newSchedFixture(t)
	sc := f.create(t, dailyUTC)
	b := bundleObject(t, f.exportBundle(t))
	b["version"] = 2
	x := b["schedules"].([]any)[0].(map[string]any)
	x["id"], x["roles"] = sc.ID, nativeRolesObject()
	return f, b, x
}

func testOverride(key string) map[string]any {
	return map[string]any{"occurrence_key": key, "role": "target", "value_at": "2026-10-30T12:00:00Z", "value_wall": "2026-10-30T12:00:00", "created_at": "2026-10-29T12:00:00Z", "updated_at": "2026-10-29T13:00:00Z"}
}

func testChange(id, item, sc string) map[string]any {
	return map[string]any{"id": id, "item_id": item, "schedule_id": sc, "change_type": "move", "role": "target", "actor": "user", "reason": "rescheduled", "reason_source": "user", "before_json": "{\"target\":1}", "after_json": "{\"target\":2}", "root_change_id": id, "recorded_at": "2026-10-29T14:00:00Z"}
}

func restoredBundle(t *testing.T, f *schedFixture, b map[string]any) (*models.WorkspaceExport, ImportReport) {
	t.Helper()
	ws, rep := f.importBundle(t, bundleDecode(t, b), "Restored commitment")
	out, err := f.s.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatal(err)
	}
	return jsonRoundTrip(t, out), rep
}

func stripV2(b *models.WorkspaceExport) {
	b.Version = 1
	// The wire keys define the v1 format independently of the current model.
	raw, _ := json.Marshal(b)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	delete(m, "schedule_changes")
	if xs, ok := m["schedules"].([]any); ok {
		for _, v := range xs {
			x := v.(map[string]any)
			delete(x, "id")
			delete(x, "roles")
			delete(x, "overrides")
			if os, ok := x["occurrences"].([]any); ok {
				for _, v := range os {
					o := v.(map[string]any)
					delete(o, "actual_done_at")
					delete(o, "actual_source")
				}
			}
		}
	}
	raw, _ = json.Marshal(m)
	*b = models.WorkspaceExport{}
	_ = json.Unmarshal(raw, b)
}

func TestBundleV2_RoundTripNativeCommitment(t *testing.T) {
	f := newSchedFixture(t)
	sc := f.create(t, dailyUTC)
	actual := rt("2026-10-30T08:00:00Z")
	f.mustResolve(t, f.item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user", ActualDoneAt: &actual}, "2026-10-30T10:00:00Z")
	cur := f.reload(t, f.item.ID)
	f.mustResolve(t, f.item, ResolveInput{OccurrenceAt: rt(*cur.NextAt), State: "done", Actor: "user"}, "2026-10-31T10:00:00Z")
	cur = f.reload(t, f.item.ID)
	key := OccurrenceKey(cur, *cur.NextAt, *cur.NextWall)
	until := rt("2026-11-01T12:00:00Z")
	f.mustResolve(t, f.item, ResolveInput{OccurrenceAt: rt(*cur.NextAt), State: "move", Actor: "user", Until: &until}, "2026-10-31T11:00:00Z")
	// Seed the moved target before adding a native deadline: resolve cannot move
	// a commitment that already carries that role.
	if _, err := f.s.SetCommitmentRoles(f.ws.ID, f.item.ID, models.CommitmentRolesInput{TargetDays: 2, TargetSecs: 1800, EarliestDays: i(-1), EarliestSecs: i(-600), DeadlineDays: i(3), DeadlineSecs: i(7200), DeadlineBehavior: ptr("lapses"), OccupiedDays: i(0), OccupiedSecs: i(5400), Outcome: "Certificate renewed"}); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE schedule_occurrence_overrides SET value_wall=?,created_at=?,updated_at=? WHERE schedule_id=? AND occurrence_key=?`, "2026-11-01T12:00:00", "2026-10-31T12:00:00.123456789Z", "2026-10-31T13:00:00.987654321Z", sc.ID, key)
	src := f.exportBundle(t)
	sm := bundleObject(t, src)
	sx := sm["schedules"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(sx["roles"], bundleObject(t, nativeRolesObject())) {
		t.Fatalf("native roles missing: %+v", sx["roles"])
	}
	if len(sx["overrides"].([]any)) != 1 {
		t.Fatal("override lost")
	}
	out, rep := restoredBundle(t, f, sm)
	if len(rep.DroppedSchedules) != 0 {
		t.Fatal(rep)
	}
	dm := bundleObject(t, out)
	dx := dm["schedules"].([]any)[0].(map[string]any)
	delete(sx, "id")
	delete(dx, "id")
	delete(sx, "item_id")
	delete(dx, "item_id")
	if !reflect.DeepEqual(sx, dx) {
		t.Fatalf("schedule differs:\nsrc %+v\ndst %+v", sx, dx)
	}
	before := sm["schedule_changes"].([]any)
	after := dm["schedule_changes"].([]any)
	if len(after) != len(before)+1 || after[len(after)-1].(map[string]any)["change_type"] != "import" {
		t.Fatalf("journal count/marker: %+v", after)
	}
	for i := range before {
		a := before[i].(map[string]any)
		b := after[i].(map[string]any)
		for _, k := range []string{"id", "item_id", "schedule_id", "root_change_id", "parent_change_id"} {
			delete(a, k)
			delete(b, k)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("journal %d differs: %+v %+v", i, a, b)
		}
	}
}

func TestBundleV2_JournalFollowsRemappedIds(t *testing.T) {
	f, b, x := nativeBundle(t)
	a := testChange("first", f.item.ID, x["id"].(string))
	c := testChange("second", f.item.ID, x["id"].(string))
	c["root_change_id"], c["parent_change_id"] = "first", "first"
	b["schedule_changes"] = []any{a, c}
	out, rep := restoredBundle(t, f, b)
	if len(rep.DroppedSchedules) != 0 {
		t.Fatal(rep)
	}
	m := bundleObject(t, out)
	rows := m["schedule_changes"].([]any)
	ids := map[any]bool{}
	for _, v := range rows {
		r := v.(map[string]any)
		ids[r["id"]] = true
		if r["item_id"] == f.item.ID || r["schedule_id"] == x["id"] {
			t.Fatal("source binding retained")
		}
	}
	for _, v := range rows {
		r := v.(map[string]any)
		if !ids[r["root_change_id"]] {
			t.Fatal("root outside destination")
		}
		if p := r["parent_change_id"]; p != nil && !ids[p] {
			t.Fatal("parent outside destination")
		}
	}
}

func TestBundleV2_OccurrenceKeysCanonical(t *testing.T) {
	f, b, x := nativeBundle(t)
	x["overrides"] = []any{testOverride("2026-10-30T11:00:00+02:00|2026-10-30T09:00"), testOverride("garbled")}
	c := testChange("key-change", f.item.ID, x["id"].(string))
	c["occurrence_key"] = "2026-10-30T11:00:00+02:00|2026-10-30T09:00"
	b["schedule_changes"] = []any{c}
	out, rep := restoredBundle(t, f, b)
	if len(rep.DroppedSchedules) != 1 || rep.DroppedSchedules[0].Reason != "override:key" {
		t.Fatalf("drops: %+v", rep)
	}
	m := bundleObject(t, out)
	dx := m["schedules"].([]any)[0].(map[string]any)
	os := dx["overrides"].([]any)
	want := "2026-10-30T09:00:00Z|2026-10-30T09:00:00"
	if len(os) != 1 || os[0].(map[string]any)["occurrence_key"] != want || m["schedule_changes"].([]any)[0].(map[string]any)["occurrence_key"] != want {
		t.Fatal("noncanonical key stored")
	}
	item := f.itemByTitle(t, outWorkspaceID(t, f, out), f.item.Title)
	sc, err := f.s.GetSchedule(item.WorkspaceID, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.resolveOccurrenceAt(item.WorkspaceID, item.ID, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user"}, rt("2026-10-30T12:00:00Z"))
	if err != nil {
		t.Fatalf("restored override cannot resolve: %v", err)
	}
}

func outWorkspaceID(t *testing.T, f *schedFixture, out *models.WorkspaceExport) string {
	t.Helper()
	var id string
	if err := f.s.db.QueryRow(f.s.q(`SELECT id FROM workspaces WHERE slug=?`), out.Workspace.Slug).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestBundleV2_FloatingAndAllDayKeys(t *testing.T) {
	for _, floating := range []bool{false, true} {
		for _, allDay := range []bool{false, true} {
			t.Run(fmt.Sprintf("floating=%v/allDay=%v", floating, allDay), func(t *testing.T) {
				f, b, x := nativeBundle(t)
				x["floating"] = floating
				if floating {
					delete(x, "tz")
				}
				wall := "2026-10-30T09:00"
				if allDay {
					wall = "2026-10-30"
					x["dtstart"], x["next_wall"] = wall, wall
					roles := x["roles"].(map[string]any)
					roles["occupied_days"], roles["occupied_secs"] = 1, 0
				}
				key := "2026-10-30T09:00:00Z|" + wall
				x["overrides"] = []any{testOverride(key)}
				out, rep := restoredBundle(t, f, b)
				if len(rep.DroppedSchedules) != 0 {
					t.Fatal(rep)
				}
				m := bundleObject(t, out)
				dx := m["schedules"].([]any)[0].(map[string]any)
				sc := &models.ItemSchedule{Floating: floating}
				cw := wall
				if !allDay {
					cw += ":00"
				}
				want := OccurrenceKey(sc, "2026-10-30T09:00:00Z", cw)
				if dx["overrides"].([]any)[0].(map[string]any)["occurrence_key"] != want {
					t.Fatal("wrong identity")
				}
			})
		}
	}
}

func TestBundleV2_DeletedScheduleJournalKept(t *testing.T) {
	f, _, x := nativeBundle(t)
	if ok, err := f.s.DeleteSchedule(f.ws.ID, f.item.ID); err != nil || !ok {
		t.Fatalf("delete schedule: %v", err)
	}
	b := bundleObject(t, f.exportBundle(t))
	if rows, ok := b["schedule_changes"].([]any); !ok || len(rows) != 2 {
		t.Fatal("export lost deleted schedule journal")
	}
	out, rep := restoredBundle(t, f, b)
	if len(rep.DroppedSchedules) != 0 {
		t.Fatal(rep)
	}
	rows, ok := bundleObject(t, out)["schedule_changes"].([]any)
	if !ok || len(rows) != 2 || rows[0].(map[string]any)["schedule_id"] == x["id"] || rows[0].(map[string]any)["schedule_id"] != rows[1].(map[string]any)["schedule_id"] {
		t.Fatal("deleted schedule identity lost")
	}
}

func TestBundleV2_DroppedScheduleTakesItsJournal(t *testing.T) {
	f, b, x := nativeBundle(t)
	x["roles"].(map[string]any)["roles_origin"] = "bad"
	b["schedule_changes"] = []any{testChange("orphan", f.item.ID, x["id"].(string))}
	out, rep := restoredBundle(t, f, b)
	if len(out.Schedules) != 0 || len(rep.DroppedSchedules) != 2 || rep.DroppedSchedules[1].Reason != "changes:orphan" {
		t.Fatalf("orphan journal retained: %+v %+v", out, rep)
	}
}

func TestBundleV2_NoMisbinding(t *testing.T) {
	f, b, x := nativeBundle(t)
	other := f.another(t, "Other")
	exported := f.exportBundle(t)
	b["items"] = bundleObject(t, exported)["items"]
	b["schedule_changes"] = []any{testChange("mismatch", other.ID, x["id"].(string)), testChange("external", "outside-bundle", "unknown")}
	out, rep := restoredBundle(t, f, b)
	if len(rep.DroppedSchedules) != 2 {
		t.Fatalf("misbinding not reported: %+v", rep)
	}
	for _, r := range rep.DroppedSchedules {
		if r.Reason != "changes:orphan" {
			t.Fatal(r)
		}
	}
	if n := len(bundleObject(t, out)["schedule_changes"].([]any)); n != 1 {
		t.Fatalf("misbound journal: %d", n)
	}
}

func TestBundleV2_ExternalDependencyUnresolved(t *testing.T) {
	f, b, x := nativeBundle(t)
	x["occurrences"] = []any{map[string]any{"occurrence_at": "2026-10-29T09:00:00Z", "occurrence_wall": "2026-10-29T09:00:00", "state": "done", "actor": "user", "resolved_at": "2026-10-29T10:00:00Z", "actual_source": "legacy_unknown", "comment_id": "outside-bundle"}}
	out, rep := restoredBundle(t, f, b)
	if len(rep.DroppedSchedules) != 0 {
		t.Fatal(rep)
	}
	if out.Schedules[0].Occurrences[0].CommentID != "" {
		t.Fatal("external comment rebound")
	}
}

func TestBundleV2_RefusesMalformedRoles(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		edit         func(map[string]any)
	}{
		{"pairing", "roles:pairing", func(x map[string]any) { delete(x["roles"].(map[string]any), "earliest_secs") }},
		{"origin", "roles:enum", func(x map[string]any) { x["roles"].(map[string]any)["roles_origin"] = "foreign" }},
		{"range", "roles:range", func(x map[string]any) { x["roles"].(map[string]any)["target_days"] = 2147483648 }},
		{"behavior", "roles:enum", func(x map[string]any) { x["roles"].(map[string]any)["deadline_behavior"] = "soft" }},
		{"duplicate", "override:duplicate", func(x map[string]any) {
			x["overrides"] = []any{testOverride("2026-10-30T09:00:00Z|2026-10-30T09:00"), testOverride("2026-10-30T09:00:00Z|2026-10-30T09:00:00")}
		}},
		{"override role", "override:role", func(x map[string]any) {
			o := testOverride("2026-10-30T09:00:00Z|2026-10-30T09:00:00")
			o["role"] = "bad"
			x["overrides"] = []any{o}
		}},
		{"non done", "actual:combination", func(x map[string]any) {
			x["occurrences"] = []any{map[string]any{"occurrence_at": "2026-10-29T09:00:00Z", "occurrence_wall": "2026-10-29T09:00:00", "state": "skipped", "resolved_at": "2026-10-29T10:00:00Z", "actual_source": "reported", "actual_done_at": "2026-10-29T09:00:00Z"}}
		}},
		{"no time", "actual:combination", func(x map[string]any) {
			x["occurrences"] = []any{map[string]any{"occurrence_at": "2026-10-29T09:00:00Z", "occurrence_wall": "2026-10-29T09:00:00", "state": "done", "resolved_at": "2026-10-29T10:00:00Z", "actual_source": "reported"}}
		}},
		{"bad time", "actual:instant", func(x map[string]any) {
			x["occurrences"] = []any{map[string]any{"occurrence_at": "2026-10-29T09:00:00Z", "occurrence_wall": "2026-10-29T09:00:00", "state": "done", "resolved_at": "2026-10-29T10:00:00Z", "actual_source": "reported", "actual_done_at": "nope"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, b, x := nativeBundle(t)
			delete(b, "schedule_changes")
			tc.edit(x)
			out, rep := restoredBundle(t, f, b)
			if len(out.Schedules) != 0 || len(rep.DroppedSchedules) != 1 || rep.DroppedSchedules[0].Reason != tc.reason {
				t.Fatalf("want %s, got %+v", tc.reason, rep)
			}
		})
	}
}

func TestBundleV1_StillImportsThroughFourKind(t *testing.T) {
	for _, kind := range []string{"check", "deadline_soft", "deadline_hard", "window"} {
		t.Run(kind, func(t *testing.T) {
			f := newSchedFixture(t)
			in := dailyUTC
			in.Kind = kind
			if kind == "window" {
				in.Dtend = "2026-10-30T11:00"
			}
			sc := f.create(t, in)
			f.mustResolve(t, f.item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user"}, "2026-10-30T10:00:00Z")
			b := f.exportBundle(t)
			stripV2(b)
			out, _ := restoredBundle(t, f, bundleObject(t, b))
			dst := out.Schedules[0]
			m := bundleObject(t, dst)
			r, ok := m["roles"].(map[string]any)
			if !ok {
				t.Fatal("export omits mapped legacy roles")
			}
			if r["roles_origin"] != "legacy_backfill" {
				t.Fatal("legacy origin")
			}
			if kind == "deadline_hard" && r["deadline_behavior"] != "stays_owed" {
				t.Fatal("legacy deadline")
			}
			if kind == "window" && r["occupied_secs"] != float64(7200) {
				t.Fatal("legacy window")
			}
			if r["target_days"] != float64(0) || r["target_secs"] != float64(0) {
				t.Fatal("legacy target")
			}
			if m["occurrences"].([]any)[0].(map[string]any)["actual_source"] != "legacy_unknown" {
				t.Fatal("fabricated actual")
			}
			rows := bundleObject(t, out)["schedule_changes"].([]any)
			if len(rows) != 1 || rows[0].(map[string]any)["change_type"] != "import" {
				t.Fatal("legacy journal")
			}
		})
	}
}

func TestBundleV1_WithV2MembersRefused(t *testing.T) {
	for _, member := range []string{"roles", "id", "overrides", "schedule_changes", "actual_source", "actual_done_at", "roles_null", "id_empty", "overrides_empty", "changes_empty", "actual_empty"} {
		t.Run(member, func(t *testing.T) {
			f, b, _ := nativeBundle(t)
			v1 := bundleDecode(t, b)
			stripV2(v1)
			b = bundleObject(t, v1)
			x := b["schedules"].([]any)[0].(map[string]any)
			switch member {
			case "roles_null":
				x["roles"] = nil
			case "id_empty":
				x["id"] = ""
			case "overrides_empty":
				x["overrides"] = []any{}
			case "changes_empty":
				b["schedule_changes"] = []any{}
			case "actual_empty":
				x["occurrences"] = []any{map[string]any{"actual_source": ""}}
			case "roles":
				x[member] = nativeRolesObject()
			case "id":
				x[member] = "source"
			case "overrides":
				x[member] = []any{testOverride("bad")}
			case "schedule_changes":
				b[member] = []any{testChange("id", f.item.ID, "s")}
			default:
				x["occurrences"] = []any{map[string]any{member: "reported"}}
			}
			n := f.count(t, `SELECT COUNT(*) FROM workspaces`)
			_, _, err := f.s.ImportWorkspaceWithReport(bundleDecode(t, b), "Refused", "", "api")
			if err == nil {
				t.Fatal("v1 silently dropped v2 members")
			}
			if f.count(t, `SELECT COUNT(*) FROM workspaces`) != n {
				t.Fatal("husk workspace")
			}
		})
	}
}

func TestImportRefusesUnsupportedVersions(t *testing.T) {
	for _, v := range []int{0, models.WorkspaceExportVersion + 1, -1, 99} {
		t.Run(fmt.Sprint(v), func(t *testing.T) {
			f := newSchedFixture(t)
			b := f.exportBundle(t)
			b.Version = v
			n := f.count(t, `SELECT COUNT(*) FROM workspaces`)
			_, _, err := f.s.ImportWorkspaceWithReport(b, "Refused", "", "api")
			if err == nil || err.Error() != fmt.Sprintf("unsupported export version: %d", v) {
				t.Fatal(err)
			}
			if f.count(t, `SELECT COUNT(*) FROM workspaces`) != n {
				t.Fatal("husk workspace")
			}
		})
	}
}

func TestExportBundleV2AdditiveDecode(t *testing.T) {
	_, b, _ := nativeBundle(t)
	decoded := bundleDecode(t, b)
	round := bundleObject(t, decoded)
	if round["schedules"].([]any)[0].(map[string]any)["roles"] == nil {
		t.Fatal("v2 roles lost on decode")
	}
	stripV2(decoded)
	round = bundleObject(t, decoded)
	if round["schedules"].([]any)[0].(map[string]any)["roles"] != nil {
		t.Fatal("v1 fabricated roles")
	}
}

func TestCopyCarriesNativeRolesFreshHistory(t *testing.T) {
	c := newSchedCopy(t)
	sc := c.scheduleFuture(t, daily())
	c.history(t, sc, 2)
	c.exec(t, `UPDATE item_schedules SET target_days=2,target_secs=1800,outcome='renewed',roles_origin='native' WHERE id=?`, sc.ID)
	o := testOverride(OccurrenceKey(sc, *sc.NextAt, *sc.NextWall))
	c.exec(t, `INSERT INTO schedule_occurrence_overrides (id,schedule_id,occurrence_key,role,value_at,created_at,updated_at) VALUES (?,?,?,'target',?,?,?)`, newID(), sc.ID, o["occurrence_key"], o["value_at"], o["created_at"], o["updated_at"])
	result := c.do(t, false)
	dst := c.sched(t, c.wsB.ID, result.Item.ID)
	if c.count(t, `SELECT COUNT(*) FROM item_schedules WHERE id=? AND target_days=2 AND target_secs=1800 AND outcome='renewed' AND roles_origin='native'`, dst.ID) != 1 {
		t.Fatal("copy downgraded native roles")
	}
	if c.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=?`, dst.ID) != 0 || c.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides WHERE schedule_id=?`, dst.ID) != 0 {
		t.Fatal("copy history not fresh")
	}
	rows := commitmentJournal(t, c.s, dst.ID)
	if len(rows) != 1 || rows[0].Type != "carry" {
		t.Fatal("copy journal not fresh")
	}
}

func TestMoveCarriesNativeRolesAndOutstandingOverride(t *testing.T) {
	c := newSchedCopy(t)
	sc := c.scheduleFuture(t, daily())
	c.history(t, sc, 2)
	c.exec(t, `UPDATE item_schedules SET earliest_days=-1,earliest_secs=0,deadline_days=2,deadline_secs=0,deadline_behavior='lapses',occupied_days=0,occupied_secs=3600,target_days=1,outcome='renewed',roles_origin='native' WHERE id=?`, sc.ID)
	key := OccurrenceKey(sc, *sc.NextAt, *sc.NextWall)
	for _, k := range []string{key, "2026-10-01T09:00:00Z|2026-10-01T09:00:00"} {
		c.exec(t, `INSERT INTO schedule_occurrence_overrides (id,schedule_id,occurrence_key,role,value_at,created_at,updated_at) VALUES (?,?,?,'target',?,?,?)`, newID(), sc.ID, k, "2026-10-30T12:00:00Z", copyNowTS, copyNowTS)
	}
	result := c.do(t, true)
	dst := c.sched(t, c.wsB.ID, result.Item.ID)
	if c.count(t, `SELECT COUNT(*) FROM item_schedules WHERE id=? AND target_days=1 AND earliest_days=-1 AND deadline_behavior='lapses' AND occupied_secs=3600 AND outcome='renewed' AND roles_origin='native'`, dst.ID) != 1 {
		t.Fatal("move downgraded native roles")
	}
	if c.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides WHERE schedule_id=? AND occurrence_key=?`, dst.ID, key) != 1 || c.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides WHERE schedule_id=?`, dst.ID) != 1 {
		t.Fatal("outstanding override not preserved exclusively")
	}
	if c.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=?`, dst.ID) != 0 {
		t.Fatal("occurrence history carried")
	}
	rows := commitmentJournal(t, c.s, sc.ID)
	if rows[len(rows)-1].Type != "end" {
		t.Fatal("source not ended")
	}
}

func TestMoveCarriesJournal(t *testing.T) {
	c := newSchedCopy(t)
	sc := c.scheduleFuture(t, daily())
	result := c.do(t, true)
	dst := c.sched(t, c.wsB.ID, result.Item.ID)
	rows := commitmentJournal(t, c.s, dst.ID)
	if len(rows) != 2 || rows[0].Type != "create" || rows[1].Type != "carry" {
		t.Fatal("source journal lost")
	}
	if c.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND workspace_id=? AND item_id=? AND root_change_id IN (SELECT id FROM schedule_changes WHERE schedule_id=?)`, dst.ID, c.wsB.ID, result.Item.ID, dst.ID) != 2 {
		t.Fatal("journal remap")
	}
	if len(commitmentJournal(t, c.s, sc.ID)) != 2 {
		t.Fatal("source end missing")
	}
}

func TestMoveRefusedWhenRolesCannotCarry(t *testing.T) {
	c := newSchedCopy(t)
	if c.s.dialect.Driver() != DriverSQLite {
		t.Skip("SQLite INTEGER permits out-of-range persisted values")
	}
	sc := c.scheduleFuture(t, daily())
	c.exec(t, `UPDATE item_schedules SET target_days=2147483648,roles_origin='native' WHERE id=?`, sc.ID)
	req := c.req()
	req.SourceItemID = c.src.ID
	req.ArchiveSource = true
	req.scheduleNow = copyNow
	before := c.count(t, `SELECT COUNT(*) FROM items`)
	_, err := c.s.CopyItemAcrossWorkspaces(req)
	var refused *ScheduleNotCarriedError
	if !errors.As(err, &refused) {
		t.Fatalf("move not refused: %v", err)
	}
	if c.count(t, `SELECT COUNT(*) FROM items`) != before || c.sched(t, c.wsA.ID, c.src.ID).EndedAt != nil {
		t.Fatal("refused move mutated workspaces")
	}
	if live, err := c.s.GetItem(c.src.ID); err != nil || live == nil {
		t.Fatalf("refused move archived source: %v", err)
	}
	if c.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=?`, sc.ID) != 1 {
		t.Fatal("refused move appended journal history")
	}

}

func TestCopyPreflightMatchesCopyForNativeRoles(t *testing.T) {
	for _, bad := range []bool{false, true} {
		t.Run(fmt.Sprint(bad), func(t *testing.T) {
			c := newSchedCopy(t)
			if bad && c.s.dialect.Driver() != DriverSQLite {
				t.Skip("SQLite INTEGER permits out-of-range persisted values")
			}
			sc := c.scheduleFuture(t, daily())
			days := 1
			if bad {
				days = 2147483648
			}
			c.exec(t, `UPDATE item_schedules SET target_days=?,roles_origin='native' WHERE id=?`, days, sc.ID)
			req := c.req()
			preview, err := c.s.PreviewScheduleCarry(c.s.db, c.wsA.ID, c.src.ID, "", req.CreatedBy, false, copyNow)
			if err != nil {
				t.Fatal(err)
			}
			result := c.do(t, false)
			if preview.Carried == bad {
				t.Fatalf("preflight wrong: %+v", preview)
			}
			if result.Schedule != preview {
				t.Fatalf("preflight differs: %+v %+v", preview, result.Schedule)
			}
			if bad && !strings.HasPrefix(preview.Skipped, "roles:") {
				t.Fatal(preview)
			}
		})
	}
}

func TestBundleV2_SourceIDsGuard(t *testing.T) {
	for _, which := range []string{"schedule_duplicate", "schedule_missing", "change_duplicate"} {
		t.Run(which, func(t *testing.T) {
			f, b, x := nativeBundle(t)
			b["schedule_changes"] = []any{testChange("change", f.item.ID, x["id"].(string))}
			switch which {
			case "schedule_duplicate":
				b["schedules"] = []any{x, x}
			case "schedule_missing":
				delete(x, "id")
			case "change_duplicate":
				b["schedule_changes"] = []any{testChange("change", f.item.ID, x["id"].(string)), testChange("change", f.item.ID, x["id"].(string))}
			}
			n := f.count(t, `SELECT COUNT(*) FROM workspaces`)
			_, _, err := f.s.ImportWorkspaceWithReport(bundleDecode(t, b), "Refused IDs", "", "api")
			if err == nil {
				t.Fatal("ambiguous ids accepted")
			}
			if f.count(t, `SELECT COUNT(*) FROM workspaces`) != n {
				t.Fatal("husk workspace")
			}
		})
	}
}

func TestBundleV2_JournalInvalidRowsAndMissingParents(t *testing.T) {
	f, b, x := nativeBundle(t)
	keep := testChange("keep", f.item.ID, x["id"].(string))
	keep["root_change_id"], keep["parent_change_id"] = "missing", "missing"
	badKey := testChange("key", f.item.ID, x["id"].(string))
	badKey["occurrence_key"] = "unparseable"
	badEnum := testChange("enum", f.item.ID, x["id"].(string))
	badEnum["reason_source"] = "foreign"
	b["schedule_changes"] = []any{keep, badKey, badEnum}
	out, rep := restoredBundle(t, f, b)
	if len(rep.DroppedSchedules) != 2 {
		t.Fatalf("invalid rows not counted: %+v", rep)
	}
	for _, d := range rep.DroppedSchedules {
		if d.Reason != "changes:orphan" {
			t.Fatal(d)
		}
	}
	rows := bundleObject(t, out)["schedule_changes"].([]any)
	r := rows[0].(map[string]any)
	if len(rows) != 2 || r["root_change_id"] != r["id"] || r["parent_change_id"] != nil {
		t.Fatalf("missing reference fallback: %+v", rows)
	}
}

func TestBundleV2_UnknownScheduleJournalNeedsOneItem(t *testing.T) {
	f, b, x := nativeBundle(t)
	other := f.another(t, "Other")
	exported := f.exportBundle(t)
	b["items"] = bundleObject(t, exported)["items"]
	b["schedule_changes"] = []any{testChange("a", x["item_id"].(string), "deleted-in-source"), testChange("b", other.ID, "deleted-in-source")}
	out, rep := restoredBundle(t, f, b)
	if len(rep.DroppedSchedules) != 1 || rep.DroppedSchedules[0].Reason != "changes:orphan" {
		t.Fatalf("second item under one deleted schedule not dropped: %+v", rep.DroppedSchedules)
	}
	kept := 0
	for _, c := range bundleObject(t, out)["schedule_changes"].([]any) {
		if c.(map[string]any)["change_type"] == "move" {
			kept++
		}
	}
	if kept != 1 {
		t.Fatalf("kept %d moved rows, want 1", kept)
	}
}

func TestBundleV2_JournalRowNeedsTimeAndCleanText(t *testing.T) {
	f, b, x := nativeBundle(t)
	nul := testChange("nul", x["item_id"].(string), x["id"].(string))
	nul["actor"] = "a\x00b"
	late := testChange("late", x["item_id"].(string), x["id"].(string))
	late["recorded_at"] = ""
	escaped := testChange("escaped", x["item_id"].(string), x["id"].(string))
	escaped["after_json"] = `{"x":"\u0000"}`
	badSchedule := testChange("badsched", x["item_id"].(string), "deleted\x00id")
	b["schedule_changes"] = []any{nul, late, escaped, badSchedule}
	_, rep := restoredBundle(t, f, b)
	if len(rep.DroppedSchedules) != 4 {
		t.Fatalf("unclean journal rows not dropped: %+v", rep.DroppedSchedules)
	}
}

func TestCanonicalCarriedKeyFollowsTheDestinationZone(t *testing.T) {
	const pinned = "2026-10-30T09:00:00Z|2026-10-30T09:00:00"
	got, err := canonicalCarriedKey(pinned, true)
	if err != nil || got != "2026-10-30T09:00:00" {
		t.Fatalf("floating destination, pinned-format key: %q %v", got, err)
	}
	if _, err := canonicalCarriedKey("2026-10-30T09:00:00", false); err == nil {
		t.Fatal("pinned timed destination accepted a wall-only key")
	}
	if got, err := canonicalCarriedKey("2026-10-30", false); err != nil || got != "2026-10-30" {
		t.Fatalf("all-day key: %q %v", got, err)
	}
	a, b := testOverride(pinned), testOverride("2026-10-30T10:00:00Z|2026-10-30T09:00:00")
	b["updated_at"] = "2026-10-29T15:00:00Z"
	var in []models.ScheduleOverrideExport
	for _, m := range []map[string]any{a, b} {
		raw, _ := json.Marshal(m)
		var o models.ScheduleOverrideExport
		if err := json.Unmarshal(raw, &o); err != nil {
			t.Fatal(err)
		}
		in = append(in, o)
	}
	out, drops, err := prepareCarriedOverrides(in, true)
	if err != nil || len(out) != 1 || out[0].UpdatedAt != "2026-10-29T15:00:00Z" || len(drops) != 1 {
		t.Fatalf("floating collapse: %+v %v %v", out, drops, err)
	}
}
