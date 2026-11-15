package server

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func readReopenID(t *testing.T, f *schedHTTP) string {
	t.Helper()
	rr := f.as(f.token(f.owner, ""), "GET", f.path(f.item), nil)
	if rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	last := schedMap(t, rr)["last_resolution"].(map[string]any)
	id, _ := last["resolution_id"].(string)
	if id == "" {
		t.Fatal("GET missing resolution ID")
	}
	return id
}

func TestReopenEligibilityOnlyOnReadAndPost(t *testing.T) {
	f := newGraphHTTP(t, models.CommitmentRolesInput{})
	check := func(raw []byte) {
		t.Helper()
		var walk func(any)
		walk = func(v any) {
			switch v := v.(type) {
			case map[string]any:
				for k, x := range v {
					if k == "last_resolution" || k == "resolution_id" {
						t.Errorf("write response contains %s", k)
					}
					walk(x)
				}
			case []any:
				for _, x := range v {
					walk(x)
				}
			}
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		walk(v)
	}
	rr := f.resolve(map[string]any{"state": "snooze", "occurrence_at": f.dates[0], "until": time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339)})
	if rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	check(rr.Body.Bytes())
	body := map[string]any{"role": "target", "to": time.Now().UTC().Add(31 * 24 * time.Hour).Format(time.RFC3339), "reason": "Replan"}
	for _, route := range []string{"move-preview", "move"} {
		rr = f.as(f.token(f.owner, ""), "POST", f.path(f.item)+"/"+route, body)
		if rr.Code != 200 {
			t.Fatal(rr.Body.String())
		}
		check(rr.Body.Bytes())
		body["revision"] = schedMap(t, rr)["revision"]
	}
	readReopenID(t, f.schedHTTP)
}

func TestReopenViewerGetsNoLastResolution(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(map[bool]string{false: "unlinked", true: "linked"}[linked], func(t *testing.T) {
			f := newGraphHTTP(t, models.CommitmentRolesInput{}, models.CommitmentRolesInput{})
			if linked {
				f.link(0, 1, "target", "target", "")
			}
			rr := f.resolve(map[string]any{"state": "done", "occurrence_at": f.dates[0], "end_schedule": true})
			if rr.Code != 200 {
				t.Fatal(rr.Body.String())
			}
			viewer := f.member("viewer@example.com", "viewer")
			rr = f.as(f.token(viewer, ""), "GET", f.path(f.item), nil)
			if rr.Code != 200 {
				t.Fatal(rr.Body.String())
			}
			if _, ok := schedMap(t, rr)["last_resolution"]; ok {
				t.Fatal("viewer received last_resolution")
			}
		})
	}
}

func TestReopenSupersededIDRefused(t *testing.T) {
	f := newGraphHTTP(t, models.CommitmentRolesInput{})
	for i, until := range []string{time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339), time.Now().UTC().Add(31 * 24 * time.Hour).Format(time.RFC3339)} {
		rr := f.resolve(map[string]any{"state": "snooze", "occurrence_at": f.dates[0], "until": until})
		if rr.Code != 200 {
			t.Fatal(rr.Body.String())
		}
		if i == 0 {
			f.dates = append(f.dates, readReopenID(t, f.schedHTTP))
		}
	}
	before := graphHTTPState(t, f)
	f.expectErr(f.as(f.token(f.owner, ""), "POST", f.path(f.item)+"/reopen", map[string]any{"resolution_id": f.dates[1]}), 409, "reopen_conflict", "")
	if !reflect.DeepEqual(before, graphHTTPState(t, f)) {
		t.Fatal("refusal changed state")
	}
}

func TestReopenDependencyBodyConstant(t *testing.T) {
	for _, kind := range []string{"link", "milestone"} {
		t.Run(kind, func(t *testing.T) {
			f := newGraphHTTP(t, models.CommitmentRolesInput{}, models.CommitmentRolesInput{}, models.CommitmentRolesInput{})
			exec := func(q string, args ...any) {
				t.Helper()
				if _, err := f.srv.store.DB().Exec(q, args...); err != nil {
					t.Fatal(err)
				}
			}
			sc, err := f.srv.store.GetSchedule(f.ws.ID, f.item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "link" {
				f.link(0, 1, "target", "target", "")
			} else {
				exec(`INSERT INTO milestone_children(id,workspace_id,schedule_id,child_item_id,kind,added_by,added_at) VALUES('child',?,?,?,'child',?,'2026-01-01')`, f.ws.ID, sc.ID, f.items[1].ID, f.owner.ID)
			}
			rr := f.resolve(map[string]any{"state": "done", "occurrence_at": f.dates[0], "end_schedule": true})
			if rr.Code != 200 {
				t.Fatal(rr.Body.String())
			}
			id := readReopenID(t, f.schedHTTP)
			var firstGet, firstPost []byte
			check := func(label, token string) {
				t.Helper()
				rr := f.as(token, "GET", f.path(f.item), nil)
				if rr.Code != 200 {
					t.Fatal(rr.Body.String())
				}
				var body map[string]json.RawMessage
				if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				get := body["last_resolution"]
				if !bytes.Contains(get, []byte(`"reopen_code":"reopen_dependency"`)) {
					t.Errorf("%s: %s", label, get)
				}
				rr = f.as(token, "POST", f.path(f.item)+"/reopen", map[string]any{"resolution_id": id})
				f.expectErr(rr, 409, "reopen_dependency", "")
				if firstGet == nil {
					firstGet = append([]byte{}, get...)
					firstPost = append([]byte{}, rr.Body.Bytes()...)
				} else if !bytes.Equal(firstGet, get) || !bytes.Equal(firstPost, rr.Body.Bytes()) {
					t.Errorf("%s changed eligibility/refusal: %s %s", label, get, rr.Body.String())
				}
			}
			owner := f.token(f.owner, "")
			check("visible", owner)
			guest := f.token(f.guest(0), "")
			check("hidden", guest)
			exec(`UPDATE items SET seq=seq+1,title='Changed' WHERE id=?`, f.items[1].ID)
			check("changed child", guest)
			exec(`UPDATE items SET deleted_at='2026-01-01' WHERE id=?`, f.items[1].ID)
			check("soft deleted", guest)
			if kind == "link" {
				f.link(0, 2, "target", "target", "")
				check("two partners", guest)
				exec(`INSERT INTO schedule_gates(id,workspace_id,blocked_item_id,blocker_item_id,created_at) VALUES('gate',?,?,?,'2026-01-01')`, f.ws.ID, f.item.ID, f.items[2].ID)
				check("open blocker", guest)
				exec(`UPDATE items SET fields='{"status":"done"}',seq=seq+1 WHERE id=?`, f.items[2].ID)
				exec(`UPDATE schedule_gates SET opened_at='2026-01-02' WHERE id='gate'`)
				check("closed blocker", guest)
			}
			exec(`UPDATE schedule_reopen_checkpoints SET blocked_code='reopen_conflict',reopened_change_id='consumed' WHERE schedule_id=?`, sc.ID)
			check("consumed conflict", guest)
		})
	}
}

func graphHTTPState(t *testing.T, f *graphHTTP) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, table := range []string{"items", "item_schedules", "item_reminders", "schedule_occurrences", "schedule_occurrence_overrides", "milestone_completion_evidence", "schedule_changes", "activities", "comments", "schedule_reopen_checkpoints"} {
		rows, err := f.srv.store.DB().Query("SELECT * FROM " + table + " ORDER BY 1")
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var all [][]any
		for rows.Next() {
			vals := make([]any, len(cols))
			dest := make([]any, len(cols))
			for i := range vals {
				dest[i] = &vals[i]
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			all = append(all, vals)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		raw, err := json.Marshal(all)
		if err != nil {
			t.Fatal(err)
		}
		out[table] = string(raw)
	}
	return out
}

func TestReopenInvalidationsHTTP(t *testing.T) {
	for _, kind := range []string{"reminder fire", "item seq", "close item too", "link added after"} {
		t.Run(kind, func(t *testing.T) {
			f := newGraphHTTP(t, models.CommitmentRolesInput{}, models.CommitmentRolesInput{})
			rr := f.resolve(map[string]any{"state": "snooze", "occurrence_at": f.dates[0], "until": time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339)})
			if rr.Code != 200 {
				t.Fatal(rr.Body.String())
			}
			id := readReopenID(t, f.schedHTTP)
			code := "reopen_conflict"
			var err error
			switch kind {
			case "reminder fire":
				_, err = f.srv.store.DB().Exec(`UPDATE item_reminders SET fired_at='2026-11-15T00:00:00Z' WHERE item_id=?`, f.item.ID)
			case "item seq":
				f.item, err = f.srv.store.UpdateItem(f.item.ID, models.ItemUpdate{Title: func() *string { v := "Changed"; return &v }()})
			case "close item too":
				_, err = f.srv.store.UpdateItem(f.item.ID, models.ItemUpdate{Fields: func() *string { v := `{"status":"done"}`; return &v }(), Force: true})
			case "link added after":
				f.link(0, 1, "target", "target", "")
				code = "reopen_dependency"
			}
			if err != nil {
				t.Fatal(err)
			}
			before := graphHTTPState(t, f)
			f.expectErr(f.as(f.token(f.owner, ""), "POST", f.path(f.item)+"/reopen", map[string]any{"resolution_id": id}), 409, code, "")
			if !reflect.DeepEqual(before, graphHTTPState(t, f)) {
				t.Fatal("refusal changed state")
			}
		})
	}
}
