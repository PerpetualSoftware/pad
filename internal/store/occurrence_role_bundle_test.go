package store

import (
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4714: occurrence role snapshots travel in the workspace bundle.

// stampedBundle resolves one occurrence done under the legacy zero target and
// then gives the schedule a native half-hour target, so the snapshot and the
// current offsets differ.
func stampedBundle(t *testing.T) (*schedFixture, map[string]any) {
	t.Helper()
	f := newSchedFixture(t)
	sc := f.create(t, dailyUTC)
	f.mustResolve(t, f.item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user"}, "2026-10-30T10:00:00Z")
	if _, err := f.s.SetCommitmentRoles(f.ws.ID, f.item.ID, models.CommitmentRolesInput{TargetSecs: 1800}); err != nil {
		t.Fatal(err)
	}
	return f, bundleObject(t, f.exportBundle(t))
}

func bundleOccurrence(b map[string]any) map[string]any {
	return b["schedules"].([]any)[0].(map[string]any)["occurrences"].([]any)[0].(map[string]any)
}

func importedStamp(t *testing.T, f *schedFixture, b map[string]any) (occStamp, ImportReport) {
	t.Helper()
	out, rep := restoredBundle(t, f, b)
	if len(out.Schedules) != 1 {
		return occStamp{}, rep
	}
	return f.stampOf(t, out.Schedules[0].ID, "2026-10-30T09:00:00Z"), rep
}

func TestBundleCarriesOccurrenceRoles(t *testing.T) {
	t.Run("round trip keeps the snapshot", func(t *testing.T) {
		f, b := stampedBundle(t)
		if got := bundleOccurrence(b)["roles"]; !reflect.DeepEqual(got, map[string]any{"target_days": float64(0), "target_secs": float64(0)}) {
			t.Fatalf("exported occurrence roles = %#v", got)
		}
		got, rep := importedStamp(t, f, b)
		if len(rep.DroppedSchedules) != 0 || got.String() != (occStamp{TargetDays: ptr(0), TargetSecs: ptr(0)}).String() {
			t.Fatalf("imported stamp = %s, drops %+v", got, rep.DroppedSchedules)
		}
	})
	t.Run("a bundle without occurrence roles is stamped by the migration rule", func(t *testing.T) {
		f, b := stampedBundle(t)
		delete(bundleOccurrence(b), "roles")
		got, rep := importedStamp(t, f, b)
		if len(rep.DroppedSchedules) != 0 || got.String() != (occStamp{TargetDays: ptr(0), TargetSecs: ptr(1800)}).String() {
			t.Fatalf("imported stamp = %s, drops %+v", got, rep.DroppedSchedules)
		}
	})
	t.Run("a version 1 bundle is stamped from its legacy roles", func(t *testing.T) {
		f, _ := stampedBundle(t)
		v1 := f.exportBundle(t)
		stripV2(v1)
		got, rep := importedStamp(t, f, bundleObject(t, v1))
		if len(rep.DroppedSchedules) != 0 || got.String() != (occStamp{TargetDays: ptr(0), TargetSecs: ptr(0)}).String() {
			t.Fatalf("imported stamp = %s, drops %+v", got, rep.DroppedSchedules)
		}
	})
	t.Run("a version 1 bundle with the member is refused", func(t *testing.T) {
		f, _ := stampedBundle(t)
		v1 := f.exportBundle(t)
		stripV2(v1)
		m := bundleObject(t, v1)
		bundleOccurrence(m)["roles"] = map[string]any{"target_days": 0, "target_secs": 0}
		data := bundleDecode(t, m)
		if !data.HasV2ScheduleMembers() {
			t.Fatal("occurrence roles are not seen as a version 2 member")
		}
		if _, _, err := f.s.ImportWorkspaceWithReport(data, "Refused", "", "api"); err == nil || !strings.Contains(err.Error(), "export version 1 contains version 2 schedule members") {
			t.Fatalf("err=%v", err)
		}
	})
	for _, tc := range []struct {
		name, reason string
		edit         func(o map[string]any)
	}{
		{"pairing", "occurrence_roles:pairing", func(o map[string]any) {
			o["roles"] = map[string]any{"target_days": 0, "target_secs": 0, "earliest_days": 1}
		}},
		{"deadline without behavior", "occurrence_roles:pairing", func(o map[string]any) {
			o["roles"] = map[string]any{"target_days": 0, "target_secs": 0, "deadline_days": 1, "deadline_secs": 0}
		}},
		{"enum", "occurrence_roles:enum", func(o map[string]any) {
			o["roles"] = map[string]any{"target_days": 0, "target_secs": 0, "deadline_days": 1, "deadline_secs": 0, "deadline_behavior": "soft"}
		}},
		{"range", "occurrence_roles:range", func(o map[string]any) {
			o["roles"] = map[string]any{"target_days": 2147483648, "target_secs": 0}
		}},
		{"state", "occurrence_roles:state", func(o map[string]any) {
			o["state"], o["actual_source"] = "moved", "legacy_unknown"
			delete(o, "actual_done_at")
		}},
	} {
		t.Run("drops "+tc.name, func(t *testing.T) {
			f, b := stampedBundle(t)
			delete(b, "schedule_changes")
			tc.edit(bundleOccurrence(b))
			out, rep := restoredBundle(t, f, b)
			if len(out.Schedules) != 0 || len(rep.DroppedSchedules) != 1 || rep.DroppedSchedules[0].Reason != tc.reason {
				t.Fatalf("want %s, got %+v", tc.reason, rep)
			}
		})
	}
}
