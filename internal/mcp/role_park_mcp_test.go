package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TestRoleParkErrors_MappedIdenticallyOnStdioAndHTTP verifies that the three
// role-park error codes map identically on stdio (structured CLI marker) and
// HTTP (classifyHTTPStatus).
func TestRoleParkErrors_MappedIdenticallyOnStdioAndHTTP(t *testing.T) {
	cases := []struct {
		code    string
		want    ErrorCode
		status  int
		message string
		details json.RawMessage
	}{
		{
			code:    "item_parked",
			want:    ErrItemParked,
			status:  http.StatusConflict,
			message: `Role "implementer" is parked; pass ignore_park=true to claim anyway`,
			details: json.RawMessage(`{"role":"implementer","first_parked_ref":"TASK-2","set_by":"Alice"}`),
		},
		{
			code:    "park_conflict",
			want:    ErrParkConflict,
			status:  http.StatusConflict,
			message: "Role park state has changed; retry with the current park_seq",
			details: json.RawMessage(`{"actual_park_seq":3}`),
		},
		{
			code:    "park_anchor_not_in_queue",
			want:    ErrParkAnchorNotInQueue,
			status:  http.StatusUnprocessableEntity,
			message: "Anchor item is not in this role lane's queue",
		},
	}

	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			// Stdio path: CLI writes structured marker -> extractStructuredCLIError lifts it.
			var stderr bytes.Buffer
			cli.WriteRoleParkError(&stderr, &cli.APIError{
				Code:    tc.code,
				Message: tc.message,
				Status:  tc.status,
				Details: tc.details,
			})
			stdioRes := extractStructuredCLIError(stderr.String())
			if stdioRes == nil {
				t.Fatalf("stdio did not lift %s marker: %q", tc.code, stderr.String())
			}
			stdioEnv := decodeEnvelope(t, stdioRes)

			// HTTP path: server JSON body -> classifyHTTPStatus lifts it.
			bodyMap := map[string]any{
				"code":    tc.code,
				"message": tc.message,
			}
			if len(tc.details) > 0 {
				bodyMap["details"] = tc.details
			}
			rawBody, _ := json.Marshal(map[string]any{"error": bodyMap})
			httpRes := classifyHTTPStatus(context.Background(), "role park", tc.status, rawBody, nil)
			httpEnv := decodeEnvelope(t, httpRes)

			if stdioEnv.Error.Code != tc.want {
				t.Errorf("stdio code = %q, want %q", stdioEnv.Error.Code, tc.want)
			}
			if httpEnv.Error.Code != tc.want {
				t.Errorf("http code = %q, want %q", httpEnv.Error.Code, tc.want)
			}
			if stdioEnv.Error.Hint == "" || stdioEnv.Error.Hint != httpEnv.Error.Hint {
				t.Errorf("hints differ or empty:\n  stdio: %q\n  http:  %q", stdioEnv.Error.Hint, httpEnv.Error.Hint)
			}
			if string(stdioEnv.Error.Details) != string(httpEnv.Error.Details) {
				t.Errorf("details differ:\n  stdio: %s\n  http:  %s", stdioEnv.Error.Details, httpEnv.Error.Details)
			}
		})
	}
}

// TestCatalogRole_ParkUnparkArgsReachCLIAndHTTP verifies pad_role park and
// unpark forward slug, before, after, and expected_park_seq cleanly on both
// stdio (BuildCLIArgs) and HTTP (routeTable).
func TestCatalogRole_ParkUnparkArgsReachCLIAndHTTP(t *testing.T) {
	def := findCatalogTool(t, "pad_role")
	for _, action := range []string{"park", "unpark"} {
		if _, ok := def.Actions[action]; !ok {
			t.Fatalf("pad_role missing action %q", action)
		}
	}

	// Stdio dispatch check for park with before + expected_park_seq.
	d := &fakeDispatcher{}
	env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: d}
	res, err := makeFanOutHandler(def, env)(context.Background(), callToolRequest(map[string]any{
		"action":            "park",
		"slug":              "implementer",
		"before":            "TASK-5",
		"expected_park_seq": float64(2),
	}))
	if err != nil || (res != nil && res.IsError) {
		t.Fatalf("stdio pad_role park failed: err=%v res=%s", err, textOf(res))
	}
	if strings.Join(d.gotPath, " ") != "role park" {
		t.Fatalf("gotPath = %v, want [role park]", d.gotPath)
	}
	if !containsPair(d.gotArgs, "--before", "TASK-5") {
		t.Errorf("missing --before TASK-5 in %v", d.gotArgs)
	}
	if !containsPair(d.gotArgs, "--expected-park-seq", "2") {
		t.Errorf("missing --expected-park-seq 2 in %v", d.gotArgs)
	}

	// HTTP route mapper checks for park (top, before, after) and unpark.
	m, p, body, err := routeTable["role park"](map[string]any{
		"workspace": "docapp",
		"slug":      "implementer",
	})
	if err != nil || m != http.MethodPut || p != "/api/v1/workspaces/docapp/agent-roles/implementer/park" {
		t.Fatalf("role park top: m=%s p=%s err=%v", m, p, err)
	}
	var topBody map[string]any
	if err := json.Unmarshal(body, &topBody); err != nil || topBody["at"] != "top" {
		t.Fatalf("role park top body = %s (err=%v), want at=top", body, err)
	}

	m, p, body, err = routeTable["role park"](map[string]any{
		"workspace":         "docapp",
		"slug":              "implementer",
		"after":             "TASK-7",
		"expected_park_seq": float64(4),
	})
	if err != nil || m != http.MethodPut || p != "/api/v1/workspaces/docapp/agent-roles/implementer/park" {
		t.Fatalf("role park after: m=%s p=%s err=%v", m, p, err)
	}
	var afterBody map[string]any
	if err := json.Unmarshal(body, &afterBody); err != nil || afterBody["after"] != "TASK-7" || afterBody["expected_park_seq"] != float64(4) {
		t.Fatalf("role park after body = %s, want after=TASK-7 expected_park_seq=4", body)
	}

	m, p, body, err = routeTable["role unpark"](map[string]any{
		"workspace":         "docapp",
		"slug":              "implementer",
		"expected_park_seq": float64(5),
	})
	if err != nil || m != http.MethodDelete || p != "/api/v1/workspaces/docapp/agent-roles/implementer/park" {
		t.Fatalf("role unpark: m=%s p=%s err=%v", m, p, err)
	}
	var unparkBody map[string]any
	if err := json.Unmarshal(body, &unparkBody); err != nil || unparkBody["expected_park_seq"] != float64(5) {
		t.Fatalf("role unpark body = %s, want expected_park_seq=5", body)
	}

	// pad_item claim ignore_park reaches HTTP query string.
	m, p, _, err = routeTable["item claim"](map[string]any{
		"workspace":   "docapp",
		"ref":         "TASK-5",
		"ignore_park": true,
	})
	if err != nil || m != http.MethodPost || p != "/api/v1/workspaces/docapp/items/TASK-5/claim?ignore_park=true" {
		t.Fatalf("item claim ignore_park: m=%s p=%s err=%v", m, p, err)
	}
}

// TestCatalogProject_RoleParamAcceptedForReadyNextAndRefusedElsewhere checks
// action scoping on pad_project's `role` param and end-to-end HTTP dispatch.
func TestCatalogProject_RoleParamAcceptedForReadyNextAndRefusedElsewhere(t *testing.T) {
	def := findCatalogTool(t, "pad_project")

	// Refused on non-ready/next actions (e.g. stale, dashboard).
	for _, action := range []string{"stale", "dashboard", "activity"} {
		d := &fakeDispatcher{}
		env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: d}
		res, err := makeFanOutHandler(def, env)(context.Background(), callToolRequest(map[string]any{
			"action": action,
			"role":   "implementer",
		}))
		if err != nil {
			t.Fatalf("action=%s: unexpected Go error: %v", action, err)
		}
		if res == nil || !res.IsError {
			t.Fatalf("pad_project.%s accepted role param, want refusal: %s", action, textOf(res))
		}
		envErr := decodeEnvelope(t, res)
		if envErr.Error.Code != ErrValidationFailed {
			t.Errorf("pad_project.%s code = %q, want validation_failed", action, envErr.Error.Code)
		}
		if d.gotPath != nil {
			t.Errorf("pad_project.%s dispatched despite refused role param: %v", action, d.gotPath)
		}
	}

	// Accepted on stdio for ready and next -> forwards --role.
	for _, action := range []string{"ready", "next"} {
		d := &fakeDispatcher{}
		env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: NewWorkspaceState("docapp"), Dispatcher: d}
		res, err := makeFanOutHandler(def, env)(context.Background(), callToolRequest(map[string]any{
			"action": action,
			"role":   "implementer",
		}))
		if err != nil || (res != nil && res.IsError) {
			t.Fatalf("stdio pad_project.%s with role failed: err=%v res=%s", action, err, textOf(res))
		}
		if !containsPair(d.gotArgs, "--role", "implementer") {
			t.Errorf("pad_project.%s missing --role implementer in %v", action, d.gotArgs)
		}
	}

	// Accepted on HTTP dispatcher for ready and next against a real server.
	srv, st := newPadServer(t)
	user, err := st.CreateUser(models.UserCreate{Email: "mcp-role@example.com", Name: "Alice", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := st.CreateWorkspace(models.WorkspaceCreate{Name: "MCPRoleWS", OwnerID: user.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddWorkspaceMember(ws.ID, user.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	schema := `{"fields":[{"key":"status","type":"select","options":["open","in_progress","done"],"terminal_options":["done"],"default":"open"}]}`
	coll, err := st.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "TASK", Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	role, err := st.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Implementer", Slug: "implementer"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		it, err := st.CreateItem(ws.ID, coll.ID, models.ItemCreate{
			Title: "Task", Fields: `{"status":"open"}`, AgentRoleID: &role.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateRoleSortOrder(ws.ID, []store.RoleSortUpdate{{ItemID: it.ID, RoleSortOrder: (i + 1) * 1024}}); err != nil {
			t.Fatal(err)
		}
	}

	disp := &HTTPHandlerDispatcher{Handler: srv, UserResolver: fixedUserResolver(user)}
	ctxReady := WithDispatchInput(context.Background(), map[string]any{"workspace": ws.Slug, "role": "implementer"})
	resReady, err := disp.Dispatch(ctxReady, []string{"project", "ready"}, nil)
	if err != nil || resReady == nil || resReady.IsError {
		t.Fatalf("HTTP project ready --role failed: err=%v res=%s", err, textOf(resReady))
	}
	var readyBody struct {
		Count   int              `json:"count"`
		Results []map[string]any `json:"results"`
		Role    string           `json:"role"`
		Now     []map[string]any `json:"now"`
	}
	if err := json.Unmarshal([]byte(textOf(resReady)), &readyBody); err != nil {
		t.Fatalf("decode HTTP ready --role: %v\n%s", err, textOf(resReady))
	}
	if readyBody.Count != 4 || len(readyBody.Results) != 4 || readyBody.Role != "implementer" {
		t.Fatalf("HTTP ready --role = %+v, want count=4 role=implementer", readyBody)
	}

	ctxNext := WithDispatchInput(context.Background(), map[string]any{"workspace": ws.Slug, "role": "implementer"})
	resNext, err := disp.Dispatch(ctxNext, []string{"project", "next"}, nil)
	if err != nil || resNext == nil || resNext.IsError {
		t.Fatalf("HTTP project next --role failed: err=%v res=%s", err, textOf(resNext))
	}
	var nextBody struct {
		Count   int              `json:"count"`
		Results []map[string]any `json:"results"`
		Role    string           `json:"role"`
	}
	if err := json.Unmarshal([]byte(textOf(resNext)), &nextBody); err != nil {
		t.Fatalf("decode HTTP next --role: %v\n%s", err, textOf(resNext))
	}
	if nextBody.Count != 3 || len(nextBody.Results) != 3 || nextBody.Role != "implementer" {
		t.Fatalf("HTTP next --role = %+v, want count=3 role=implementer", nextBody)
	}
}

// A present but empty or non-string anchor is refused by the HTTP mapper rather
// than falling through to a park at the top.
func TestRoleParkRoute_EmptyAnchorRefused(t *testing.T) {
	for _, in := range []map[string]any{
		{"workspace": "docapp", "slug": "implementer", "before": ""},
		{"workspace": "docapp", "slug": "implementer", "after": "   "},
		{"workspace": "docapp", "slug": "implementer", "before": float64(3)},
	} {
		if _, _, _, err := routeTable["role park"](in); err == nil || !strings.Contains(err.Error(), "non-empty item ref") {
			t.Fatalf("input %v: err = %v, want a non-empty-item-ref refusal", in, err)
		}
	}
}

// An empty role is refused over HTTP, as the CLI refuses --role "" on stdio.
func TestProjectRoleParam_EmptyRefused(t *testing.T) {
	for _, v := range []any{"", "   "} {
		_, ok, res := projectRoleParam(map[string]any{"role": v}, "project ready")
		if ok || res == nil || !res.IsError {
			t.Fatalf("role %q: ok=%v res=%v, want a refusal", v, ok, res)
		}
		if got := decodeEnvelope(t, res).Error.Code; got != ErrValidationFailed {
			t.Fatalf("role %q: code = %q, want validation_failed", v, got)
		}
	}
	if _, ok, res := projectRoleParam(map[string]any{}, "project ready"); ok || res != nil {
		t.Fatalf("absent role: ok=%v res=%v, want absent with no error", ok, res)
	}
}
