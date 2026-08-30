package server

import (
	"encoding/json"
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
	// rate-limit tests. BUG-1430 raised burst to 60, so loop past it
	// to drain the bucket; the (burst+1)th request 429s.
	got429 := false
	for i := 0; i < 80; i++ {
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
		t.Fatal("never hit 429 within 80 requests")
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

// TestMCPAudit_ToolNameCannotCarryANUL closes a door BUG-2803's own
// completeness map wrongly certified as safe (codex round 20).
//
// That map listed middleware_mcp_audit.go as "audit capture — records the body
// and restores it; decoding still happens in the MCP dispatcher". The first
// half is true and the second is irrelevant: parseMCPRequestBody runs its OWN
// json.Unmarshal and hands the DECODED params.name straight to
// mcp_audit_log.tool_name, a TEXT NOT NULL column. Whether the dispatcher
// decodes the body again has no bearing on what this middleware persists.
//
// The /mcp transport decodes the JSON-RPC envelope itself rather than through
// decodeJSON, so nothing upstream refuses the escape either. On PostgreSQL the
// insert fails with 22021 — the exact symptom this whole unit exists to
// remove; on SQLite it is stored and the audit log carries an unprintable
// tool name.
//
// The disposition is SANITISE, not refuse, following the User-Agent precedent
// established earlier in this unit: the audit row is metadata the SERVER chose
// to record about a request, and the request that carries a malformed value is
// precisely the one you most want a row for. Failing the audit write would
// lose that row, which is worse than recording a cleaned name.
func TestMCPAudit_ToolNameCannotCarryANUL(t *testing.T) {
	srv, user, bearer := auditedMCPServer(t)

	post := func(t *testing.T, body string) {
		t.Helper()
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.1:1234"
		srv.ServeHTTP(httptest.NewRecorder(), req)
	}

	// Control FIRST, so the test asserts its own premise: an ordinary name is
	// stored verbatim. Without this leg the assertion below would pass against
	// a middleware that mangled or dropped every tool name.
	post(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pad_item","arguments":{}}}`)
	rows := waitForAuditRows(t, srv, user.ID, 1)
	if rows[0].ToolName != "pad_item" {
		t.Fatalf("premise failed: an ordinary tool name must round-trip verbatim, got %q", rows[0].ToolName)
	}

	post(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"pad_item`+escNULLiteral+`evil","arguments":{}}}`)
	rows = waitForAuditRows(t, srv, user.ID, 2)

	for _, row := range rows {
		if strings.ContainsRune(row.ToolName, 0) {
			t.Errorf("a decoded NUL reached mcp_audit_log.tool_name (%q). On PostgreSQL this insert "+
				"fails with 22021; the value must be sanitised before it is persisted", row.ToolName)
		}
	}

	// And the sanitised name must still IDENTIFY the tool — dropping the row
	// or storing "(unknown)" would make the audit log useless for exactly the
	// requests worth auditing.
	var sanitised string
	for _, row := range rows {
		if row.ToolName != "pad_item" {
			sanitised = row.ToolName
		}
	}
	// The cleaned text is kept for diagnosis, but MARKED — round 23 showed
	// that storing the bare cleaned name makes the row indistinguishable from
	// a genuine call with that name. Asserted as "contains the cleaned text
	// and is not equal to it", so this pins the two properties that matter
	// (diagnosable, and distinguishable) without pinning the marker's wording.
	if !strings.Contains(sanitised, "pad_itemevil") {
		t.Errorf("the NUL-bearing call should still be diagnosable from its cleaned name, got %q", sanitised)
	}
	if sanitised == "pad_itemevil" {
		t.Errorf("the cleaned name is stored bare, so it is indistinguishable from a genuine call " +
			"named pad_itemevil; it must be marked as cleaned")
	}

	// The OTHER path into tool_name. parseMCPRequestBody has two returns that
	// carry caller text - params.name for tools/call, and the METHOD itself
	// for everything else - and both are bound to the same column. Testing
	// only the first would leave the second's sanitise call unkilled by any
	// mutation, which is how a fixed surface count turns back into a defect.
	post(t, `{"jsonrpc":"2.0","id":3,"method":"tools/li`+escNULLiteral+`st"}`)
	rows = waitForAuditRows(t, srv, user.ID, 3)
	for _, row := range rows {
		if strings.ContainsRune(row.ToolName, 0) {
			t.Errorf("a decoded NUL reached tool_name via the METHOD path (%q); params.name is not "+
				"the only return that carries caller text", row.ToolName)
		}
	}
}

// TestMCPAudit_SanitiseNeverEmptiesToolName covers the boundary the round-20
// fix created and did not test (codex round 21, top-ranked un-probed lens).
//
// parseMCPRequestBody checks env.Method == "" and p.Name == "" BEFORE
// sanitising, so a value made ENTIRELY of NULs is non-empty at the fallback
// and empty by the time it is returned. tool_name is TEXT NOT NULL, so the
// row still inserts — with an empty identifier, which defeats the whole point
// of the "(unknown)" / "tools/call" fallbacks. That function's own doc comment
// says they exist to give the audit reader a visible signal rather than
// silently dropping the row; an empty string is the silent drop wearing a
// different shape.
//
// This is the round-20 fix's own boundary: I closed the NUL door and did not
// ask what the close does when it consumes the entire value.
func TestMCPAudit_SanitiseNeverEmptiesToolName(t *testing.T) {
	// escNULLiteral, not a local literal. Rolling my own here is exactly how
	// two of these tests became vacuous: written inline, the escape is one
	// backslash away from being the NUL itself, which is what that helper
	// exists to prevent and what its comment already warned about.
	nul := escNULLiteral

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"method is all NULs", `{"jsonrpc":"2.0","id":1,"method":"` + nul + `"}`, "(unknown)"},
		{"tools/call name is all NULs",
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + nul + `"}}`, "tools/call"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := parseMCPRequestBody([]byte(tc.body))
			if got == "" {
				t.Fatalf("tool_name came back EMPTY; the fallback must survive sanitisation, want %q", tc.want)
			}
			if got != tc.want {
				t.Errorf("tool_name = %q, want the fallback %q", got, tc.want)
			}
		})
	}

	// Premise: the same shapes with ordinary values still return the value
	// itself, so the assertions above are about emptiness rather than about
	// the fallback swallowing everything.
	if got, _ := parseMCPRequestBody([]byte(`{"method":"tools/list"}`)); got != "tools/list" {
		t.Fatalf("premise failed: an ordinary method must round-trip, got %q", got)
	}
}

// TestMCPAudit_RawMethodDecidesClassification pins that dispatch reads what the
// client SENT while only the stored value is cleaned (codex round 22).
//
// The round-21 fix sanitised before comparing, so "tools/<NUL>call" cleaned up
// INTO the literal "tools/call" and the parser then extracted params.name and
// hashed the arguments. Measured before the fix: tool_name="pad_item" with a
// full 64-character args_hash — an audit row indistinguishable from a genuine
// pad_item call, forgeable by anyone who can send a request.
func TestMCPAudit_RawMethodDecidesClassification(t *testing.T) {
	// escNULLiteral, not a local literal. Rolling my own here is exactly how
	// two of these tests became vacuous: written inline, the escape is one
	// backslash away from being the NUL itself, which is what that helper
	// exists to prevent and what its comment already warned about.
	nul := escNULLiteral

	name, hash := parseMCPRequestBody([]byte(
		`{"method":"tools/` + nul + `call","params":{"name":"pad_item","arguments":{}}}`))
	if name == "pad_item" {
		t.Errorf("a method that is not tools/call must NOT have params.name lifted out of it; " +
			"tool_name came back as the inner tool name, which forges a genuine call")
	}
	if hash != "" {
		t.Errorf("args_hash must be empty for a non-tools/call method, got %d chars", len(hash))
	}

	// Control: a genuine tools/call still classifies and still hashes, so the
	// assertions above pin the classification rather than a parser that
	// stopped working.
	name, hash = parseMCPRequestBody([]byte(
		`{"method":"tools/call","params":{"name":"pad_item","arguments":{}}}`))
	if name != "pad_item" || hash == "" {
		t.Fatalf("premise failed: a genuine tools/call must still yield its name and a hash, got %q / %d chars",
			name, len(hash))
	}
}

// TestMCPAudit_CleanedIdentityIsNotForgeable pins that a value which only
// became well-formed by cleaning cannot imitate a genuine one (codex round 23).
//
// Round 22 closed the coarse version: sanitising before classifying let
// "tools/<NUL>call" become a real tools/call and lift params.name. Classifying
// on the raw method fixed that, but cleaning is LOSSY, so the stored pair
// still collapsed onto a genuine identity — "pad_<NUL>item" stored exactly
// what "pad_item" stores, hash included. An audit row anyone can mint to look
// like someone else's call is not an audit row.
//
// Each case asserts the forged value DIFFERS from the genuine one. That is the
// property; the specific marker text is incidental and deliberately not
// asserted beyond being distinguishable.
func TestMCPAudit_CleanedIdentityIsNotForgeable(t *testing.T) {
	// escNULLiteral, not a local literal. Rolling my own here is exactly how
	// two of these tests became vacuous: written inline, the escape is one
	// backslash away from being the NUL itself, which is what that helper
	// exists to prevent and what its comment already warned about.
	nul := escNULLiteral

	for _, tc := range []struct {
		name          string
		forged, real_ string
	}{
		{
			"tool name",
			`{"method":"tools/call","params":{"name":"pad_` + nul + `item","arguments":{"a":1}}}`,
			`{"method":"tools/call","params":{"name":"pad_item","arguments":{"a":1}}}`,
		},
		{
			"method name",
			`{"method":"initialize` + nul + `"}`,
			`{"method":"initialize"}`,
		},
		{
			"method that cleans into tools/call",
			`{"method":"tools/` + nul + `call","params":{"name":"pad_item","arguments":{}}}`,
			`{"method":"tools/call","params":{"name":"pad_item","arguments":{}}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fName, fHash := parseMCPRequestBody([]byte(tc.forged))
			rName, rHash := parseMCPRequestBody([]byte(tc.real_))

			if fName == rName && fHash == rHash {
				t.Errorf("a NUL-bearing request stores the SAME audit identity as a genuine one "+
					"(tool_name=%q args_hash=%q) — anyone who can send a request can mint a row "+
					"attributed to a real call", fName, fHash)
			}
			if fName == "" {
				t.Errorf("the forged case must still record something diagnosable, got an empty tool_name")
			}
			// Premise: the genuine leg is the ordinary value, so the
			// assertion above is about the forged one differing rather than
			// about the parser having stopped working.
			if rName == "" {
				t.Fatalf("premise failed: the genuine request must record a name, got empty")
			}
		})
	}
}

// TestMetricsToolLabelIsBounded pins that the cleaned-identity marker costs
// exactly ONE extra Prometheus label value, not one per tool (codex round 24).
//
// The audit ROW keeps the cleaned name; an operator reading a single row needs
// to know which tool it resembles. A metric SERIES does not, and keeping the
// name there would split "(sanitised) pad_item" from "pad_item" per user and
// per status for a distinction no aggregate query asks.
//
// This bounds the MARKER only. The tool label as a whole is still caller-driven
// and unbounded (BUG-2817, pre-existing); that is why this collapses rather
// than passing the marked name through.
func TestMetricsToolLabelIsBounded(t *testing.T) {
	genuine := metricsToolLabel("pad_item")
	if genuine != "pad_item" {
		t.Fatalf("premise failed: an unmarked name must reach metrics unchanged, got %q", genuine)
	}

	// Every marked name collapses to the SAME label value, so the marker adds
	// one series family rather than doubling every tool's.
	seen := map[string]bool{}
	for _, name := range []string{"pad_item", "pad_workspace", "pad_search", "initialize"} {
		seen[metricsToolLabel(auditLabel(name, true))] = true
	}
	if len(seen) != 1 {
		t.Errorf("marked names must collapse to one metrics label; got %d distinct values: %v",
			len(seen), seen)
	}
	for label := range seen {
		if strings.ContainsAny(label, "abcdefghijklmnopqrstuvwxyz_") && label != "(sanitised)" {
			t.Errorf("the collapsed label still carries caller-derived text: %q", label)
		}
		if label == genuine {
			t.Errorf("the marked label collides with the genuine one (%q)", label)
		}
	}
}

// TestMCPAudit_SynthesisedNamespaceIsReserved pins that a caller cannot name a
// tool so that its UNMODIFIED value collides with a value this server
// synthesises (codex round 25).
//
// Marking only what cleaning changed was not enough: "(unknown)" is what the
// parser returns for a malformed body, and "(sanitised) pad_item" is what it
// returns for a NUL-bearing pad_item — both are strings a caller may simply
// choose. The leading "(" is now reserved, so any caller value entering that
// namespace is marked and therefore differs from the synthesised one.
func TestMCPAudit_SynthesisedNamespaceIsReserved(t *testing.T) {
	nul := escNULLiteral

	// What the server synthesises, obtained from the server rather than
	// hardcoded, so this test cannot drift away from the real values.
	synthUnknown, _ := parseMCPRequestBody([]byte(`not json at all`))
	synthMarked, _ := parseMCPRequestBody([]byte(
		`{"method":"tools/call","params":{"name":"pad_` + nul + `item","arguments":{}}}`))
	if synthUnknown == "" || synthMarked == "" {
		t.Fatalf("premise failed: expected synthesised values, got %q and %q", synthUnknown, synthMarked)
	}

	for _, tc := range []struct {
		name, chosen, collidesWith string
	}{
		{"a tool named like the unknown fallback", synthUnknown, synthUnknown},
		{"a tool named like a marked identity", synthMarked, synthMarked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := parseMCPRequestBody([]byte(
				`{"method":"tools/call","params":{"name":` + mustJSONString(t, tc.chosen) + `,"arguments":{}}}`))
			if got == tc.collidesWith {
				t.Errorf("a caller choosing the name %q produces the same audit identity as the value "+
					"the server synthesises; a genuine request is then indistinguishable from a "+
					"substituted one", tc.chosen)
			}
			if got == "" {
				t.Errorf("the call must still record something diagnosable, got an empty tool_name")
			}
		})
	}

	// Premise: an ordinary name outside the reserved namespace is untouched,
	// so the assertions above pin the namespace rather than a parser that
	// marks everything.
	if got, _ := parseMCPRequestBody([]byte(
		`{"method":"tools/call","params":{"name":"pad_item","arguments":{}}}`)); got != "pad_item" {
		t.Fatalf("premise failed: an ordinary name must be recorded verbatim, got %q", got)
	}
}

func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal %q: %v", s, err)
	}
	return string(b)
}
