package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4104 D2, both import doors: a bundle's schedules import, and a malformed
// one is dropped, counted and named in the 201 as dropped_schedules while its
// item and the rest of the import survive.
func TestTASK4104_ImportReportsDroppedSchedules(t *testing.T) {
	const ts = "2026-10-15T00:00:00Z"
	good := models.ScheduleExport{
		ItemID: "11111111-1111-4111-8111-111111111111", Kind: "check", Dtstart: "2030-01-01T09:00:00", Tz: "UTC",
		Rrule: "FREQ=DAILY", Anchor: "calendar", Audience: "both",
		NextAt: "2030-01-01T09:00:00Z", NextWall: "2030-01-01T09:00:00", CreatedAt: ts, UpdatedAt: ts,
	}
	bad := good
	bad.ItemID = "22222222-2222-4222-8222-222222222222"
	bad.Rrule = "FREQ=HOURLY;BYSETPOS=1"
	bundle := models.WorkspaceExport{
		Version:   1,
		Workspace: models.WorkspaceExportMeta{Name: "Sched Import", Slug: "sched-import", Settings: "{}"},
		Collections: []models.CollectionExport{{
			ID: "c", Name: "Tasks", Slug: "tasks", Prefix: "TASK", Settings: "{}", CreatedAt: ts, UpdatedAt: ts, Schema: `{"fields":[]}`,
		}},
		Items: []models.ItemExport{
			{ID: good.ItemID, CollectionID: "c", Title: "One", Slug: "one", Fields: "{}", Tags: "[]", ItemNumber: 1, CreatedAt: ts, UpdatedAt: ts},
			{ID: bad.ItemID, CollectionID: "c", Title: "Two", Slug: "two", Fields: "{}", Tags: "[]", ItemNumber: 2, CreatedAt: ts, UpdatedAt: ts},
		},
		Schedules: []models.ScheduleExport{good, bad},
	}
	export, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	want := []models.ImportScheduleDrop{{Item: "TASK-2", Reason: "timing:combination"}}

	for _, door := range []struct {
		name   string
		server func(*testing.T) *Server
		post   func(*Server) *httptest.ResponseRecorder
	}{
		{"json", testServer, func(srv *Server) *httptest.ResponseRecorder {
			return rawJSONRequest(srv, "POST", "/api/v1/workspaces/import", string(export))
		}},
		{"bundle", func(t *testing.T) *Server {
			srv, _ := testServerWithAttachments(t)
			return srv
		}, func(srv *Server) *httptest.ResponseRecorder {
			return postBundle(srv, "SchedBundle", gzipTar(t, []bundleEntry{{"pad-export.json", export}}))
		}},
	} {
		t.Run(door.name, func(t *testing.T) {
			rr := door.post(door.server(t))
			if rr.Code != http.StatusCreated {
				t.Fatalf("import: %d %s, want 201", rr.Code, rr.Body.String())
			}
			var got struct {
				Slug             string                      `json:"slug"`
				DroppedSchedules []models.ImportScheduleDrop `json:"dropped_schedules"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v (%s)", err, rr.Body.String())
			}
			if got.Slug == "" || !reflect.DeepEqual(got.DroppedSchedules, want) {
				t.Errorf("slug %q dropped_schedules = %+v, want %+v", got.Slug, got.DroppedSchedules, want)
			}
		})
	}
}

func TestImportBundleV2NativeRoles(t *testing.T) {
	srv := testServer(t)
	const body = `{"version":2,"workspace":{"name":"Native import","slug":"native-import","settings":"{}"},"collections":[{"id":"c","name":"Tasks","slug":"tasks","prefix":"TASK","schema":"{\"fields\":[]}","settings":"{}","created_at":"2026-11-01T00:00:00Z","updated_at":"2026-11-01T00:00:00Z"}],"items":[{"id":"i","collection_id":"c","title":"Renew","slug":"renew","fields":"{}","tags":"[]","item_number":1,"created_at":"2026-11-01T00:00:00Z","updated_at":"2026-11-01T00:00:00Z"}],"schedules":[{"id":"s","item_id":"i","kind":"check","dtstart":"2030-01-01T09:00:00","tz":"UTC","anchor":"calendar","audience":"both","next_at":"2030-01-01T09:00:00Z","next_wall":"2030-01-01T09:00:00","created_at":"2026-11-01T00:00:00Z","updated_at":"2026-11-01T00:00:00Z","roles":{"target_days":2,"target_secs":60,"deadline_days":3,"deadline_secs":0,"deadline_behavior":"lapses","roles_origin":"native","outcome":"Renewed"}}]}`
	rr := rawJSONRequest(srv, "POST", "/api/v1/workspaces/import", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("v2 import: %d %s", rr.Code, rr.Body.String())
	}
	var ws struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &ws); err != nil {
		t.Fatal(err)
	}
	rr = rawJSONRequest(srv, "GET", "/api/v1/workspaces/"+ws.Slug+"/export", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("export: %d %s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	roles := out["schedules"].([]any)[0].(map[string]any)["roles"].(map[string]any)
	if roles["roles_origin"] != "native" || roles["target_days"] != float64(2) || roles["deadline_behavior"] != "lapses" || roles["outcome"] != "Renewed" {
		t.Fatal(roles)
	}
	var input map[string]any
	_ = json.Unmarshal([]byte(body), &input)
	input["version"] = 3
	raw, _ := json.Marshal(input)
	unsupported := rawJSONRequest(srv, "POST", "/api/v1/workspaces/import", string(raw))
	input["version"] = 1
	raw, _ = json.Marshal(input)
	refused := rawJSONRequest(srv, "POST", "/api/v1/workspaces/import", string(raw))
	if refused.Code != unsupported.Code || refused.Code == http.StatusCreated {
		t.Fatalf("v1/v2 refusal status=%d unsupported=%d", refused.Code, unsupported.Code)
	}
}
