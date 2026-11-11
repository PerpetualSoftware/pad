package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestTASK4423_CommitmentAccessHistory(t *testing.T) {
	f := newGraphHTTP(t, models.CommitmentRolesInput{})
	unrelated := mustItem(t, f.srv, f.ws.ID, f.coll.ID, "Unrelated")
	g := f.guest(0)
	r := httptest.NewRequest("GET", f.path(f.item), nil)
	r = r.WithContext(WithCurrentUser(r.Context(), g))
	a, err := f.srv.buildCommitmentAccess(r, f.srv.store.Q(), f.ws.ID, f.item.ID, commitmentAccessLive)
	if err != nil {
		t.Fatal(err)
	}
	// Even an owner should only walk the graph candidates.
	r = r.WithContext(WithCurrentUser(r.Context(), f.owner))
	ownerAccess, err := f.srv.buildCommitmentAccess(r, f.srv.store.Q(), f.ws.ID, f.item.ID, commitmentAccessLive)
	if err != nil {
		t.Fatal(err)
	}
	if ownerAccess.Readable[unrelated.ID] || !a.Readable[f.item.ID] {
		t.Errorf("access must include the request item and exclude unrelated items: %+v", ownerAccess)
	}
	if _, err := f.srv.store.DB().Exec(`INSERT INTO schedule_changes (id,workspace_id,item_id,schedule_id,change_type,role,reason,reason_source,actor,root_change_id,recorded_at) SELECT i.id,i.workspace_id,i.id,sc.id,'update','deadline','Own history reason','user','own-actor',i.id,'2026-01-01T00:00:00Z' FROM items i JOIN item_schedules sc ON sc.item_id=i.id WHERE i.id=?`, f.item.ID); err != nil {
		t.Fatal(err)
	}
	rr := f.as(f.token(g, ""), "GET", f.path(f.item), nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Own history reason") || !strings.Contains(rr.Body.String(), "own-actor") {
		t.Fatalf("unlinked own history redacted: %d %s", rr.Code, rr.Body.String())
	}
}

func TestTASK4423_CommitmentOwnerWriteBoundary(t *testing.T) {
	for _, boundary := range []string{"read-only token", "app collection"} {
		t.Run(boundary, func(t *testing.T) {
			f := newGraphHTTP(t, graphHard(), graphHard())
			f.link(0, 1, "deadline", "deadline", "")
			dependentCollection := mustCollection(t, f.srv, f.ws.ID, "Dependent")
			if _, err := f.srv.store.DB().Exec(`UPDATE items SET collection_id=? WHERE id=?`, dependentCollection.ID, f.items[1].ID); err != nil {
				t.Fatal(err)
			}
			p := f.reneg(f.token(f.owner, ""), true, "", false)
			if p.Code != 200 {
				t.Fatalf("preview: %d %s", p.Code, p.Body.String())
			}
			r := httptest.NewRequest("POST", f.path(f.item)+"/renegotiate", nil)
			r = r.WithContext(WithCurrentUser(r.Context(), f.owner))
			if boundary == "read-only token" {
				r = r.WithContext(WithTokenScopes(r.Context(), `["read"]`))
			} else {
				r = r.WithContext(withAppContextForTest(r, &appContext{Companions: []string{f.coll.ID}}))
			}
			a, err := f.srv.buildCommitmentAccess(r, f.srv.store.Q(), f.ws.ID, f.item.ID, commitmentAccessLive)
			if err != nil {
				t.Fatal(err)
			}
			if !a.Readable[f.items[1].ID] || a.Editable[f.items[1].ID] {
				t.Errorf("owner dependent must be readable without edit authority: %+v", a)
			}
			before, err := f.srv.store.GetCommitment(f.ws.ID, f.items[1].ID)
			if err != nil {
				t.Fatal(err)
			}
			base, _ := time.Parse(time.RFC3339, f.dates[0])
			to := base.Add(15 * 24 * time.Hour).Format(time.RFC3339)
			// HTTP rejects read-only writes before planning; exercise the cascade
			// with the same access maps to verify dependent authority independently.
			_, err = f.srv.store.RenegotiateCommitment(f.ws.ID, f.item.ID, f.owner.ID, models.CommitmentRenegotiateInput{Access: a, Deadline: &to, Reason: "Replan", Revision: schedMap(t, p)["revision"].(string), AcknowledgeViolations: true}, nil)
			rr := httptest.NewRecorder()
			f.srv.writeScheduleError(rr, r, err)
			f.expectErr(rr, 409, "deadline_approval_required", "")
			after, err := f.srv.store.GetCommitment(f.ws.ID, f.items[1].ID)
			if err != nil || *before.Roles.Deadline != *after.Roles.Deadline {
				t.Fatalf("refusal changed dependent deadline: %v", err)
			}
		})
	}
}
