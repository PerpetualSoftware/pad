package models

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func legacyUnmarshalWorkspaceExport(b []byte, x *WorkspaceExport) error {
	type plain WorkspaceExport
	var decoded plain
	if err := json.Unmarshal(b, &decoded); err != nil {
		return err
	}
	*x = WorkspaceExport(decoded)
	var raw struct {
		Changes json.RawMessage `json:"schedule_changes"`
		Links   json.RawMessage `json:"schedule_links"`
		Gates   json.RawMessage `json:"schedule_gates"`
		Items   []struct {
			DeletedAt json.RawMessage `json:"deleted_at"`
		} `json:"items"`
		Schedules []map[string]json.RawMessage `json:"schedules"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	x.hasV2Members = raw.Changes != nil
	x.hasV3Members = raw.Links != nil
	x.hasV4Members = raw.Gates != nil
	for _, it := range raw.Items {
		if it.DeletedAt != nil {
			x.hasV4Members = true
		}
	}
	for _, sc := range raw.Schedules {
		for _, key := range []string{"id", "roles", "overrides", "forecast", "forecast_events", "children", "completion_evidence"} {
			if _, ok := sc[key]; ok {
				x.hasV2Members = true
			}
		}
		var occs []map[string]json.RawMessage
		if err := json.Unmarshal(sc["occurrences"], &occs); len(sc["occurrences"]) != 0 && err != nil {
			return err
		}
		for _, o := range occs {
			for _, key := range []string{"actual_done_at", "actual_source", "roles"} {
				if _, ok := o[key]; ok {
					x.hasV2Members = true
				}
			}
		}
	}
	return nil
}

func fullExportFixture() *WorkspaceExport {
	added := 5
	removed := 2
	blockerSched := "sched-1"
	openedAt := "2026-09-01T12:00:00Z"
	occKey := "2026-09-01T12:00:00Z"
	role := "target"
	beforeJSON := `{"dtstart":"2026-09-01"}`
	afterJSON := `{"dtstart":"2026-09-02"}`
	earliestDays := -1
	deadlineDays := 2
	behavior := "warn"

	return &WorkspaceExport{
		Version:    WorkspaceExportVersion,
		ExportedAt: "2026-09-10T10:00:00Z",
		Workspace: WorkspaceExportMeta{
			Name:        "Full Workspace",
			Slug:        "full-workspace",
			Description: "Everything populated",
			Settings:    `{"theme":"light"}`,
		},
		Collections: []CollectionExport{{
			ID:          "col-1",
			Name:        "Tasks",
			Slug:        "tasks",
			Icon:        "check",
			Description: "Task list",
			Schema:      `{"fields":[]}`,
			Settings:    `{}`,
			Traits:      `{"work":true}`,
			Prefix:      "TASK",
			Source:      "cli",
			SortOrder:   1,
			IsDefault:   true,
			IsSystem:    false,
			CreatedAt:   "2026-09-01T00:00:00Z",
			UpdatedAt:   "2026-09-01T00:00:00Z",
		}},
		Items: []ItemExport{{
			ID:           "item-1",
			CollectionID: "col-1",
			Title:        "First Task",
			Slug:         "first-task",
			Content:      "Hello <world> & friends",
			ContentState: "applied_pending_flush",
			CollabSetAside: []YjsSetAsideExport{{
				OpLogID:       7,
				SchemaVersion: "1",
				CreatedAt:     "2026-09-01T01:00:00Z",
				SetAsideAt:    "2026-09-01T02:00:00Z",
				UpdateData:    []byte{1, 2, 3, 4},
			}},
			BuiltinOrigin: &BuiltinOrigin{
				Key:         "playbook/ship",
				SeedHash:    "abcd",
				SeedContent: "seed",
				SeedFields:  `{}`,
			},
			Fields:         `{"status":"open"}`,
			Tags:           `["v1"]`,
			Pinned:         true,
			SortOrder:      1,
			CreatedBy:      "alice",
			LastModifiedBy: "alice",
			Source:         "web",
			ItemNumber:     1,
			CreatedAt:      "2026-09-01T00:00:00Z",
			UpdatedAt:      "2026-09-02T00:00:00Z",
		}},
		Comments: []CommentExport{{
			ID:        "com-1",
			ItemID:    "item-1",
			Author:    "alice",
			Body:      "Looks good",
			CreatedBy: "alice",
			Source:    "web",
			CreatedAt: "2026-09-01T03:00:00Z",
			UpdatedAt: "2026-09-01T03:00:00Z",
		}},
		ItemLinks: []ItemLinkExport{{
			ID:        "lnk-1",
			SourceID:  "item-1",
			TargetID:  "item-1",
			LinkType:  "related",
			CreatedBy: "alice",
			CreatedAt: "2026-09-01T04:00:00Z",
		}},
		ItemVersions: []ItemVersionExport{
			{
				ID:            "ver-1",
				ItemID:        "item-1",
				Content:       "v1 body",
				ChangeSummary: "created",
				CreatedBy:     "alice",
				Source:        "web",
				IsDiff:        false,
				CreatedAt:     "2026-09-01T00:00:00Z",
				LinesAdded:    &added,
				IsCreate:      true,
				BodySHA256:    strings.Repeat("a", 64),
			},
			{
				ID:            "ver-2",
				ItemID:        "item-1",
				Content:       "@@ -1 +1 @@",
				ChangeSummary: "edited",
				CreatedBy:     "bob",
				Source:        "cli",
				IsDiff:        true,
				CreatedAt:     "2026-09-02T00:00:00Z",
				LinesAdded:    &added,
				LinesRemoved:  &removed,
				BodySHA256:    strings.Repeat("b", 64),
			},
		},
		Reminders: []ReminderExport{{
			ItemID:    "item-1",
			RemindAt:  "2026-09-12T09:00:00Z",
			CreatedAt: "2026-09-01T05:00:00Z",
			UpdatedAt: "2026-09-01T05:00:00Z",
		}},
		Verifications: []VerificationExport{{
			ItemID:        "item-1",
			CriterionID:   "AC-1",
			CriterionHash: "h1",
			SpecSeq:       1,
			CodeRevision:  strings.Repeat("c", 40),
			Verdict:       "pass",
			Evidence:      "ok",
			RunID:         "run-1",
			CreatedBy:     "alice",
			AgentName:     "ci",
			CreatedAt:     "2026-09-01T06:00:00Z",
		}},
		Schedules: []ScheduleExport{{
			ID: "sched-1",
			Roles: &ScheduleRolesExport{
				TargetDays:       0,
				TargetSecs:       32400,
				EarliestDays:     &earliestDays,
				DeadlineDays:     &deadlineDays,
				DeadlineBehavior: &behavior,
				RolesOrigin:      "explicit",
			},
			Overrides: []ScheduleOverrideExport{{
				OccurrenceKey: occKey,
				Role:          "target",
				ValueAt:       "2026-09-02T09:00:00Z",
				CreatedAt:     "2026-09-01T00:00:00Z",
				UpdatedAt:     "2026-09-01T00:00:00Z",
			}},
			ItemID:           "item-1",
			Kind:             "deadline",
			Dtstart:          "2026-09-01T09:00:00Z",
			Anchor:           "calendar",
			Audience:         "human",
			EndedDismissedAt: "",
			CreatedAt:        "2026-09-01T00:00:00Z",
			UpdatedAt:        "2026-09-01T00:00:00Z",
			Occurrences: []ScheduleOccurrenceExport{{
				ActualDoneAt:   "2026-09-01T10:00:00Z",
				ActualSource:   "manual",
				OccurrenceAt:   "2026-09-01T09:00:00Z",
				OccurrenceWall: "2026-09-01T09:00:00",
				State:          "done",
				ResolvedAt:     "2026-09-01T10:00:00Z",
				Roles: &ScheduleOccurrenceRolesExport{
					TargetDays: 0,
					TargetSecs: 32400,
				},
			}},
			Forecast: &ForecastExport{
				Value:     "on_track",
				SetBy:     "alice",
				SetByKind: "user",
				SetAt:     "2026-09-01T07:00:00Z",
			},
		}},
		ScheduleChanges: []ScheduleChangeExport{{
			ID:            "chg-1",
			ItemID:        "item-1",
			ScheduleID:    "sched-1",
			OccurrenceKey: &occKey,
			ChangeType:    "move",
			Role:          &role,
			BeforeJSON:    &beforeJSON,
			AfterJSON:     &afterJSON,
			Actor:         "alice",
			Reason:        "shifted",
			ReasonSource:  "user",
			RootChangeID:  "chg-1",
			RecordedAt:    "2026-09-01T08:00:00Z",
		}},
		ScheduleLinks: []ScheduleLinkExport{{
			ParentScheduleID: "sched-1",
			ParentRole:       "target",
			ChildScheduleID:  "sched-1",
			ChildRole:        "earliest",
			Offset:           ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: 3600},
			CancelPolicy:     LinkWaitingOn,
			CreatedAt:        "2026-09-01T09:00:00Z",
			UpdatedAt:        "2026-09-01T09:00:00Z",
		}},
		ScheduleGates: []ScheduleGateExport{{
			ID:                   "gate-1",
			BlockedItemID:        "item-1",
			BlockerItemID:        "item-1",
			BlockerScheduleID:    &blockerSched,
			BlockerOccurrenceKey: occKey,
			OpenedAt:             &openedAt,
			CreatedAt:            "2026-09-01T09:30:00Z",
		}},
	}
}

func TestEncodeWorkspaceExport_BaseIsNonEmptyJSONObject(t *testing.T) {
	t.Parallel()
	baseVal := *fullExportFixture()
	baseVal.ItemVersions = nil
	base, err := json.Marshal(&baseVal)
	if err != nil {
		t.Fatalf("marshal base: %v", err)
	}
	if len(base) <= 2 || base[0] != '{' || base[len(base)-1] != '}' {
		t.Fatalf("expected non-empty JSON object ending in }, got %q", string(base))
	}
	if bytes.Contains(base, []byte(`"item_versions"`)) {
		t.Fatalf("expected base with nil ItemVersions to omit item_versions key: %s", base)
	}
}

func TestEncodeWorkspaceExport_DecodesEqual(t *testing.T) {
	t.Parallel()

	t.Run("populated with iterator", func(t *testing.T) {
		fx := fullExportFixture()
		wantRaw, err := json.Marshal(fx)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		var want WorkspaceExport
		if err := json.Unmarshal(wantRaw, &want); err != nil {
			t.Fatalf("unmarshal want: %v", err)
		}

		base := *fx
		base.ItemVersions = nil
		var buf bytes.Buffer
		err = EncodeWorkspaceExport(&buf, &base, func(yield func(*ItemVersionExport) error) error {
			for i := range fx.ItemVersions {
				if err := yield(&fx.ItemVersions[i]); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("EncodeWorkspaceExport: %v", err)
		}

		var got WorkspaceExport
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal got: %v (raw=%s)", err, buf.String())
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("decoded mismatch:\n got: %+v\nwant: %+v", got, want)
		}
	})

	t.Run("nil iterator omits item_versions", func(t *testing.T) {
		fx := fullExportFixture()
		fxNoVer := *fx
		fxNoVer.ItemVersions = nil
		wantRaw, err := json.Marshal(&fxNoVer)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		var want WorkspaceExport
		if err := json.Unmarshal(wantRaw, &want); err != nil {
			t.Fatalf("unmarshal want: %v", err)
		}

		var buf bytes.Buffer
		if err := EncodeWorkspaceExport(&buf, fx, nil); err != nil {
			t.Fatalf("EncodeWorkspaceExport(nil): %v", err)
		}
		if bytes.Contains(buf.Bytes(), []byte(`"item_versions"`)) {
			t.Fatalf("expected no item_versions key when iterator is nil: %s", buf.String())
		}
		var got WorkspaceExport
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal got: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("decoded mismatch:\n got: %+v\nwant: %+v", got, want)
		}
	})

	t.Run("empty iterator omits item_versions", func(t *testing.T) {
		fx := fullExportFixture()
		fx.ItemVersions = []ItemVersionExport{}
		wantRaw, err := json.Marshal(fx)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		var want WorkspaceExport
		if err := json.Unmarshal(wantRaw, &want); err != nil {
			t.Fatalf("unmarshal want: %v", err)
		}

		var buf bytes.Buffer
		err = EncodeWorkspaceExport(&buf, fx, func(yield func(*ItemVersionExport) error) error {
			return nil
		})
		if err != nil {
			t.Fatalf("EncodeWorkspaceExport(empty): %v", err)
		}
		if bytes.Contains(buf.Bytes(), []byte(`"item_versions"`)) {
			t.Fatalf("expected no item_versions key when iterator yields nothing: %s", buf.String())
		}
		var got WorkspaceExport
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal got: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("decoded mismatch:\n got: %+v\nwant: %+v", got, want)
		}
	})
}

func TestDecodeWorkspaceExport_MatchesLegacyUnmarshal(t *testing.T) {
	t.Parallel()

	fullJSON, err := json.Marshal(fullExportFixture())
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	corpus := []struct {
		name string
		raw  string
	}{
		{name: "full v4 export", raw: string(fullJSON)},
		{name: "top-level null", raw: `null`},
		{name: "empty object", raw: `{}`},
		{name: "v1 minimal", raw: `{"version":1,"workspace":{"name":"W","slug":"w"},"collections":[],"items":[]}`},
		{name: "v2 schedule_changes null", raw: `{"version":1,"schedule_changes":null}`},
		{name: "v2 schedule_changes empty", raw: `{"version":1,"schedule_changes":[]}`},
		{name: "v3 schedule_links null", raw: `{"version":2,"schedule_links":null}`},
		{name: "v3 schedule_links empty", raw: `{"version":2,"schedule_links":[]}`},
		{name: "v4 schedule_gates null", raw: `{"version":3,"schedule_gates":null}`},
		{name: "v4 schedule_gates empty", raw: `{"version":3,"schedule_gates":[]}`},
		{name: "v4 item deleted_at explicit null", raw: `{"version":3,"items":[{"id":"1","deleted_at":null}]}`},
		{name: "v4 item deleted_at empty string", raw: `{"version":3,"items":[{"id":"1","deleted_at":""}]}`},
		{name: "v4 item deleted_at timestamp", raw: `{"version":4,"items":[{"id":"1","deleted_at":"2026-09-01T00:00:00Z"}]}`},
		{name: "v2 schedule id null", raw: `{"version":1,"schedules":[{"id":null,"item_id":"1","kind":"deadline","dtstart":"2026-09-01T00:00:00Z","anchor":"calendar","audience":"human","created_at":"2026-09-01T00:00:00Z","updated_at":"2026-09-01T00:00:00Z"}]}`},
		{name: "v2 schedule occurrence actual_done_at", raw: `{"version":1,"schedules":[{"item_id":"1","kind":"deadline","dtstart":"2026-09-01T00:00:00Z","anchor":"calendar","audience":"human","created_at":"2026-09-01T00:00:00Z","updated_at":"2026-09-01T00:00:00Z","occurrences":[{"actual_done_at":null,"occurrence_at":"2026-09-01T00:00:00Z","occurrence_wall":"2026-09-01T00:00:00","state":"done","resolved_at":"2026-09-01T00:00:00Z"}]}]}`},
		{name: "schedule ended without ended_dismissed_at", raw: `{"version":2,"schedules":[{"item_id":"1","kind":"deadline","dtstart":"2026-09-01T00:00:00Z","anchor":"calendar","audience":"human","ended_at":"2026-09-02T00:00:00Z","created_at":"2026-09-01T00:00:00Z","updated_at":"2026-09-01T00:00:00Z"}]}`},
		{name: "schedule ended with empty ended_dismissed_at", raw: `{"version":2,"schedules":[{"item_id":"1","kind":"deadline","dtstart":"2026-09-01T00:00:00Z","anchor":"calendar","audience":"human","ended_at":"2026-09-02T00:00:00Z","ended_dismissed_at":"","created_at":"2026-09-01T00:00:00Z","updated_at":"2026-09-01T00:00:00Z"}]}`},
		{name: "Items case variant", raw: `{"version":1,"Items":[{"id":"i1","title":"Case","slug":"case","content":"hi","fields":"{}","tags":"[]"}],"Collections":[{"id":"c1","name":"Tasks","slug":"tasks"}]}`},
		{name: "explicit null slices and unknown key", raw: `{"version":4,"collections":null,"items":null,"comments":null,"item_versions":null,"unknown_extra":{"nested":[1,2,3]}}`},
	}

	for _, tc := range corpus {
		t.Run(tc.name, func(t *testing.T) {
			var want WorkspaceExport
			wantErr := legacyUnmarshalWorkspaceExport([]byte(tc.raw), &want)

			got, gotErr := DecodeWorkspaceExport(strings.NewReader(tc.raw), nil)
			if (wantErr != nil) != (gotErr != nil) {
				t.Fatalf("error mismatch: gotErr=%v wantErr=%v", gotErr, wantErr)
			}
			if wantErr != nil {
				return
			}
			if got.HasV2ScheduleMembers() != want.HasV2ScheduleMembers() {
				t.Errorf("HasV2ScheduleMembers = %v, want %v", got.HasV2ScheduleMembers(), want.HasV2ScheduleMembers())
			}
			if got.HasV3ScheduleMembers() != want.HasV3ScheduleMembers() {
				t.Errorf("HasV3ScheduleMembers = %v, want %v", got.HasV3ScheduleMembers(), want.HasV3ScheduleMembers())
			}
			if got.HasV4ScheduleMembers() != want.HasV4ScheduleMembers() {
				t.Errorf("HasV4ScheduleMembers = %v, want %v", got.HasV4ScheduleMembers(), want.HasV4ScheduleMembers())
			}
			if !reflect.DeepEqual(*got, want) {
				t.Errorf("struct mismatch:\n got: %+v\nwant: %+v", *got, want)
			}
		})
	}
}
