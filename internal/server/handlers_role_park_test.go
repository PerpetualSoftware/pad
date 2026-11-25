package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

type parkEndpointFixture struct {
	srv           *Server
	ws            *models.Workspace
	wsB           *models.Workspace
	owner         *models.User
	ownerTok      string
	editor        *models.User
	editorTok     string
	restrictedEd  *models.User
	restrictedTok string
	viewer        *models.User
	viewerTok     string
	visibleColl   *models.Collection
	hiddenColl    *models.Collection
	refColl       *models.Collection
	role          *models.AgentRole
	otherRole     *models.AgentRole
	items         []*models.Item // 3 open items in visibleColl assigned to role
	hiddenItem    *models.Item   // open item in hiddenColl assigned to role
	otherWSItem   *models.Item   // item in wsB
}

func newParkEndpointFixture(t *testing.T) *parkEndpointFixture {
	t.Helper()
	srv := testServer(t)

	mkUser := func(email, name string) *models.User {
		u, err := srv.store.CreateUser(models.UserCreate{
			Email: email, Name: name, Password: "pw-test-12345",
		})
		if err != nil {
			t.Fatalf("CreateUser %s: %v", email, err)
		}
		return u
	}
	mkPAT := func(userID, wsID string) string {
		tok, err := srv.store.CreateAPIToken(userID, models.APITokenCreate{
			Name: "test-pat", WorkspaceID: wsID,
		}, 0, 0)
		if err != nil {
			t.Fatalf("CreateAPIToken: %v", err)
		}
		return tok.Token
	}

	owner := mkUser("park-owner@example.com", "Alice Owner")
	editor := mkUser("park-editor@example.com", "Bob Editor")
	restrictedEd := mkUser("park-restricted@example.com", "Cara Restricted")
	viewer := mkUser("park-viewer@example.com", "Dan Viewer")

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "ParkWS", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	wsB, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "ParkOtherWS", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace B: %v", err)
	}
	for _, m := range []struct {
		wsID, uid, role string
	}{
		{ws.ID, owner.ID, "owner"},
		{ws.ID, editor.ID, "editor"},
		{ws.ID, restrictedEd.ID, "editor"},
		{ws.ID, viewer.ID, "viewer"},
		{wsB.ID, owner.ID, "owner"},
	} {
		if err := srv.store.AddWorkspaceMember(m.wsID, m.uid, m.role); err != nil {
			t.Fatalf("AddWorkspaceMember: %v", err)
		}
	}

	schema := `{"fields":[{"key":"status","type":"select","options":["open","in_progress","done"],"terminal_options":["done"],"default":"open"}]}`
	visColl, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", Prefix: "TASK", Schema: schema,
	})
	if err != nil {
		t.Fatalf("CreateCollection tasks: %v", err)
	}
	hidColl, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Secret", Slug: "secret", Prefix: "SEC", Schema: schema,
	})
	if err != nil {
		t.Fatalf("CreateCollection secret: %v", err)
	}
	noWork := false
	refColl, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Docs", Slug: "docs", Prefix: "DOC", Schema: schema, TracksWork: &noWork,
	})
	if err != nil {
		t.Fatalf("CreateCollection docs: %v", err)
	}
	collB, err := srv.store.CreateCollection(wsB.ID, models.CollectionCreate{
		Name: "OtherTasks", Slug: "other-tasks", Prefix: "OTHR", Schema: schema,
	})
	if err != nil {
		t.Fatalf("CreateCollection wsB: %v", err)
	}

	if err := srv.store.SetMemberCollectionAccess(ws.ID, restrictedEd.ID, "specific", []string{visColl.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	role, err := srv.store.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Implementer", Slug: "implementer"})
	if err != nil {
		t.Fatalf("CreateAgentRole: %v", err)
	}
	otherRole, err := srv.store.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Reviewer", Slug: "reviewer"})
	if err != nil {
		t.Fatalf("CreateAgentRole other: %v", err)
	}

	var items []*models.Item
	for i, title := range []string{"One", "Two", "Three"} {
		it, err := srv.store.CreateItem(ws.ID, visColl.ID, models.ItemCreate{
			Title: title, Fields: `{"status":"open"}`, AgentRoleID: &role.ID,
		})
		if err != nil {
			t.Fatalf("CreateItem %s: %v", title, err)
		}
		if err := srv.store.UpdateRoleSortOrder(ws.ID, []store.RoleSortUpdate{{ItemID: it.ID, RoleSortOrder: (i + 1) * 1024}}); err != nil {
			t.Fatalf("UpdateRoleSortOrder: %v", err)
		}
		it, _ = srv.store.GetItem(it.ID)
		items = append(items, it)
	}
	hiddenItem, err := srv.store.CreateItem(ws.ID, hidColl.ID, models.ItemCreate{
		Title: "Hidden Task", Fields: `{"status":"open"}`, AgentRoleID: &role.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem hidden: %v", err)
	}
	if err := srv.store.UpdateRoleSortOrder(ws.ID, []store.RoleSortUpdate{{ItemID: hiddenItem.ID, RoleSortOrder: 512}}); err != nil {
		t.Fatalf("UpdateRoleSortOrder hidden: %v", err)
	}
	hiddenItem, _ = srv.store.GetItem(hiddenItem.ID)

	otherWSItem, err := srv.store.CreateItem(wsB.ID, collB.ID, models.ItemCreate{
		Title: "Other WS Task", Fields: `{"status":"open"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem wsB: %v", err)
	}

	return &parkEndpointFixture{
		srv:           srv,
		ws:            ws,
		wsB:           wsB,
		owner:         owner,
		ownerTok:      mkPAT(owner.ID, ws.ID),
		editor:        editor,
		editorTok:     mkPAT(editor.ID, ws.ID),
		restrictedEd:  restrictedEd,
		restrictedTok: mkPAT(restrictedEd.ID, ws.ID),
		viewer:        viewer,
		viewerTok:     mkPAT(viewer.ID, ws.ID),
		visibleColl:   visColl,
		hiddenColl:    hidColl,
		refColl:       refColl,
		role:          role,
		otherRole:     otherRole,
		items:         items,
		hiddenItem:    hiddenItem,
		otherWSItem:   otherWSItem,
	}
}

func bearerHdr(tok string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + tok}
}

func TestParkEndpoint_ViewerAndRestrictedEditor403(t *testing.T) {
	t.Parallel()
	f := newParkEndpointFixture(t)
	parkPath := "/api/v1/workspaces/" + f.ws.Slug + "/agent-roles/" + f.role.Slug + "/park"

	// Viewer refused 403 on PUT and DELETE.
	rr := doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"at": "top"}, bearerHdr(f.viewerTok))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("viewer PUT park: got %d %s, want 403", rr.Code, rr.Body.String())
	}
	rr = doRequestWithHeaders(f.srv, http.MethodDelete, parkPath, nil, bearerHdr(f.viewerTok))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("viewer DELETE park: got %d %s, want 403", rr.Code, rr.Body.String())
	}

	// Restricted editor refused 403 forbidden on PUT and DELETE (gate 3 before anchor lookup).
	rrHidden := doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"before": f.hiddenItem.Ref}, bearerHdr(f.restrictedTok))
	rrMissing := doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"before": "TASK-99999"}, bearerHdr(f.restrictedTok))
	if rrHidden.Code != http.StatusForbidden || rrMissing.Code != http.StatusForbidden {
		t.Fatalf("restricted editor PUT: hidden=%d missing=%d, want 403", rrHidden.Code, rrMissing.Code)
	}
	if rrHidden.Body.String() != rrMissing.Body.String() {
		t.Fatalf("restricted editor 403 differed for hidden vs missing anchor:\n  hidden:  %s\n  missing: %s", rrHidden.Body.String(), rrMissing.Body.String())
	}
	rr = doRequestWithHeaders(f.srv, http.MethodDelete, parkPath, nil, bearerHdr(f.restrictedTok))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("restricted editor DELETE: got %d %s, want 403", rr.Code, rr.Body.String())
	}

	// Restricted owner (via shared fixture) is also refused 403.
	rf := newRestrictedOwnerVisibilityFixture(t)
	rRole, err := rf.srv.store.CreateAgentRole(rf.ws.ID, models.AgentRoleCreate{Name: "Impl"})
	if err != nil {
		t.Fatalf("CreateAgentRole rf: %v", err)
	}
	rfPath := "/api/v1/workspaces/" + rf.ws.Slug + "/agent-roles/" + rRole.ID + "/park"
	rr = doRequestWithHeaders(rf.srv, http.MethodPut, rfPath, map[string]any{"at": "top"}, rf.bearerHeaders())
	if rr.Code != http.StatusForbidden {
		t.Fatalf("restricted owner PUT: got %d %s, want 403", rr.Code, rr.Body.String())
	}

	// Control: unrestricted editor succeeds on PUT and DELETE.
	rr = doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"at": "top"}, bearerHdr(f.editorTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("unrestricted editor PUT: got %d %s, want 200", rr.Code, rr.Body.String())
	}
	rr = doRequestWithHeaders(f.srv, http.MethodDelete, parkPath, nil, bearerHdr(f.editorTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("unrestricted editor DELETE: got %d %s, want 200", rr.Code, rr.Body.String())
	}
}

func TestParkEndpoint_HiddenAnchorByteIdentical404(t *testing.T) {
	t.Parallel()
	f := newParkEndpointFixture(t)
	parkPath := "/api/v1/workspaces/" + f.ws.Slug + "/agent-roles/" + f.role.Slug + "/park"

	// Soft-delete an item in this workspace too.
	archived, err := f.srv.store.CreateItem(f.ws.ID, f.visibleColl.ID, models.ItemCreate{
		Title: "Archived", Fields: `{"status":"open"}`, AgentRoleID: &f.role.ID,
	})
	if err != nil {
		t.Fatalf("create archived: %v", err)
	}
	if err := f.srv.store.DeleteItem(archived.ID); err != nil {
		t.Fatalf("delete archived: %v", err)
	}

	missingRR := doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"before": "00000000-0000-0000-0000-000000000000"}, bearerHdr(f.ownerTok))
	if missingRR.Code != http.StatusNotFound {
		t.Fatalf("missing anchor: got %d %s, want 404", missingRR.Code, missingRR.Body.String())
	}
	var missingEnv struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	parseJSON(t, missingRR, &missingEnv)
	if missingEnv.Error.Code != "not_found" {
		t.Fatalf("missing anchor error code = %q, want not_found (body: %s)", missingEnv.Error.Code, missingRR.Body.String())
	}

	for _, tc := range []struct {
		name string
		ref  string
	}{
		{"other workspace item id", f.otherWSItem.ID},
		{"other workspace item ref", f.otherWSItem.Ref},
		{"archived item id", archived.ID},
		{"nonexistent ref", "TASK-99999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"before": tc.ref}, bearerHdr(f.ownerTok))
			if rr.Code != missingRR.Code || rr.Body.String() != missingRR.Body.String() {
				t.Fatalf("%s: got %d %s, want byte-identical %d %s", tc.name, rr.Code, rr.Body.String(), missingRR.Code, missingRR.Body.String())
			}
		})
	}
}

func TestParkEndpoint_AnchorNotInQueue422(t *testing.T) {
	t.Parallel()
	f := newParkEndpointFixture(t)
	parkPath := "/api/v1/workspaces/" + f.ws.Slug + "/agent-roles/" + f.role.Slug + "/park"

	otherRoleItem, err := f.srv.store.CreateItem(f.ws.ID, f.visibleColl.ID, models.ItemCreate{
		Title: "Other Role", Fields: `{"status":"open"}`, AgentRoleID: &f.otherRole.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	noRoleItem, err := f.srv.store.CreateItem(f.ws.ID, f.visibleColl.ID, models.ItemCreate{
		Title: "No Role", Fields: `{"status":"open"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	doneItem, err := f.srv.store.CreateItem(f.ws.ID, f.visibleColl.ID, models.ItemCreate{
		Title: "Done Item", Fields: `{"status":"done"}`, AgentRoleID: &f.role.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	inProgItem, err := f.srv.store.CreateItem(f.ws.ID, f.visibleColl.ID, models.ItemCreate{
		Title: "InProg Item", Fields: `{"status":"in_progress"}`, AgentRoleID: &f.role.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	refItem, err := f.srv.store.CreateItem(f.ws.ID, f.refColl.ID, models.ItemCreate{
		Title: "Doc Item", Fields: `{"status":"open"}`, AgentRoleID: &f.role.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	leasedItem, err := f.srv.store.CreateItem(f.ws.ID, f.visibleColl.ID, models.ItemCreate{
		Title: "Leased Item", Fields: `{"status":"open"}`, AgentRoleID: &f.role.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.ClaimItemLease(leasedItem.ID, "worker-a", "", f.owner.ID, 15*time.Minute); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		ref  string
	}{
		{"wrong role", otherRoleItem.Ref},
		{"no role", noRoleItem.Ref},
		{"terminal", doneItem.Ref},
		{"active status Now", inProgItem.Ref},
		{"live lease Now", leasedItem.Ref},
		{"reference collection", refItem.Ref},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"before": tc.ref}, bearerHdr(f.ownerTok))
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("%s: got %d %s, want 422", tc.name, rr.Code, rr.Body.String())
			}
			var env struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			parseJSON(t, rr, &env)
			if env.Error.Code != "park_anchor_not_in_queue" {
				t.Fatalf("%s: code = %q, want park_anchor_not_in_queue", tc.name, env.Error.Code)
			}
		})
	}

	// Also verify stale expected_park_seq -> 409 park_conflict with details.actual_park_seq.
	rr := doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"at": "top", "expected_park_seq": 99}, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusConflict {
		t.Fatalf("stale seq: got %d %s, want 409", rr.Code, rr.Body.String())
	}
	var conflict struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	parseJSON(t, rr, &conflict)
	if conflict.Error.Code != "park_conflict" || conflict.Error.Details["actual_park_seq"] != float64(0) {
		t.Fatalf("unexpected conflict body: %s", rr.Body.String())
	}
}

func TestParkEndpoint_BadBody400(t *testing.T) {
	t.Parallel()
	f := newParkEndpointFixture(t)
	parkPath := "/api/v1/workspaces/" + f.ws.Slug + "/agent-roles/" + f.role.Slug + "/park"

	cases := []struct {
		name string
		body any
	}{
		{"empty object", map[string]any{}},
		{"invalid at value", map[string]any{"at": "bottom"}},
		{"at and before", map[string]any{"at": "top", "before": f.items[0].Ref}},
		{"before and after", map[string]any{"before": f.items[0].Ref, "after": f.items[1].Ref}},
		{"empty before", map[string]any{"before": ""}},
		{"whitespace after", map[string]any{"after": "   "}},
		{"unknown field", map[string]any{"at": "top", "bogus": true}},
		{"negative expected_park_seq", map[string]any{"at": "top", "expected_park_seq": -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := doRequestWithHeaders(f.srv, http.MethodPut, parkPath, tc.body, bearerHdr(f.ownerTok))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("%s: got %d %s, want 400", tc.name, rr.Code, rr.Body.String())
			}
			var env struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			parseJSON(t, rr, &env)
			if env.Error.Code != "validation_error" {
				t.Fatalf("%s: code = %q, want validation_error", tc.name, env.Error.Code)
			}
		})
	}
}

func TestParkEndpoint_PinnedTokenMatchesLaneReorder(t *testing.T) {
	t.Parallel()
	f := newParkEndpointFixture(t)

	// Token pinned to wsB (owner is a member of both ws and wsB).
	otherPinned, err := f.srv.store.CreateAPIToken(f.owner.ID, models.APITokenCreate{
		Name: "wsb-pin", WorkspaceID: f.wsB.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken wsB: %v", err)
	}

	parkPath := "/api/v1/workspaces/" + f.ws.Slug + "/agent-roles/" + f.role.Slug + "/park"
	laneReorderPath := "/api/v1/workspaces/" + f.ws.Slug + "/roles/board/lane-order"

	// Pinned token of ws MAY park and clear on ws.
	rr := doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"at": "top"}, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("own pinned token PUT park: got %d %s, want 200", rr.Code, rr.Body.String())
	}
	rr = doRequestWithHeaders(f.srv, http.MethodDelete, parkPath, nil, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("own pinned token DELETE park: got %d %s, want 200", rr.Code, rr.Body.String())
	}

	// Pinned token of wsB is refused on ws with the exact same status and body as lane reorder.
	reorderRefusal := doRequestWithHeaders(f.srv, http.MethodPut, laneReorderPath, []any{}, bearerHdr(otherPinned.Token))
	putParkRefusal := doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"at": "top"}, bearerHdr(otherPinned.Token))
	delParkRefusal := doRequestWithHeaders(f.srv, http.MethodDelete, parkPath, nil, bearerHdr(otherPinned.Token))

	if reorderRefusal.Code != http.StatusForbidden {
		t.Fatalf("control lane reorder refusal: got %d %s, want 403", reorderRefusal.Code, reorderRefusal.Body.String())
	}
	if putParkRefusal.Code != reorderRefusal.Code || putParkRefusal.Body.String() != reorderRefusal.Body.String() {
		t.Fatalf("PUT park pinned refusal mismatch:\n  park:    %d %s\n  reorder: %d %s",
			putParkRefusal.Code, putParkRefusal.Body.String(), reorderRefusal.Code, reorderRefusal.Body.String())
	}
	if delParkRefusal.Code != reorderRefusal.Code || delParkRefusal.Body.String() != reorderRefusal.Body.String() {
		t.Fatalf("DELETE park pinned refusal mismatch:\n  park:    %d %s\n  reorder: %d %s",
			delParkRefusal.Code, delParkRefusal.Body.String(), reorderRefusal.Code, reorderRefusal.Body.String())
	}
}

func TestAgentRolesList_ParkField(t *testing.T) {
	t.Parallel()
	f := newParkEndpointFixture(t)
	listPath := "/api/v1/workspaces/" + f.ws.Slug + "/agent-roles"
	getPath := listPath + "/" + f.role.Slug
	parkPath := getPath + "/park"

	// Before parking: "park" key must be absent on both list and get.
	rr := doRequestWithHeaders(f.srv, http.MethodGet, listPath, nil, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET list: %d %s", rr.Code, rr.Body.String())
	}
	var rawList []map[string]any
	parseJSON(t, rr, &rawList)
	for _, r := range rawList {
		if _, has := r["park"]; has {
			t.Fatalf("unparked role %v should omit park field, got %v", r["slug"], r["park"])
		}
	}
	rr = doRequestWithHeaders(f.srv, http.MethodGet, getPath, nil, bearerHdr(f.ownerTok))
	var rawOne map[string]any
	parseJSON(t, rr, &rawOne)
	if _, has := rawOne["park"]; has {
		t.Fatalf("unparked GET role should omit park field, got %v", rawOne["park"])
	}

	// Park before items[1] (Two).
	rr = doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"before": f.items[1].Ref}, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT park: %d %s", rr.Code, rr.Body.String())
	}
	var putBody struct {
		Park struct {
			Mode           string  `json:"mode"`
			SortOrder      *int    `json:"sort_order"`
			FirstParkedRef *string `json:"first_parked_ref"`
			SetBy          *string `json:"set_by"`
			SetAt          string  `json:"set_at"`
			ParkSeq        int     `json:"park_seq"`
		} `json:"park"`
	}
	parseJSON(t, rr, &putBody)
	if putBody.Park.Mode != "line" || putBody.Park.SortOrder == nil || *putBody.Park.SortOrder != 2048 {
		t.Fatalf("PUT park mode/sort_order: %+v", putBody.Park)
	}
	if putBody.Park.FirstParkedRef == nil || *putBody.Park.FirstParkedRef != f.items[1].Ref {
		t.Fatalf("PUT first_parked_ref = %v, want %q", putBody.Park.FirstParkedRef, f.items[1].Ref)
	}
	if putBody.Park.SetBy == nil || *putBody.Park.SetBy != f.owner.Name {
		t.Fatalf("PUT set_by = %v, want %q", putBody.Park.SetBy, f.owner.Name)
	}
	if putBody.Park.ParkSeq != 1 || putBody.Park.SetAt == "" {
		t.Fatalf("PUT park_seq/set_at: %+v", putBody.Park)
	}

	// Now GET list and GET single must include "park" on the parked role and omit it on otherRole.
	rr = doRequestWithHeaders(f.srv, http.MethodGet, listPath, nil, bearerHdr(f.ownerTok))
	parseJSON(t, rr, &rawList)
	for _, r := range rawList {
		if r["slug"] == f.role.Slug {
			p, ok := r["park"].(map[string]any)
			if !ok || p["mode"] != "line" || p["set_by"] != f.owner.Name || p["park_seq"] != float64(1) {
				t.Fatalf("parked role in list has unexpected park: %v", r["park"])
			}
		} else {
			if _, has := r["park"]; has {
				t.Fatalf("other unparked role should omit park: %v", r)
			}
		}
	}
	rr = doRequestWithHeaders(f.srv, http.MethodGet, getPath, nil, bearerHdr(f.ownerTok))
	rawOne = nil
	parseJSON(t, rr, &rawOne)
	if p, ok := rawOne["park"].(map[string]any); !ok || p["mode"] != "line" {
		t.Fatalf("GET parked role missing park: %v", rawOne)
	}

	// Clear park -> returns {"park":null,"park_seq":2} and GET omits "park" again.
	rr = doRequestWithHeaders(f.srv, http.MethodDelete, parkPath, nil, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE park: %d %s", rr.Code, rr.Body.String())
	}
	var delRaw map[string]any
	parseJSON(t, rr, &delRaw)
	if val, present := delRaw["park"]; !present || val != nil {
		t.Fatalf("DELETE response park = %v (present=%v), want null", val, present)
	}
	if delRaw["park_seq"] != float64(2) {
		t.Fatalf("DELETE park_seq = %v, want 2", delRaw["park_seq"])
	}
	rr = doRequestWithHeaders(f.srv, http.MethodGet, getPath, nil, bearerHdr(f.ownerTok))
	rawOne = nil
	parseJSON(t, rr, &rawOne)
	if _, has := rawOne["park"]; has {
		t.Fatalf("GET after clear should omit park, got %v", rawOne["park"])
	}
}

func TestClaim_ParkedRefused409_IgnoreParkOK(t *testing.T) {
	t.Parallel()
	f := newParkEndpointFixture(t)
	parkPath := "/api/v1/workspaces/" + f.ws.Slug + "/agent-roles/" + f.role.Slug + "/park"

	// Park before items[1] (Two @ 2048). One (1024) is above; Two (2048) & Three (3072) are parked.
	rr := doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"before": f.items[1].Ref}, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT park: %d %s", rr.Code, rr.Body.String())
	}

	// Claiming One (above park line) succeeds.
	claimOne := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.items[0].Slug + "/claim"
	rr = doRequestWithHeaders(f.srv, http.MethodPost, claimOne, map[string]any{"holder": "agent-a"}, bearerHdr(f.editorTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("claim above line: got %d %s, want 200", rr.Code, rr.Body.String())
	}

	// Claiming Three (below park line) is refused 409 item_parked with details.
	claimThree := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.items[2].Slug + "/claim"
	rr = doRequestWithHeaders(f.srv, http.MethodPost, claimThree, map[string]any{"holder": "agent-a"}, bearerHdr(f.editorTok))
	if rr.Code != http.StatusConflict {
		t.Fatalf("claim parked: got %d %s, want 409", rr.Code, rr.Body.String())
	}
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	parseJSON(t, rr, &env)
	if env.Error.Code != "item_parked" {
		t.Fatalf("code = %q, want item_parked", env.Error.Code)
	}
	if env.Error.Details["role"] != f.role.Slug {
		t.Errorf("details.role = %v, want %q", env.Error.Details["role"], f.role.Slug)
	}
	if env.Error.Details["first_parked_ref"] != f.items[1].Ref {
		t.Errorf("details.first_parked_ref = %v, want %q", env.Error.Details["first_parked_ref"], f.items[1].Ref)
	}
	if env.Error.Details["set_by"] != f.owner.Name {
		t.Errorf("details.set_by = %v, want %q", env.Error.Details["set_by"], f.owner.Name)
	}

	// Malformed ignore_park query value is refused 400.
	rr = doRequestWithHeaders(f.srv, http.MethodPost, claimThree+"?ignore_park=yes", map[string]any{"holder": "agent-a"}, bearerHdr(f.editorTok))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad ignore_park: got %d %s, want 400", rr.Code, rr.Body.String())
	}

	// Any caller who may claim (including restricted editor!) may pass ?ignore_park=true.
	rr = doRequestWithHeaders(f.srv, http.MethodPost, claimThree+"?ignore_park=true", map[string]any{"holder": "agent-restricted"}, bearerHdr(f.restrictedTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("restricted editor claim with ignore_park=true: got %d %s, want 200", rr.Code, rr.Body.String())
	}
}

func TestClaim_ParkedBodyUsesVisibleSetOnly(t *testing.T) {
	t.Parallel()
	f := newParkEndpointFixture(t)
	parkPath := "/api/v1/workspaces/" + f.ws.Slug + "/agent-roles/" + f.role.Slug + "/park"

	// hiddenItem has role_sort_order=512 (FIRST in the lane), while visible items are 1024, 2048, 3072.
	// Park at top so hiddenItem is the workspace-wide first parked item.
	rr := doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"at": "top"}, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT park top: %d %s", rr.Code, rr.Body.String())
	}

	// Restricted editor (who sees visibleColl, NOT hiddenColl) tries to claim items[1].
	// The 409 details.first_parked_ref MUST be items[0].Ref (first VISIBLE parked item), NOT hiddenItem.Ref!
	claimPath := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.items[1].Slug + "/claim"
	rr = doRequestWithHeaders(f.srv, http.MethodPost, claimPath, map[string]any{"holder": "agent-r"}, bearerHdr(f.restrictedTok))
	if rr.Code != http.StatusConflict {
		t.Fatalf("restricted claim parked: got %d %s, want 409", rr.Code, rr.Body.String())
	}
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	parseJSON(t, rr, &env)
	if env.Error.Code != "item_parked" {
		t.Fatalf("code = %q, want item_parked", env.Error.Code)
	}
	if env.Error.Details["first_parked_ref"] == f.hiddenItem.Ref {
		t.Fatalf("LEAK: first_parked_ref exposed hidden item %q", f.hiddenItem.Ref)
	}
	if env.Error.Details["first_parked_ref"] != f.items[0].Ref {
		t.Fatalf("first_parked_ref = %v, want %q (first visible parked item)", env.Error.Details["first_parked_ref"], f.items[0].Ref)
	}

	// Control: unrestricted owner gets hiddenItem.Ref as first_parked_ref.
	rr = doRequestWithHeaders(f.srv, http.MethodPost, claimPath, map[string]any{"holder": "agent-o"}, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusConflict {
		t.Fatalf("owner claim parked: got %d %s, want 409", rr.Code, rr.Body.String())
	}
	var ownerEnv struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &ownerEnv); err != nil {
		t.Fatal(err)
	}
	if ownerEnv.Error.Details["first_parked_ref"] != f.hiddenItem.Ref {
		t.Fatalf("owner first_parked_ref = %v, want %q", ownerEnv.Error.Details["first_parked_ref"], f.hiddenItem.Ref)
	}
}

func TestClaim_OwnLeaseAndNowNeverParked(t *testing.T) {
	t.Parallel()
	f := newParkEndpointFixture(t)
	parkPath := "/api/v1/workspaces/" + f.ws.Slug + "/agent-roles/" + f.role.Slug + "/park"

	// 1. Editor claims items[0] before parking.
	claim0 := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.items[0].Slug + "/claim"
	rr := doRequestWithHeaders(f.srv, http.MethodPost, claim0, map[string]any{"holder": "agent-ed"}, bearerHdr(f.editorTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("initial claim: %d %s", rr.Code, rr.Body.String())
	}

	// 2. Mark items[1] as in_progress (Now item).
	inProg := `{"status":"in_progress"}`
	if _, err := f.srv.store.UpdateItem(f.items[1].ID, models.ItemUpdate{Fields: &inProg}); err != nil {
		t.Fatalf("update items[1] in_progress: %v", err)
	}

	// 3. Park the role at top.
	rr = doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"at": "top"}, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT park top: %d %s", rr.Code, rr.Body.String())
	}

	// 4. Editor refreshing their OWN live lease on items[0] succeeds (never refused as parked).
	rr = doRequestWithHeaders(f.srv, http.MethodPost, claim0, map[string]any{"holder": "agent-ed"}, bearerHdr(f.editorTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("own lease refresh under park: got %d %s, want 200", rr.Code, rr.Body.String())
	}

	// 5. Owner attempting to claim items[0] (held by editor -> Now item) gets 409 lease_held, NOT item_parked.
	rr = doRequestWithHeaders(f.srv, http.MethodPost, claim0, map[string]any{"holder": "agent-owner"}, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusConflict {
		t.Fatalf("foreign lease claim under park: got %d %s, want 409", rr.Code, rr.Body.String())
	}
	var heldEnv struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	parseJSON(t, rr, &heldEnv)
	if heldEnv.Error.Code != "lease_held" {
		t.Fatalf("foreign lease under park: code = %q, want lease_held", heldEnv.Error.Code)
	}

	// 6. Claiming items[1] (active status in_progress -> Now item) succeeds without ignore_park.
	claim1 := "/api/v1/workspaces/" + f.ws.Slug + "/items/" + f.items[1].Slug + "/claim"
	rr = doRequestWithHeaders(f.srv, http.MethodPost, claim1, map[string]any{"holder": "agent-ed"}, bearerHdr(f.editorTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("claim Now item under park: got %d %s, want 200", rr.Code, rr.Body.String())
	}
}
