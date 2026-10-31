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
