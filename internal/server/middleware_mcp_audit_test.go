package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// MCP audit middleware tests (PLAN-943 TASK-960).
//
// What's covered:
//
//   - A successful tool call writes one audit row with the right
//     tool_name + args_hash + result_status.
//   - A 401 from the MCP gate (no bearer) doesn't produce a row —
//     audit only runs after MCPBearerAuth, and an unauth'd request
//     never gets there.
//   - A 200 with a `tools/list` JSON-RPC envelope records the
//     method as the tool_name (no args_hash).
//   - The buffer-full path increments the drop counter without
//     blocking the request.
//   - Args round-trip through canonical-JSON hashing — same payload
//     in different field orders produces the same args_hash.
//   - Async writes complete before Stop returns (clean drain).

// auditedMCPServer builds an MCP-enabled server and returns it +
// the user + a PAT bearer suitable for /mcp calls. The audit
// writer is started by SetMCPTransport (the wiring lives there).
func auditedMCPServer(t *testing.T) (srv *Server, user *models.User, bearer string) {
	t.Helper()
	srv = testServer(t)
	srv.SetCloudMode("test-secret")
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	})
	srv.SetMCPTransport(stub, "https://mcp.test.example", "https://app.test.example")

	var err error
	user, err = srv.store.CreateUser(models.UserCreate{
		Email: "mcp-audit@example.com", Name: "Audit Tester", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Audit WS"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	tok, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{
		Name:        "audit-test-pat",
		WorkspaceID: ws.ID,
	}, 30, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	return srv, user, tok.Token
}

// waitForAuditRows polls ListMCPAuditByUser until we see the
// expected count or the deadline expires. The middleware writes
// async via a buffered channel + worker goroutine, so the row
// won't appear immediately; this avoids flakes without sleeping
// for an arbitrary fixed duration.
func waitForAuditRows(t *testing.T, srv *Server, userID string, want int) []models.MCPAuditEntry {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows, err := srv.store.ListMCPAuditByUser(userID, 100, 0)
		if err != nil {
			t.Fatalf("ListMCPAuditByUser: %v", err)
		}
		if len(rows) >= want {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit rows = %d after 2s, want >= %d", len(rows), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMCPAudit_RecordsToolCallWithToolNameAndArgsHash(t *testing.T) {
	srv, user, bearer := auditedMCPServer(t)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pad_item","arguments":{"action":"list","collection":"tasks"}}}`
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}

	rows := waitForAuditRows(t, srv, user.ID, 1)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	got := rows[0]
	if got.ToolName != "pad_item" {
		t.Errorf("ToolName = %q, want %q", got.ToolName, "pad_item")
	}
	if got.ArgsHash == "" {
		t.Error("ArgsHash empty for a tools/call with arguments")
	}
	if got.TokenKind != models.TokenKindPAT {
		t.Errorf("TokenKind = %q, want %q", got.TokenKind, models.TokenKindPAT)
	}
	if got.TokenRef == "" {
		t.Error("TokenRef empty")
	}
	if got.ResultStatus != models.MCPAuditResultOK {
		t.Errorf("ResultStatus = %q, want %q", got.ResultStatus, models.MCPAuditResultOK)
	}
	if got.LatencyMs < 0 {
		t.Errorf("LatencyMs negative: %d", got.LatencyMs)
	}
}

func TestMCPAudit_NoBearer_NoRow(t *testing.T) {
	srv, user, _ := auditedMCPServer(t)
	// Same body as the happy-path test, but no Authorization header —
	// MCPBearerAuth 401s before the audit middleware runs, so no row.
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`))
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
	// Give the (nonexistent) async write a moment to land if it
	// did anyway, then confirm no rows.
	time.Sleep(100 * time.Millisecond)
	rows, err := srv.store.ListMCPAuditByUser(user.ID, 100, 0)
	if err != nil {
		t.Fatalf("ListMCPAuditByUser: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 audit rows for unauth'd request, got %d", len(rows))
	}
}

func TestMCPAudit_NonToolsCallMethod_RecordsMethodAsToolName(t *testing.T) {
	srv, user, bearer := auditedMCPServer(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize"}`
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	rows := waitForAuditRows(t, srv, user.ID, 1)
	if rows[0].ToolName != "initialize" {
		t.Errorf("ToolName = %q, want %q", rows[0].ToolName, "initialize")
	}
	if rows[0].ArgsHash != "" {
		t.Errorf("ArgsHash = %q, want empty for non-tools/call method", rows[0].ArgsHash)
	}
}

func TestMCPAudit_HashCanonicalJSON_IsOrderIndependent(t *testing.T) {
	// Same fields, different order → same hash. This is the
	// "group similar calls together" property the spec wants.
	a := hashCanonicalJSON([]byte(`{"action":"list","collection":"tasks"}`))
	b := hashCanonicalJSON([]byte(`{"collection":"tasks","action":"list"}`))
	if a == "" || b == "" {
		t.Fatalf("got empty hash: a=%q b=%q", a, b)
	}
	if a != b {
		t.Errorf("hashCanonicalJSON not order-independent: %q vs %q", a, b)
	}

	// Different content → different hash.
	c := hashCanonicalJSON([]byte(`{"action":"create","collection":"tasks"}`))
	if c == a {
		t.Error("hashCanonicalJSON collided across different content")
	}

	// nil/null/empty → empty hash (audit reflects "no args").
	if got := hashCanonicalJSON(nil); got != "" {
		t.Errorf("nil hash = %q, want empty", got)
	}
	if got := hashCanonicalJSON([]byte("null")); got != "" {
		t.Errorf("null hash = %q, want empty", got)
	}
}

func TestMCPAudit_ParseRequestBody(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantTool    string
		wantHashSet bool
	}{
		{"tools/call with args", `{"method":"tools/call","params":{"name":"pad_item","arguments":{"a":1}}}`, "pad_item", true},
		{"tools/list", `{"method":"tools/list"}`, "tools/list", false},
		{"initialize", `{"method":"initialize"}`, "initialize", false},
		{"empty body", ``, "(unknown)", false},
		{"malformed", `not json`, "(unknown)", false},
		{"tools/call missing name", `{"method":"tools/call","params":{}}`, "tools/call", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool, hash := parseMCPRequestBody([]byte(tc.body))
			if tool != tc.wantTool {
				t.Errorf("tool = %q, want %q", tool, tc.wantTool)
			}
			if (hash != "") != tc.wantHashSet {
				t.Errorf("hash empty=%v, want empty=%v (got %q)", hash == "", !tc.wantHashSet, hash)
			}
		})
	}
}

func TestMCPAudit_BufferFull_DropsAndIncrementsCounter(t *testing.T) {
	srv, user, _ := auditedMCPServer(t)
	if srv.mcpAudit == nil {
		t.Fatal("mcpAudit writer not started")
	}

	// Fill the queue to capacity by direct enqueue (bypassing the
	// HTTP path, which is rate-limited by the worker draining).
	// Stop the worker first so it doesn't drain while we're filling.
	// Then enqueue one extra entry; that one must drop.
	srv.mcpAudit.shutdown()
	// TASK-1120: SetMCPTransport (called by auditedMCPServer setup)
	// also spawned the session-tracker sweeper on srv.bg. Without
	// shutting that down too, srv.bg.Wait() below would block on the
	// sweeper's 5-minute ticker. Mirror what Server.Stop() does.
	srv.stopMCPSessionTracker()
	// Wait for the worker goroutine to finish so the queue is
	// guaranteed not to be drained mid-test.
	srv.bg.Wait()

	// Drain to a clean state.
	for {
		select {
		case <-srv.mcpAudit.queue:
		default:
			goto filled
		}
	}
filled:
	// Re-create the channel because shutdown closed `stop`, not
	// `queue`. The queue is still usable.
	for i := 0; i < mcpAuditBufferSize; i++ {
		ok := srv.mcpAudit.enqueue(models.MCPAuditEntryInput{
			UserID:       user.ID,
			TokenKind:    models.TokenKindPAT,
			TokenRef:     "ref",
			ToolName:     "x",
			ResultStatus: models.MCPAuditResultOK,
			RequestID:    "r",
		})
		if !ok {
			t.Fatalf("enqueue %d returned false; expected to fill to capacity first", i)
		}
	}
	// One past capacity → drop.
	if ok := srv.mcpAudit.enqueue(models.MCPAuditEntryInput{
		UserID:       user.ID,
		TokenKind:    models.TokenKindPAT,
		TokenRef:     "ref",
		ToolName:     "x",
		ResultStatus: models.MCPAuditResultOK,
		RequestID:    "r",
	}); ok {
		t.Fatal("expected drop on full queue, got accepted")
	}
	if got := srv.mcpAuditDroppedSnapshot(); got != 1 {
		t.Errorf("dropped counter = %d, want 1", got)
	}
}

// TestMCPAudit_RateLimited_RecordsDeniedRow pins the fix for Codex
// review on PR #389 round 1: when MCPBearerAuth resolves the user
// + token but then rate-limits the request, the wrapping
// MCPAuditLog never sees the response (it's mounted INSIDE
// MCPBearerAuth, which returns before next.ServeHTTP). The fix is
// emitMCPAuditDenied called from the rate-limit deny branch — this
// test asserts the audit row lands with result_status="denied" and
// error_kind="rate_limited".
func TestMCPAudit_RateLimited_RecordsDeniedRow(t *testing.T) {
	srv := mcpEnabledTestServer(t)
	pat := mustCreatePATForTest(t, srv, "audit-rate-limit")

	// Find the user we just created so we can scope the audit query.
	user, err := srv.store.GetUserByEmail("audit-rate-limit@example.com")
	if err != nil || user == nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}

	// Hammer until we see a 429 — same pattern as the existing
	// rate-limit tests.
	got429 := false
	for i := 0; i < 30; i++ {
		req := httptest.NewRequest("POST", "/mcp",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pad_item"}}`))
		req.Header.Set("Authorization", "Bearer "+pat)
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.1:1234"
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if rr.Code == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("never hit 429 within 30 requests")
	}

	// Wait for the async audit writer to drain. Burst sent + 1 denied
	// = burst+1 audit rows (every accepted call writes one too via
	// the wrapping middleware; the 429 writes one via the direct
	// emit). We just need at least one row with status=denied.
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows, err := srv.store.ListMCPAuditByUser(user.ID, 100, 0)
		if err != nil {
			t.Fatalf("ListMCPAuditByUser: %v", err)
		}
		var sawDenied bool
		for _, r := range rows {
			if r.ResultStatus == models.MCPAuditResultDenied {
				sawDenied = true
				if r.ErrorKind == nil || *r.ErrorKind != "rate_limited" {
					t.Errorf("denied row error_kind = %v, want rate_limited", r.ErrorKind)
				}
				if r.ToolName != "pad_item" {
					t.Errorf("denied row tool_name = %q, want pad_item", r.ToolName)
				}
				break
			}
		}
		if sawDenied {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no denied row landed within 2s; rows=%+v", rows)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMCPAudit_ClassifyResult(t *testing.T) {
	cases := []struct {
		status int
		want   models.MCPAuditResultStatus
		kind   string
	}{
		{200, models.MCPAuditResultOK, ""},
		{401, models.MCPAuditResultDenied, "unauthorized"},
		{403, models.MCPAuditResultDenied, "forbidden"},
		{429, models.MCPAuditResultDenied, "rate_limited"},
		{500, models.MCPAuditResultError, "server_error_500"},
		{502, models.MCPAuditResultError, "server_error_502"},
		{400, models.MCPAuditResultError, "client_error_400"},
		{404, models.MCPAuditResultError, "client_error_404"},
		{0, models.MCPAuditResultOK, ""}, // ResponseWriter never wrote → treat as OK
	}
	for _, tc := range cases {
		got, kind := classifyMCPResult(tc.status)
		if got != tc.want || kind != tc.kind {
			t.Errorf("classifyMCPResult(%d) = (%q, %q), want (%q, %q)",
				tc.status, got, kind, tc.want, tc.kind)
		}
	}
}
