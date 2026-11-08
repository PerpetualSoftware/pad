package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

const groupsPath = "/api/v1/me/workspace-groups"

type groupWire struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	WorkspaceIDs []string `json:"workspace_ids"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
}

func groupRequest(s *Server, u *models.User, method, path, body string, mods ...func(context.Context) context.Context) *httptest.ResponseRecorder {
	const testCSRF = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.AddCookie(&http.Cookie{Name: csrfCookieName(s.secureCookies), Value: testCSRF})
	r.Header.Set("X-CSRF-Token", testCSRF)
	ctx := r.Context()
	if u != nil {
		ctx = WithAPITokenAuth(WithCurrentUser(ctx, u))
	}
	for _, mod := range mods {
		ctx = mod(ctx)
	}
	rr := httptest.NewRecorder()
	s.ensureRouter()
	s.router.ServeHTTP(rr, r.WithContext(ctx))
	return rr
}
func groupStatus(t *testing.T, rr *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rr.Code != want {
		t.Fatalf("status %d, want %d: %s", rr.Code, want, rr.Body.String())
	}
}
func groupDecode(t *testing.T, rr *httptest.ResponseRecorder) groupWire {
	t.Helper()
	var g groupWire
	if err := json.Unmarshal(rr.Body.Bytes(), &g); err != nil {
		t.Fatal(err)
	}
	return g
}
func groupCreate(t *testing.T, s *Server, u *models.User, name string, ids ...string) groupWire {
	t.Helper()
	if ids == nil {
		ids = []string{}
	}
	b, _ := json.Marshal(map[string]any{"name": name, "workspace_ids": ids})
	rr := groupRequest(s, u, "POST", groupsPath, string(b))
	groupStatus(t, rr, 201)
	return groupDecode(t, rr)
}
func groupList(t *testing.T, s *Server, u *models.User, mods ...func(context.Context) context.Context) []groupWire {
	t.Helper()
	rr := groupRequest(s, u, "GET", groupsPath, "", mods...)
	groupStatus(t, rr, 200)
	var body struct {
		Groups []groupWire `json:"groups"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Groups == nil {
		t.Fatal("groups must be an array")
	}
	return body.Groups
}
func groupNoLeak(t *testing.T, rr *httptest.ResponseRecorder, ws *models.Workspace) {
	t.Helper()
	for _, secret := range []string{ws.ID, ws.Name, ws.Slug} {
		if strings.Contains(rr.Body.String(), secret) {
			t.Fatalf("hidden workspace leaked: %s", rr.Body.String())
		}
	}
}
func groupError(t *testing.T, rr *httptest.ResponseRecorder, status int, code, field, detail string) {
	t.Helper()
	groupStatus(t, rr, status)
	var b struct {
		Error struct {
			Code    string            `json:"code"`
			Details map[string]string `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if b.Error.Code != code || b.Error.Details["field"] != field || b.Error.Details["code"] != detail {
		t.Fatalf("error: %s", rr.Body.String())
	}
}

func TestWorkspaceGroupsLostAccessPrivacy(t *testing.T) {
	t.Parallel()
	for _, loss := range []string{"membership", "guest", "soft-delete"} {
		t.Run(loss, func(t *testing.T) {
			w := newAgendaWorld(t, "tasks")
			u := mustUser(t, w.srv, "group-reader@example.com", "groupreader", "")
			hidden := mustWorkspace(t, w.srv, "Planted Hidden Sentinel", w.owner.ID)
			if loss == "guest" {
				if _, err := w.srv.store.CreateCollectionGrant(w.ws.ID, w.colls["tasks"].ID, u.ID, "view", w.owner.ID); err != nil {
					t.Fatal(err)
				}
			} else if err := w.srv.store.AddWorkspaceMember(w.ws.ID, u.ID, "viewer"); err != nil {
				t.Fatal(err)
			}
			g := groupCreate(t, w.srv, u, "Saved", w.ws.ID)
			if len(g.WorkspaceIDs) != 1 {
				t.Fatalf("admitted member missing: %+v", g)
			}
			var err error
			switch loss {
			case "membership":
				err = w.srv.store.RemoveWorkspaceMember(w.ws.ID, u.ID)
			case "guest":
				err = w.srv.store.RevokeAllUserGrants(w.ws.ID, u.ID)
			case "soft-delete":
				err = w.srv.store.DeleteWorkspace(w.ws.Slug)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, req := range []struct {
				method, path, body string
				status             int
			}{
				{"GET", groupsPath, "", 200}, {"GET", groupsPath + "/" + g.ID, "", 200},
				{"PATCH", groupsPath + "/" + g.ID, `{"name":"Renamed"}`, 200},
				{"POST", groupsPath, `{"name":"Empty","workspace_ids":[]}`, 201},
				{"GET", "/api/v1/auth/export", "", 200},
			} {
				rr := groupRequest(w.srv, u, req.method, req.path, req.body)
				groupStatus(t, rr, req.status)
				groupNoLeak(t, rr, w.ws)
				groupNoLeak(t, rr, hidden)
			}
			rr := groupRequest(w.srv, u, "GET", groupsPath+"/"+g.ID, "")
			if got := groupDecode(t, rr); got.WorkspaceIDs == nil || len(got.WorkspaceIDs) != 0 {
				t.Fatalf("empty group missing: %+v", got)
			}
		})
	}
}

func TestWorkspaceGroupsOwnerIsolation(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t)
	b := mustUser(t, w.srv, "group-b@example.com", "groupb", "")
	g := groupCreate(t, w.srv, w.owner, "Private", w.ws.ID)
	before := groupRequest(w.srv, w.owner, "GET", groupsPath+"/"+g.ID, "")
	for _, method := range []string{"GET", "PATCH", "DELETE"} {
		a := groupRequest(w.srv, b, method, groupsPath+"/"+g.ID, `{"name":"Changed"}`)
		z := groupRequest(w.srv, b, method, groupsPath+"/00000000-0000-4000-8000-000000000001", `{"name":"Changed"}`)
		groupStatus(t, a, 404)
		groupStatus(t, z, 404)
		if !bytes.Equal(a.Body.Bytes(), z.Body.Bytes()) {
			t.Fatalf("owner oracle: %s / %s", a.Body, z.Body)
		}
	}
	after := groupRequest(w.srv, w.owner, "GET", groupsPath+"/"+g.ID, "")
	if !bytes.Equal(before.Body.Bytes(), after.Body.Bytes()) {
		t.Fatal("other user changed group")
	}
	if got := groupList(t, w.srv, b); len(got) != 2 {
		t.Fatalf("other user groups: %+v", got)
	}
	groupCreate(t, w.srv, b, "Private")
	groupStatus(t, groupRequest(w.srv, w.owner, "PATCH", groupsPath+"/"+g.ID, `{"name":"Changed"}`), 200)
	if got := groupList(t, w.srv, b); len(got) != 3 || got[2].Name != "Private" {
		t.Fatalf("shared state: %+v", got)
	}
	rr := groupRequest(w.srv, w.owner, "DELETE", groupsPath+"/"+g.ID, "")
	groupStatus(t, rr, 200)
	var deleted struct {
		ID      string `json:"id"`
		Deleted bool   `json:"deleted"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &deleted); err != nil {
		t.Fatal(err)
	}
	if deleted.ID != g.ID || !deleted.Deleted {
		t.Fatalf("delete response: %s", rr.Body.String())
	}
	groupStatus(t, groupRequest(w.srv, w.owner, "GET", groupsPath+"/"+g.ID, ""), 404)
}

func TestWorkspaceGroupsPrincipals(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t)
	app := func(c context.Context) context.Context { return context.WithValue(c, appCtxKey{}, &appContext{}) }
	want := newNfx(t, w.srv).get(w.owner, "", app)
	for _, req := range []struct{ method, path string }{{"GET", groupsPath}, {"POST", groupsPath}, {"GET", groupsPath + "/all"}, {"PATCH", groupsPath + "/all"}, {"DELETE", groupsPath + "/all"}} {
		rr := groupRequest(w.srv, w.owner, req.method, req.path, "null", app)
		groupStatus(t, rr, 404)
		if !bytes.Equal(rr.Body.Bytes(), want.Body.Bytes()) {
			t.Fatalf("app refusal: %s vs %s", rr.Body, want.Body)
		}
		groupStatus(t, groupRequest(w.srv, nil, req.method, req.path, "null"), 401)
	}
	groupList(t, w.srv, w.owner)
}

func TestWorkspaceGroupsSaveRefusalIsPrivateAndAtomic(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t)
	u := mustUser(t, w.srv, "save-group@example.com", "savegroup", "")
	g := groupCreate(t, w.srv, u, "Original")
	for _, method := range []string{"POST", "PATCH"} {
		path := groupsPath
		if method == "PATCH" {
			path += "/" + g.ID
		}
		responses := []*httptest.ResponseRecorder{}
		for _, id := range []string{w.ws.ID, "00000000-0000-4000-8000-000000000001"} {
			rr := groupRequest(w.srv, u, method, path, fmt.Sprintf(`{"name":"Changed","workspace_ids":[%q]}`, id))
			groupError(t, rr, 400, "validation_error", "workspace_ids", "unknown_workspace")
			groupNoLeak(t, rr, w.ws)
			responses = append(responses, rr)
		}
		if !bytes.Equal(responses[0].Body.Bytes(), responses[1].Body.Bytes()) {
			t.Fatal("save existence oracle")
		}
		if got := groupList(t, w.srv, u); len(got) != 3 || got[2].Name != "Original" || len(got[2].WorkspaceIDs) != 0 {
			t.Fatalf("partial write: %+v", got)
		}
	}
}

func TestWorkspaceGroupsTokenAdmission(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t)
	b := mustWorkspace(t, w.srv, "Excluded Workspace Sentinel", w.owner.ID)
	g := groupCreate(t, w.srv, w.owner, "Both", w.ws.ID, b.ID)
	for _, mode := range []string{"allowlist"} {
		t.Run(mode, func(t *testing.T) {
			mod := func(c context.Context) context.Context {
				return WithTokenAllowedWorkspaces(c, []string{w.ws.Slug})
			}
			for _, entry := range groupList(t, w.srv, w.owner, mod) {
				if len(entry.WorkspaceIDs) != 1 || entry.WorkspaceIDs[0] != w.ws.ID {
					t.Fatalf("scope: %+v", entry)
				}
			}
			for _, id := range []string{"all", "mine", g.ID} {
				rr := groupRequest(w.srv, w.owner, "GET", groupsPath+"/"+id, "", mod)
				groupStatus(t, rr, 200)
				groupNoLeak(t, rr, b)
			}
			for _, method := range []string{"POST", "PATCH"} {
				path := groupsPath
				if method == "PATCH" {
					path += "/" + g.ID
				}
				rr := groupRequest(w.srv, w.owner, method, path, fmt.Sprintf(`{"name":"Scoped","workspace_ids":[%q]}`, b.ID), mod)
				groupError(t, rr, 400, "validation_error", "workspace_ids", "unknown_workspace")
				groupNoLeak(t, rr, b)
				rr = groupRequest(w.srv, w.owner, method, path, fmt.Sprintf(`{"name":%q,"workspace_ids":[%q]}`, mode+method, w.ws.ID), mod)
				status := 201
				if method == "PATCH" {
					status = 200
				}
				groupStatus(t, rr, status)
				groupNoLeak(t, rr, b)
			}
		})
	}
}

func TestWorkspaceGroupsBuiltinsAndOrdering(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t)
	other := mustUser(t, w.srv, "other-group@example.com", "othergroup", "")
	before := groupList(t, w.srv, w.owner)
	if len(before) != 2 || before[0].ID != "all" || before[0].Name != "All workspaces" || before[1].ID != "mine" || before[1].Name != "My workspaces" {
		t.Fatalf("builtins: %+v", before)
	}
	b := mustWorkspace(t, w.srv, "A workspace", other.ID)
	if err := w.srv.store.AddWorkspaceMember(b.ID, w.owner.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	groupCreate(t, w.srv, w.owner, "zeta", w.ws.ID, b.ID)
	groupCreate(t, w.srv, w.owner, "Alpha")
	list := groupList(t, w.srv, w.owner)
	if len(list[0].WorkspaceIDs) != 2 || list[0].WorkspaceIDs[0] != b.ID || len(list[1].WorkspaceIDs) != 1 || list[1].WorkspaceIDs[0] != w.ws.ID || list[2].Name != "Alpha" || list[3].Name != "zeta" {
		t.Fatalf("computed/order: %+v", list)
	}
	if list[0].CreatedAt != "" || list[2].CreatedAt == "" || list[2].UpdatedAt == "" {
		t.Fatalf("timestamps: %+v", list)
	}
	for _, id := range []string{"all", "mine"} {
		for _, method := range []string{"PATCH", "DELETE"} {
			groupError(t, groupRequest(w.srv, w.owner, method, groupsPath+"/"+id, `{}`), 400, "validation_error", "id", "builtin_group")
		}
	}
	for _, name := range []string{"all", "ALL", "Mine", "mInE"} {
		groupError(t, groupRequest(w.srv, w.owner, "POST", groupsPath, fmt.Sprintf(`{"name":%q,"workspace_ids":[]}`, name)), 400, "validation_error", "name", "reserved")
	}
}

func TestWorkspaceGroupsStrictBodiesAndReplacement(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t)
	g := groupCreate(t, w.srv, w.owner, "Original", w.ws.ID)
	for _, method := range []string{"POST", "PATCH"} {
		path := groupsPath
		if method == "PATCH" {
			path += "/" + g.ID
		}
		for _, body := range []string{`null`, `[]`, `42`, `"str"`, `{"unknown":1}`, `{"name":"a","name":"b"}`, `{"name":3}`, `{"workspace_ids":"bad"}`, `{"workspace_ids":[1]}`, `{"workspace_ids":[null]}`, `{"name":"a\u0000b"}`, `{"workspace_ids":["\u0000"]}`, `{"name":"ok"} {}`} {
			t.Run(method+body, func(t *testing.T) {
				groupError(t, groupRequest(w.srv, w.owner, method, path, body), 400, "invalid_body", "", "")
			})
		}
		for _, field := range []string{"name", "workspace_ids"} {
			body := map[string]any{"name": "Original", "workspace_ids": []string{}}
			body[field] = nil
			raw, _ := json.Marshal(body)
			groupError(t, groupRequest(w.srv, w.owner, method, path, string(raw)), 400, "validation_error", field, "required")
		}
	}
	before := groupRequest(w.srv, w.owner, "GET", groupsPath+"/"+g.ID, "")
	noop := groupRequest(w.srv, w.owner, "PATCH", groupsPath+"/"+g.ID, `{}`)
	groupStatus(t, noop, 200)
	if !bytes.Equal(before.Body.Bytes(), noop.Body.Bytes()) {
		t.Fatal("no-op changed group")
	}
	rr := groupRequest(w.srv, w.owner, "PATCH", groupsPath+"/"+g.ID, fmt.Sprintf(`{"workspace_ids":[%q,%q]}`, w.ws.ID, w.ws.ID))
	groupStatus(t, rr, 200)
	if len(groupDecode(t, rr).WorkspaceIDs) != 1 {
		t.Fatal("duplicates retained")
	}
	rr = groupRequest(w.srv, w.owner, "PATCH", groupsPath+"/"+g.ID, `{"workspace_ids":[]}`)
	groupStatus(t, rr, 200)
	if got := groupDecode(t, rr); got.WorkspaceIDs == nil || len(got.WorkspaceIDs) != 0 {
		t.Fatal("members not replaced")
	}
}

func TestWorkspaceGroupsLimitsAndNames(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t)
	for _, name := range []string{"", "   ", strings.Repeat("é", 61), "bad\nname"} {
		groupError(t, groupRequest(w.srv, w.owner, "POST", groupsPath, fmt.Sprintf(`{"name":%q,"workspace_ids":[]}`, name)), 400, "validation_error", "name", "range")
	}
	g := groupCreate(t, w.srv, w.owner, "  Mixed  ")
	if g.Name != "Mixed" {
		t.Fatalf("untrimmed: %q", g.Name)
	}
	groupError(t, groupRequest(w.srv, w.owner, "POST", groupsPath, `{"name":"mIXED","workspace_ids":[]}`), 409, "conflict", "name", "name_taken")
	groupStatus(t, groupRequest(w.srv, w.owner, "PATCH", groupsPath+"/"+g.ID, `{"name":"MIXED"}`), 200)
	groupStatus(t, groupRequest(w.srv, w.owner, "PATCH", groupsPath+"/"+g.ID, `{"name":"MIXED"}`), 200)
	groupCreate(t, w.srv, w.owner, strings.Repeat("é", 60))
	var ids []string
	for i := 0; i < 101; i++ {
		ids = append(ids, fmt.Sprintf("00000000-0000-4000-8000-%012d", i))
	}
	body, _ := json.Marshal(map[string]any{"name": "Too many", "workspace_ids": ids})
	groupError(t, groupRequest(w.srv, w.owner, "POST", groupsPath, string(body)), 400, "validation_error", "workspace_ids", "range")
	for i := 2; i < 20; i++ {
		groupCreate(t, w.srv, w.owner, fmt.Sprintf("Group %02d", i))
	}
	groupError(t, groupRequest(w.srv, w.owner, "POST", groupsPath, `{"name":"Overflow","workspace_ids":[]}`), 400, "validation_error", "groups", "range")
}

func TestWorkspaceGroupsConcurrentCap(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t)
	w.srv.ensureRouter()
	var wg sync.WaitGroup
	codes := make(chan int, 32)
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rr := groupRequest(w.srv, w.owner, "POST", groupsPath, fmt.Sprintf(`{"name":"Concurrent %d","workspace_ids":[]}`, i))
			codes <- rr.Code
		}(i)
	}
	close(start)
	wg.Wait()
	close(codes)
	successes := 0
	for code := range codes {
		if code == 201 {
			successes++
		} else if code != 400 {
			t.Errorf("unexpected status: %d", code)
		}
	}
	if successes != 20 || len(groupList(t, w.srv, w.owner)) != 22 {
		t.Fatalf("successful creates: %d", successes)
	}
}

func TestWorkspaceGroupsExport(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t)
	empty := groupRequest(w.srv, w.owner, "GET", "/api/v1/auth/export", "")
	groupStatus(t, empty, 200)
	var e map[string]json.RawMessage
	if err := json.Unmarshal(empty.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if string(e["workspace_groups"]) != "[]" {
		t.Fatalf("empty groups: %s", empty.Body.String())
	}
	u := mustUser(t, w.srv, "export-group@example.com", "exportgroup", "")
	hidden := mustWorkspace(t, w.srv, "Hidden Export Sentinel", w.owner.ID)
	if err := w.srv.store.AddWorkspaceMember(w.ws.ID, u.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	g := groupCreate(t, w.srv, u, "Exported", w.ws.ID)
	other := groupCreate(t, w.srv, w.owner, "Other private group", hidden.ID)
	if err := w.srv.store.RemoveWorkspaceMember(w.ws.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	rr := groupRequest(w.srv, u, "GET", "/api/v1/auth/export", "")
	groupStatus(t, rr, 200)
	groupNoLeak(t, rr, w.ws)
	groupNoLeak(t, rr, hidden)
	if strings.Contains(rr.Body.String(), other.ID) || strings.Contains(rr.Body.String(), other.Name) {
		t.Fatal("other user exported")
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	var groups []groupWire
	if err := json.Unmarshal(e["workspace_groups"], &groups); err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].ID != g.ID || groups[0].Kind != "" || groups[0].WorkspaceIDs == nil || len(groups[0].WorkspaceIDs) != 0 {
		t.Fatalf("exported groups: %+v", groups)
	}
	if string(e["version"]) != "1" || string(e["complete"]) != "true" {
		t.Fatal("export contract changed")
	}
	text := rr.Body.String()
	if strings.Index(text, `"user"`) > strings.Index(text, `"workspace_groups"`) || strings.Index(text, `"workspace_groups"`) > strings.Index(text, `"workspaces"`) {
		t.Fatal("export order")
	}
}

func TestWorkspaceGroupsExportReadFailureBeforeBody(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t)
	if _, err := w.srv.store.DB().Exec(`DROP TABLE IF EXISTS user_workspace_groups`); err != nil {
		t.Fatal(err)
	}
	rr := groupRequest(w.srv, w.owner, "GET", "/api/v1/auth/export", "")
	groupStatus(t, rr, 500)
	if rr.Header().Get("Content-Disposition") != "" || strings.Contains(rr.Body.String(), `"format"`) {
		t.Fatalf("stream started: %s", rr.Body.String())
	}
}

// A member the caller cannot see right now (access lost, maybe regained
// later) must survive an edit made while it was hidden.
func TestWorkspaceGroupsPatchKeepsHiddenMembers(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	u := mustUser(t, w.srv, "group-keeper@example.com", "groupkeeper", "")
	other := mustWorkspace(t, w.srv, "Second Space", w.owner.ID)
	for _, ws := range []*models.Workspace{w.ws, other} {
		if err := w.srv.store.AddWorkspaceMember(ws.ID, u.ID, "viewer"); err != nil {
			t.Fatal(err)
		}
	}
	g := groupCreate(t, w.srv, u, "Both", w.ws.ID, other.ID)
	if len(g.WorkspaceIDs) != 2 {
		t.Fatalf("both members expected: %+v", g)
	}
	if err := w.srv.store.RemoveWorkspaceMember(other.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	rr := groupRequest(w.srv, u, "PATCH", groupsPath+"/"+g.ID, `{"workspace_ids":["`+w.ws.ID+`"]}`)
	groupStatus(t, rr, 200)
	groupNoLeak(t, rr, other)
	if err := w.srv.store.AddWorkspaceMember(other.ID, u.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	got := groupDecode(t, groupRequest(w.srv, u, "GET", groupsPath+"/"+g.ID, ""))
	if len(got.WorkspaceIDs) != 2 {
		t.Fatalf("hidden member was dropped by the edit: %+v", got)
	}
}

// Named groups list in one order on both backends: lower-cased by Go, then id.
func TestWorkspaceGroupsNamedOrderIsDialectIndependent(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	u := mustUser(t, w.srv, "group-order@example.com", "grouporder", "")
	for _, name := range []string{"Éz", "éa", "Zed", "alpha"} {
		groupCreate(t, w.srv, u, name)
	}
	var names []string
	for _, g := range groupList(t, w.srv, u) {
		if g.Kind == "named" {
			names = append(names, g.Name)
		}
	}
	want := []string{"alpha", "Zed", "éa", "Éz"}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Fatalf("order %q, want %q", names, want)
	}
}

// A token pinned to one workspace is confined to it: every group route answers
// 403 token_workspace_pinned from the gate, and a group the token's user owns
// is neither read, renamed nor deleted through it.
func TestWorkspaceGroupsPinnedTokenRefused(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t)
	g := groupCreate(t, w.srv, w.owner, "Private Group Sentinel", w.ws.ID)
	pin := func(c context.Context) context.Context {
		ctx, err := w.srv.withTokenPin(WithTokenWorkspaceID(c, w.ws.ID), &models.APIToken{UserID: w.owner.ID, WorkspaceID: w.ws.ID})
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	for _, req := range []struct{ method, path, body string }{
		{"GET", groupsPath, ""},
		{"POST", groupsPath, `{"name":"Pinned","workspace_ids":[]}`},
		{"GET", groupsPath + "/" + g.ID, ""},
		{"PATCH", groupsPath + "/" + g.ID, `{"name":"Renamed"}`},
		{"DELETE", groupsPath + "/" + g.ID, ""},
	} {
		rr := groupRequest(w.srv, w.owner, req.method, req.path, req.body, pin)
		if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "token_workspace_pinned") {
			t.Fatalf("%s %s: %d %s, want 403 token_workspace_pinned", req.method, req.path, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "Private Group Sentinel") {
			t.Fatalf("%s %s leaked the group name: %s", req.method, req.path, rr.Body.String())
		}
	}
	groups := groupList(t, w.srv, w.owner)
	if len(groups) != 3 || groups[2].ID != g.ID || groups[2].Name != "Private Group Sentinel" {
		t.Fatalf("the group changed or a pinned create landed: %+v", groups)
	}
}

// A UUID is the same id in any letter case; the stored member is the canonical
// lower-case spelling.
func TestWorkspaceGroupsAcceptsUppercaseUUID(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	u := mustUser(t, w.srv, "group-upper@example.com", "groupupper", "")
	if err := w.srv.store.AddWorkspaceMember(w.ws.ID, u.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	rr := groupRequest(w.srv, u, "POST", groupsPath, `{"name":"Upper","workspace_ids":["`+strings.ToUpper(w.ws.ID)+`"]}`)
	groupStatus(t, rr, 201)
	got := groupDecode(t, rr)
	if len(got.WorkspaceIDs) != 1 || got.WorkspaceIDs[0] != w.ws.ID {
		t.Fatalf("members: %+v, want [%s]", got.WorkspaceIDs, w.ws.ID)
	}
}
