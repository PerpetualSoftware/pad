package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestTASK4423_R01Carry(t *testing.T) {
	for _, mode := range []string{"copy", "move", "hidden gate only", "bare readable gate"} {
		t.Run(mode, func(t *testing.T) {
			f := newCopyPreflightFixture(t)
			u := mustUser(t, f.srv, "carry-editor@example.com", "carryeditor", "")
			for _, w := range []*models.Workspace{f.wsA, f.wsB} {
				if err := f.srv.store.AddWorkspaceMember(w.ID, u.ID, "editor"); err != nil {
					t.Fatal(err)
				}
			}
			for _, x := range []struct{ w, c string }{{f.wsA.ID, f.collA.ID}, {f.wsB.ID, f.collB.ID}} {
				if err := f.srv.store.SetMemberCollectionAccess(x.w, u.ID, "specific", []string{x.c}); err != nil {
					t.Fatal(err)
				}
			}
			hidden := mustCollection(t, f.srv, f.wsA.ID, "Private")
			visibleEnd := mustItem(t, f.srv, f.wsA.ID, f.collA.ID, "Visible end")
			hiddenEnd := mustItem(t, f.srv, f.wsA.ID, hidden.ID, "Hidden end")
			access := models.CommitmentAccess{Readable: map[string]bool{}, Editable: map[string]bool{}}
			for _, it := range []*models.Item{f.source, visibleEnd, hiddenEnd} {
				access.Readable[it.ID] = true
				access.Editable[it.ID] = true
			}
			if mode == "copy" || mode == "move" {
				for _, it := range []*models.Item{f.source, visibleEnd, hiddenEnd} {
					if _, err := f.srv.store.CreateCommitmentSchedule(f.wsA.ID, it.ID, models.ScheduleCreate{Dtstart: "2099-01-01T09:00", Tz: "UTC"}, models.CommitmentRolesInput{}); err != nil {
						t.Fatal(err)
					}
				}
				for _, it := range []*models.Item{visibleEnd, hiddenEnd} {
					if _, err := f.srv.store.CreateScheduleLink(f.wsA.ID, models.ScheduleLinkCreate{ParentItemID: f.source.ID, ParentRole: "target", ChildItemID: it.ID, ChildRole: "target", Offset: models.ScheduleLinkOffset{Mode: "elapsed"}}, access); err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode != "bare readable gate" {
				if _, err := f.srv.store.CreateScheduleGate(f.wsA.ID, f.source.ID, hiddenEnd.ID, access); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "hidden gate only" {
				if _, err := f.srv.store.CreateScheduleGate(f.wsA.ID, f.source.ID, visibleEnd.ID, access); err != nil {
					t.Fatal(err)
				}
			}
			body := f.baseBody()
			body["field_overrides"] = map[string]any{"code": "123", "ticket": "T-1"}
			body["archive_source"] = mode == "move"
			tok, err := f.srv.store.CreateAPIToken(u.ID, models.APITokenCreate{Name: "carry"}, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{"/copy/preflight", "/copy"} {
				raw, _ := json.Marshal(body)
				rr := bearerCall(t, f.srv, http.MethodPost, "/api/v1/workspaces/"+f.wsA.Slug+"/items/"+f.source.Slug+suffix, tok.Token, raw)
				if rr.Code != 200 && rr.Code != 201 {
					t.Fatalf("%s: %d %s", suffix, rr.Code, rr.Body.String())
				}
				m := schedMap(t, rr)
				warnings, _ := m["warnings"].(map[string]any)
				sc, _ := warnings["schedule"].(map[string]any)
				if mode == "hidden gate only" {
					if sc != nil {
						t.Fatalf("hidden-only gate exposed warning: %s", rr.Body.String())
					}
					continue
				}
				wantLinks := float64(1)
				if mode == "bare readable gate" {
					wantLinks = 0
				}
				if sc == nil || sc["links_dropped"] != wantLinks || sc["gates_dropped"] != float64(1) {
					t.Errorf("%s carry counts: %s", suffix, rr.Body.String())
				}
			}
		})
	}
}
