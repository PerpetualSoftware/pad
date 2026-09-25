package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2819: every branch of the audit parser records where the tool name came
// from, decided at the branch itself. The discriminating rows are the forged
// ones: a caller who NAMES a tool "(unknown)" or "tools/call" must never read
// as the server's placeholder.
func TestParseMCPRequestBodyWithSource(t *testing.T) {
	cases := []struct {
		name, body string
		wantName   string
		wantSource models.MCPToolNameSource
	}{
		{"empty body", ``, "(unknown)", models.MCPToolNameSynthesised},
		{"not json", `not json at all`, "(unknown)", models.MCPToolNameSynthesised},
		{"tools/call without params", `{"method":"tools/call"}`, "tools/call", models.MCPToolNameSynthesised},
		{"tools/call with an empty name", `{"method":"tools/call","params":{"name":""}}`, "tools/call", models.MCPToolNameSynthesised},
		{"a method that cleans to nothing", `{"method":"\u0000"}`, "(unknown)", models.MCPToolNameSynthesised},
		{"a real tool", `{"method":"tools/call","params":{"name":"pad_item","arguments":{}}}`, "pad_item", models.MCPToolNameFromCaller},
		{"a real method", `{"method":"tools/list"}`, "tools/list", models.MCPToolNameFromCaller},
		{"a tool FORGED as (unknown)", `{"method":"tools/call","params":{"name":"(unknown)","arguments":{}}}`, "(sanitised) (unknown)", models.MCPToolNameSanitised},
		{"a method FORGED as (unknown)", `{"method":"(unknown)"}`, "(sanitised) (unknown)", models.MCPToolNameSanitised},
		{"a name cleaning changed", `{"method":"tools/call","params":{"name":"pad\u0000_item","arguments":{}}}`, "(sanitised) pad_item", models.MCPToolNameSanitised},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, hash, source := parseMCPRequestBodyWithSource([]byte(tc.body))
			if name != tc.wantName || source != tc.wantSource {
				t.Errorf("got (%q, %q), want (%q, %q)", name, source, tc.wantName, tc.wantSource)
			}
			// The two-value form is unchanged by the provenance work.
			if pName, pHash := parseMCPRequestBody([]byte(tc.body)); pName != name || pHash != hash {
				t.Errorf("parseMCPRequestBody = (%q, %q), WithSource gave (%q, %q)", pName, pHash, name, hash)
			}
			if !source.Valid() {
				t.Errorf("source %q is not a valid stored value", source)
			}
		})
	}
}

// Through the router: a caller who names a tool "(unknown)" and a body the
// server cannot parse both used to be distinguishable only by the reserved
// "(sanitised) " prefix. The row now says which it was.
func TestMCPAudit_BUG2819_SourceIsRecordedThroughTheMiddleware(t *testing.T) {
	srv, user, bearer := auditedMCPServer(t)
	send := func(body string) {
		req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.1:1234"
		srv.ServeHTTP(httptest.NewRecorder(), req)
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"(unknown)","arguments":{}}}`)
	send(`not json at all`)

	rows := waitForAuditRows(t, srv, user.ID, 2)
	got := map[string]models.MCPToolNameSource{}
	for _, r := range rows {
		got[r.ToolName] = r.ToolNameSource
	}
	if got["(sanitised) (unknown)"] != models.MCPToolNameSanitised {
		t.Errorf("forged (unknown): rows %v, want \"(sanitised) (unknown)\" recorded as sanitised", got)
	}
	if got["(unknown)"] != models.MCPToolNameSynthesised {
		t.Errorf("unparseable body: rows %v, want \"(unknown)\" recorded as synthesised", got)
	}
}
