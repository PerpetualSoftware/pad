package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestRoleArrival_BulkAssignAppendsBelowLine(t *testing.T) {
	t.Parallel()
	f := newParkEndpointFixture(t)
	// f.items have role_sort_order 1024, 2048, 3072 in f.role.

	// Park after the last item in f.role (items[2] @ 3072) -> P = 4096.
	parkPath := "/api/v1/workspaces/" + f.ws.Slug + "/agent-roles/" + f.role.Slug + "/park"
	rr := doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"after": f.items[2].Ref}, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT park after last: %d %s", rr.Code, rr.Body.String())
	}

	// Seed two unassigned items to bulk-assign to f.role.
	u1, err := f.srv.store.CreateItem(f.ws.ID, f.visibleColl.ID, models.ItemCreate{
		Title: "Bulk One", Fields: `{"status":"open"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem u1: %v", err)
	}
	u2, err := f.srv.store.CreateItem(f.ws.ID, f.visibleColl.ID, models.ItemCreate{
		Title: "Bulk Two", Fields: `{"status":"open"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem u2: %v", err)
	}

	bulkPath := "/api/v1/workspaces/" + f.ws.Slug + "/items/bulk"
	rr = doRequestWithHeaders(f.srv, http.MethodPost, bulkPath, map[string]any{
		"op":            "assign",
		"ids":           []string{u1.ID, u2.ID},
		"agent_role_id": f.role.ID,
	}, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("POST bulk assign: %d %s", rr.Code, rr.Body.String())
	}

	got1, err := f.srv.store.GetItem(u1.ID)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := f.srv.store.GetItem(u2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got1.RoleSortOrder != 5120 {
		t.Fatalf("bulk u1 role_sort_order = %d, want 5120 (max(3072, 4096) + 1024)", got1.RoleSortOrder)
	}
	if got2.RoleSortOrder != 6144 {
		t.Fatalf("bulk u2 role_sort_order = %d, want 6144 (max(5120, 4096) + 1024)", got2.RoleSortOrder)
	}

	// Both newly bulk-assigned items must now be parked (409 item_parked on claim).
	claimPath := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + u1.Slug + "/claim"
	rr = doRequestWithHeaders(f.srv, http.MethodPost, claimPath, map[string]any{"holder": "agent-a"}, bearerHdr(f.editorTok))
	if rr.Code != http.StatusConflict {
		t.Fatalf("claim bulk-assigned parked item: got %d %s, want 409", rr.Code, rr.Body.String())
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	parseJSON(t, rr, &env)
	if env.Error.Code != "item_parked" {
		t.Fatalf("claim code = %q, want item_parked", env.Error.Code)
	}
}
