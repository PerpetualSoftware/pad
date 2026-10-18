package main

// BUG-3879: a tool call refused inside a 200 /mcp response was audited as
// ok, because the audit middleware classified only the transport status.
// Driven through the server exactly as `pad server` wires it (wireMCP): the
// real bearer auth, audit middleware, Streamable HTTP transport, catalog,
// dispatcher and REST routes, so the rows below vouch for the binding and
// not only for the components (team CONVE-19).

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestBUG3879_ToolOutcomeReachesTheAuditRow(t *testing.T) {
	f := newCapFixture(t, cfgHTTP, false, nil, "true")
	readOnly, err := f.store.CreateAPIToken(f.member.ID, models.APITokenCreate{Name: "read-only", Scopes: `["read"]`}, 30, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken read-only: %v", err)
	}
	viewer, err := f.store.CreateUser(models.UserCreate{Email: "viewer-3879@example.com", Name: "Viewer", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatalf("CreateUser viewer: %v", err)
	}
	if err := f.store.AddWorkspaceMember(f.ws.ID, viewer.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember viewer: %v", err)
	}
	viewerPAT, err := f.store.CreateAPIToken(viewer.ID, models.APITokenCreate{Name: "viewer"}, 30, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken viewer: %v", err)
	}
	if err := f.store.SeedCollectionsFromTemplate(f.ws.ID, "startup"); err != nil {
		t.Fatalf("SeedCollectionsFromTemplate: %v", err)
	}

	create := `{"action":"create","workspace":"cap-ws","collection":"tasks","title":"BUG-3879 probe"}`
	cases := []struct {
		name       string
		user       *models.User
		bearer     string
		args       string
		wantStatus models.MCPAuditResultStatus
		wantKind   string
	}{
		// A write the caller may make stays ok.
		{"allowed write", f.member, f.memberPAT, create, models.MCPAuditResultOK, ""},
		// Refused before dispatch, by buildAuthedRequest's token-scope check.
		// error_kind is code:reason, the reason BUG-3864 puts on the 403.
		{"read-only key writes", f.member, readOnly.Token, create, models.MCPAuditResultDenied, "permission_denied:token_scope"},
		// Refused by the inner REST route's role check (an inner 403).
		{"viewer writes", viewer, viewerPAT.Token, create, models.MCPAuditResultDenied, "permission_denied:role"},
		// A failure that is not a refusal is an error, with its code.
		{"missing item", f.member, f.memberPAT, `{"action":"get","workspace":"cap-ws","ref":"TASK-99999"}`, models.MCPAuditResultError, "item_not_found"},
		// Refused by the catalog before any dispatch.
		{"unknown action", f.member, f.memberPAT, `{"action":"show","workspace":"cap-ws","ref":"TASK-1"}`, models.MCPAuditResultError, "validation_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bug3879Call(t, f, tc.user, tc.bearer, "pad_item", tc.args, tc.wantStatus, tc.wantKind)
		})
	}
	// mcp-go refuses an unknown tool before any tool middleware runs, with a
	// JSON-RPC error inside a 200 (hunter r1 M1): the OnError hook's row.
	t.Run("unknown tool", func(t *testing.T) {
		bug3879Call(t, f, f.member, f.memberPAT, "pad_no_such_tool", `{}`, models.MCPAuditResultError, "tool_not_found")
	})
}

// bug3879Call runs one tools/call and checks the one audit row it wrote.
// A tool failure answers with an error RESULT (isError); a call mcp-go
// refuses before any tool runs answers with a JSON-RPC error instead.
func bug3879Call(t *testing.T, f *capFixture, user *models.User, bearer, tool, args string, wantStatus models.MCPAuditResultStatus, wantKind string) {
	t.Helper()
	before := bug3879Rows(t, f, user.ID, tool)
	body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
	rr := bug3879Post(t, f, bearer, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("tools/call answered %d, want the 200 this bug is about: %.300s", rr.Code, rr.Body.String())
	}
	failed := strings.Contains(rr.Body.String(), `"isError":true`) || strings.Contains(rr.Body.String(), `"error":{"code":`)
	if wantFailed := wantStatus != models.MCPAuditResultOK; failed != wantFailed {
		t.Fatalf("precondition: call failed=%v, want %v: %.500s", failed, wantFailed, rr.Body.String())
	}
	row := bug3879NextRow(t, f, user.ID, tool, before)
	kind := ""
	if row.ErrorKind != nil {
		kind = *row.ErrorKind
	}
	if row.ResultStatus != wantStatus || kind != wantKind {
		t.Fatalf("audit row = (%q, %q), want (%q, %q); response %.300s",
			row.ResultStatus, kind, wantStatus, wantKind, rr.Body.String())
	}
}

func bug3879Post(t *testing.T, f *capFixture, bearer, body string) *mcpRecorder {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = f.host
	req.RemoteAddr = "192.0.2.10:4242"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	req.Header.Set("Authorization", "Bearer "+bearer)
	rr := newMCPRecorder()
	f.srv.ServeHTTP(rr, req)
	return rr
}

// bug3879Rows is the user's audit rows for one tool.
func bug3879Rows(t *testing.T, f *capFixture, userID, tool string) []models.MCPAuditEntry {
	t.Helper()
	rows, err := f.store.ListMCPAuditByUser(userID, 100, 0)
	if err != nil {
		t.Fatalf("ListMCPAuditByUser: %v", err)
	}
	var out []models.MCPAuditEntry
	for _, r := range rows {
		if r.ToolName == tool {
			out = append(out, r)
		}
	}
	return out
}

// bug3879NextRow waits for the one row the call wrote: the
// writer is asynchronous. The new row is found by id, not by position:
// two calls in one second tie on timestamp, and the tie-break is the
// random row id.
func bug3879NextRow(t *testing.T, f *capFixture, userID, tool string, before []models.MCPAuditEntry) models.MCPAuditEntry {
	t.Helper()
	seen := map[string]bool{}
	for _, r := range before {
		seen[r.ID] = true
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows := bug3879Rows(t, f, userID, tool)
		if len(rows) == len(before)+1 {
			for _, r := range rows {
				if !seen[r.ID] {
					return r
				}
			}
			t.Fatal("one more row, but every id was already seen")
		}
		if len(rows) > len(before)+1 {
			t.Fatalf("%d new %s rows, want exactly 1", len(rows)-len(before), tool)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s audit row within 5s", tool)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// BUG-3879 r4: since BUG-3864 an unverified email's refused write is its own
// code, email_not_verified, and it is a refusal like the other gates, so the
// audit row says denied. The Cloud fixture is the one BUG-3864's own test
// uses; the remote transport refuses in its gate before the request is built.
func TestBUG3879_UnverifiedEmailWriteAuditsAsDenied(t *testing.T) {
	f := newCapFixture(t, cfgHTTP, true, nil, "true")
	if err := f.store.SeedCollectionsFromTemplate(f.ws.ID, "startup"); err != nil {
		t.Fatalf("SeedCollectionsFromTemplate: %v", err)
	}
	unverified := bug3864User(t, f, "bug3879-unverified@example.com", true)
	if err := f.store.AddWorkspaceMember(f.ws.ID, unverified.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	tok := bug3864Token(t, f, unverified, `["*"]`)
	create := `{"action":"create","workspace":"cap-ws","collection":"tasks","title":"BUG-3879 unverified probe"}`
	bug3879Call(t, f, unverified, tok, "pad_item", create, models.MCPAuditResultDenied, "email_not_verified")
}
