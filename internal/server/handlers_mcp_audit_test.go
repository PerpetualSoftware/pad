package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// MCP audit handler tests (PLAN-943 TASK-960).
//
// Two endpoints:
//
//   - GET /api/v1/connected-apps/{id}/audit  — owner-scoped per-connection.
//   - GET /api/v1/admin/mcp-audit            — admin-only full table.
//
// What's covered:
//
//   - Per-connection endpoint requires auth + filters by user_id (a
//     user calling with another user's request_id sees zero rows).
//   - Admin endpoint rejects non-admin callers with 403.
//   - Admin endpoint returns rows across users.
//   - Pagination params parse correctly + cap at 200.
//   - Response DTO surfaces the right field names (the wire contract
//     uses `connection_id`, not `token_ref`, to keep the API decoupled
//     from the column name).

// seedAuditRow inserts one audit row for a user against a given
// (kind, ref) pair so the read endpoints have something to return.
func seedAuditRow(t *testing.T, srv *Server, userID string, kind models.TokenKind, ref, tool string) {
	t.Helper()
	err := srv.store.InsertMCPAuditEntry(models.MCPAuditEntryInput{
		UserID:       userID,
		TokenKind:    kind,
		TokenRef:     ref,
		ToolName:     tool,
		ResultStatus: models.MCPAuditResultOK,
		RequestID:    "req-seed",
	})
	if err != nil {
		t.Fatalf("InsertMCPAuditEntry: %v", err)
	}
}

func TestHandleMCPConnectionAudit_OwnerOnly(t *testing.T) {
	srv := testServer(t)

	alice, aliceTok := loginTestUser(t, srv)
	// Create a SECOND user via the same helper pattern.
	bob, err := srv.store.CreateUser(models.UserCreate{
		Email: "bob-audit@example.com", Name: "Bob", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}

	// Both users have audit rows against the same connection ref.
	seedAuditRow(t, srv, alice.ID, models.TokenKindOAuth, "ref-shared", "pad_item")
	seedAuditRow(t, srv, bob.ID, models.TokenKindOAuth, "ref-shared", "pad_item")

	// Alice asks for the connection — should see her row only.
	rr := doAuthedRequest(srv, "GET", "/api/v1/connected-apps/ref-shared/audit", nil, aliceTok)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rr.Body.String())
	}
	if len(resp.Items) != 1 {
		t.Errorf("got %d items, want 1 (Alice's only)", len(resp.Items))
	}
	if got, _ := resp.Items[0]["user_id"].(string); got != alice.ID {
		t.Errorf("returned user_id = %q, want Alice's %q", got, alice.ID)
	}
}

func TestHandleMCPConnectionAudit_RequiresAuth(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "GET", "/api/v1/connected-apps/anything/audit", nil)
	if rr.Code != http.StatusUnauthorized && rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 401 or 403 (auth-required)", rr.Code)
	}
}

func TestHandleMCPConnectionAudit_DTOShape(t *testing.T) {
	srv := testServer(t)
	user, tok := loginTestUser(t, srv)
	// Workspace-bound row to exercise the workspace_id field too.
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "DTO WS"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	err = srv.store.InsertMCPAuditEntry(models.MCPAuditEntryInput{
		UserID:       user.ID,
		WorkspaceID:  ws.ID,
		TokenKind:    models.TokenKindOAuth,
		TokenRef:     "shape-ref",
		ToolName:     "pad_item",
		ArgsHash:     "hashy",
		ResultStatus: models.MCPAuditResultOK,
		LatencyMs:    13,
		RequestID:    "req-shape",
	})
	if err != nil {
		t.Fatalf("InsertMCPAuditEntry: %v", err)
	}

	rr := doAuthedRequest(srv, "GET", "/api/v1/connected-apps/shape-ref/audit", nil, tok)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("len items = %d, want 1", len(resp.Items))
	}
	row := resp.Items[0]
	wantKeys := []string{"id", "timestamp", "user_id", "workspace_id",
		"token_kind", "connection_id", "tool_name", "args_hash",
		"result_status", "latency_ms", "request_id"}
	for _, k := range wantKeys {
		if _, ok := row[k]; !ok {
			t.Errorf("missing field %q in DTO; got %+v", k, row)
		}
	}
	if got, _ := row["connection_id"].(string); got != "shape-ref" {
		t.Errorf("connection_id = %v, want %q", row["connection_id"], "shape-ref")
	}
	if got, _ := row["token_kind"].(string); got != "oauth" {
		t.Errorf("token_kind = %v, want oauth", row["token_kind"])
	}
}

func TestHandleAdminMCPAudit_RejectsNonAdmin(t *testing.T) {
	srv := testServer(t)
	_, tok := loginTestUser(t, srv) // default role: user, not admin
	rr := doAuthedRequest(srv, "GET", "/api/v1/admin/mcp-audit", nil, tok)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for non-admin", rr.Code)
	}
}

func TestHandleAdminMCPAudit_AdminSeesAcrossUsers(t *testing.T) {
	srv := testServer(t)
	admin, adminTok := loginTestUser(t, srv)
	// Promote to admin so the /admin/mcp-audit gate passes.
	if err := srv.store.SetUserRole(admin.ID, "admin"); err != nil {
		t.Fatalf("SetUserRole: %v", err)
	}

	bob, err := srv.store.CreateUser(models.UserCreate{
		Email: "bob-admin-audit@example.com", Name: "Bob", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}
	seedAuditRow(t, srv, admin.ID, models.TokenKindOAuth, "ref-a", "pad_item")
	seedAuditRow(t, srv, bob.ID, models.TokenKindOAuth, "ref-b", "pad_item")

	rr := doAuthedRequest(srv, "GET", "/api/v1/admin/mcp-audit?limit=10", nil, adminTok)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		Items   []map[string]any `json:"items"`
		Dropped uint64           `json:"dropped"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Errorf("len items = %d, want 2 (across users)", len(resp.Items))
	}
}

func TestParseMCPAuditPaging_CapsAt200(t *testing.T) {
	req := httptest.NewRequest("GET", "/?limit=10000&offset=50", nil)
	limit, offset := parseMCPAuditPaging(req)
	if limit != 200 {
		t.Errorf("limit = %d, want 200 (cap)", limit)
	}
	if offset != 50 {
		t.Errorf("offset = %d, want 50", offset)
	}

	// Defaults.
	req2 := httptest.NewRequest("GET", "/", nil)
	limit2, offset2 := parseMCPAuditPaging(req2)
	if limit2 != 50 {
		t.Errorf("default limit = %d, want 50", limit2)
	}
	if offset2 != 0 {
		t.Errorf("default offset = %d, want 0", offset2)
	}
}
