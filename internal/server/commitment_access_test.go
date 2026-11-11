package server

import (
	"net/http/httptest"
	"strings"
	"testing"

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
