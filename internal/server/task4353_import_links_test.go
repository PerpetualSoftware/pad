package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4353 (bundle version 3), both import doors: a bundle's links import onto
// the new schedules, a link that fails a live write's checks is dropped and named
// in the 201 as dropped_schedules with a links:* reason, an older bundle that
// carries links is refused as a 400, and the workspace exports its links again.
func TestTASK4353_ImportCarriesAndReportsLinks(t *testing.T) {
	t.Parallel()
	const ts = "2026-10-15T00:00:00Z"
	schedule := func(id, item string) models.ScheduleExport {
		return models.ScheduleExport{ID: id, ItemID: item, Kind: "check", Dtstart: "2030-01-01T09:00:00", Tz: "UTC", Anchor: "calendar",
			Audience: "both", NextAt: "2030-01-01T09:00:00Z", NextWall: "2030-01-01T09:00:00", CreatedAt: ts, UpdatedAt: ts}
	}
	link := func(parent, child string) models.ScheduleLinkExport {
		return models.ScheduleLinkExport{ParentScheduleID: parent, ParentRole: "target", ChildScheduleID: child, ChildRole: "target",
			Offset: models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: 3600}, CancelPolicy: models.LinkWaitingOn, CreatedAt: ts, UpdatedAt: ts}
	}
	items := []models.ItemExport{}
	for i, title := range []string{"One", "Two"} {
		items = append(items, models.ItemExport{ID: string(rune('a' + i)), CollectionID: "c", Title: title, Slug: title, Fields: "{}", Tags: "[]", ItemNumber: i + 1, CreatedAt: ts, UpdatedAt: ts})
	}
	build := func(version int) []byte {
		raw, err := json.Marshal(models.WorkspaceExport{
			Version:   version,
			Workspace: models.WorkspaceExportMeta{Name: "Links Import", Slug: "links-import", Settings: "{}"},
			Collections: []models.CollectionExport{{
				ID: "c", Name: "Tasks", Slug: "tasks", Prefix: "TASK", Settings: "{}", CreatedAt: ts, UpdatedAt: ts, Schema: `{"fields":[]}`,
			}},
			Items:     items,
			Schedules: []models.ScheduleExport{schedule("s1", "a"), schedule("s2", "b")},
			// s1 -> s2 imports; s2 -> s1 would close a cycle; the third names no schedule.
			ScheduleLinks: []models.ScheduleLinkExport{link("s1", "s2"), link("s2", "s1"), link("s1", "missing")},
		})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	want := []models.ImportScheduleDrop{{Item: "TASK-1", Reason: "links:cycle"}, {Item: "", Reason: "links:orphan"}}

	for _, door := range []struct {
		name   string
		server func(*testing.T) *Server
		post   func(*testing.T, *Server, []byte) *httptest.ResponseRecorder
	}{
		{"json", testServer, func(t *testing.T, srv *Server, body []byte) *httptest.ResponseRecorder {
			return rawJSONRequest(srv, "POST", "/api/v1/workspaces/import", string(body))
		}},
		{"bundle", func(t *testing.T) *Server {
			srv, _ := testServerWithAttachments(t)
			return srv
		}, func(t *testing.T, srv *Server, body []byte) *httptest.ResponseRecorder {
			return postBundle(srv, "LinksBundle", gzipTar(t, []bundleEntry{{"pad-export.json", body}}))
		}},
	} {
		t.Run(door.name, func(t *testing.T) {
			srv := door.server(t)
			rr := door.post(t, srv, build(3))
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
			exp := rawJSONRequest(srv, "GET", "/api/v1/workspaces/"+got.Slug+"/export", "")
			var out models.WorkspaceExport
			if exp.Code != http.StatusOK || json.Unmarshal(exp.Body.Bytes(), &out) != nil {
				t.Fatalf("export: %d %s", exp.Code, exp.Body.String())
			}
			if out.Version != models.WorkspaceExportVersion || len(out.ScheduleLinks) != 1 || out.ScheduleLinks[0].Offset.ElapsedSecs != 3600 {
				t.Fatalf("the restored workspace exports version %d with links %+v", out.Version, out.ScheduleLinks)
			}

			// A version 2 bundle that carries links is refused, as a bundle the caller can fix.
			refused := door.post(t, srv, build(2))
			if refused.Code != http.StatusBadRequest {
				t.Fatalf("v2 bundle with links: %d %s, want 400", refused.Code, refused.Body.String())
			}
		})
	}
}
