package server

import (
	"net/http"
	"testing"
)

func TestTokenScopeAllows(t *testing.T) {
	tests := []struct {
		name   string
		scopes string
		method string
		path   string
		want   bool
	}{
		// Wildcard scope
		{"wildcard allows GET", `["*"]`, http.MethodGet, "/api/v1/test", true},
		{"wildcard allows POST", `["*"]`, http.MethodPost, "/api/v1/test", true},
		{"wildcard allows DELETE", `["*"]`, http.MethodDelete, "/api/v1/test", true},

		// Empty/default scopes
		{"empty string allows all", "", http.MethodPost, "/api/v1/test", true},
		{"default wildcard allows all", `["*"]`, http.MethodPatch, "/api/v1/test", true},

		// Read scope
		{"read allows GET", `["read"]`, http.MethodGet, "/api/v1/test", true},
		{"read allows HEAD", `["read"]`, http.MethodHead, "/api/v1/test", true},
		{"read allows OPTIONS", `["read"]`, http.MethodOptions, "/api/v1/test", true},
		{"read blocks POST", `["read"]`, http.MethodPost, "/api/v1/test", false},
		{"read blocks DELETE", `["read"]`, http.MethodDelete, "/api/v1/test", false},
		{"read blocks PATCH", `["read"]`, http.MethodPatch, "/api/v1/test", false},

		// Write scope
		{"write allows GET", `["write"]`, http.MethodGet, "/api/v1/test", true},
		{"write allows POST", `["write"]`, http.MethodPost, "/api/v1/test", true},
		{"write allows DELETE", `["write"]`, http.MethodDelete, "/api/v1/test", true},

		// Invalid/unparseable JSON — now DENY (TASK-667 deny-by-default).
		// Data corruption / tampering should not fall open.
		{"invalid json denies all", "not-json", http.MethodPost, "/api/v1/test", false},
		{"invalid json denies GET", "not-json", http.MethodGet, "/api/v1/test", false},

		// JSON null MUST NOT fall through the legacy-unrestricted path.
		// json.Unmarshal("null", &[]string) succeeds and leaves nil slice —
		// without an explicit raw-string check we'd grant full access.
		{"json null denies POST", "null", http.MethodPost, "/api/v1/test", false},
		{"json null denies GET", "null", http.MethodGet, "/api/v1/test", false},

		// Multiple scopes
		{"read+write allows POST", `["read","write"]`, http.MethodPost, "/api/v1/test", true},
		{"read only blocks PUT", `["read"]`, http.MethodPut, "/api/v1/test", false},

		// Empty array still allows all — legacy "unrestricted" form. New
		// tokens should use ["*"]. Whitespace-padded variants that clients
		// might serialize also count as empty arrays.
		{"empty array allows all", `[]`, http.MethodGet, "/api/v1/test", true},
		{"empty array allows POST", `[]`, http.MethodPost, "/api/v1/test", true},
		{"empty array with spaces", `[ ]`, http.MethodPost, "/api/v1/test", true},
		{"empty array with newline", "[\n]", http.MethodPost, "/api/v1/test", true},
		{"empty array with tab", "[\t]", http.MethodGet, "/api/v1/test", true},
		{"wildcard with spaces", `[ "*" ]`, http.MethodPost, "/api/v1/test", true},

		// Unrecognized scopes — TASK-667 deny-by-default. A typo like
		// "read-only" must NOT silently grant full access.
		{"unknown scope only denies GET", `["docs"]`, http.MethodGet, "/api/v1/test", false},
		{"unknown scope only denies POST", `["repo"]`, http.MethodPost, "/api/v1/test", false},
		{"read-only typo denies GET", `["read-only"]`, http.MethodGet, "/api/v1/test", false},
		{"unknown+read allows GET", `["docs","read"]`, http.MethodGet, "/api/v1/test", true},
		{"unknown+read blocks POST", `["docs","read"]`, http.MethodPost, "/api/v1/test", false},
		{"unknown+wildcard still allows POST", `["docs","*"]`, http.MethodPost, "/api/v1/test", true},
		{"unknown+write still allows DELETE", `["docs","write"]`, http.MethodDelete, "/api/v1/test", true},

		// OAuth scope vocabulary (sub-PR E, TASK-1027). MCPBearerAuth
		// stashes fosite-issued scopes as JSON arrays alongside PAT
		// scopes, so the same policy applies. Asserts the read/write/
		// admin mappings hold under the OAuth namespace.
		{"pad:read allows GET", `["pad:read"]`, http.MethodGet, "/api/v1/test", true},
		{"pad:read allows HEAD", `["pad:read"]`, http.MethodHead, "/api/v1/test", true},
		{"pad:read allows OPTIONS", `["pad:read"]`, http.MethodOptions, "/api/v1/test", true},
		{"pad:read blocks POST", `["pad:read"]`, http.MethodPost, "/api/v1/test", false},
		{"pad:read blocks PATCH", `["pad:read"]`, http.MethodPatch, "/api/v1/test", false},
		{"pad:read blocks DELETE", `["pad:read"]`, http.MethodDelete, "/api/v1/test", false},
		{"pad:write allows GET", `["pad:write"]`, http.MethodGet, "/api/v1/test", true},
		{"pad:write allows POST", `["pad:write"]`, http.MethodPost, "/api/v1/test", true},
		{"pad:write allows DELETE", `["pad:write"]`, http.MethodDelete, "/api/v1/test", true},
		{"pad:write allows PATCH", `["pad:write"]`, http.MethodPatch, "/api/v1/test", true},
		{"pad:admin allows POST", `["pad:admin"]`, http.MethodPost, "/api/v1/test", true},
		{"pad:admin allows DELETE", `["pad:admin"]`, http.MethodDelete, "/api/v1/test", true},
		// Multi-scope OAuth grants (the realistic shape — DCR clients
		// usually request all scopes they might need).
		{"pad:read+pad:write allows POST", `["pad:read","pad:write"]`, http.MethodPost, "/api/v1/test", true},
		{"pad:read+pad:write allows GET", `["pad:read","pad:write"]`, http.MethodGet, "/api/v1/test", true},
		// PAT + OAuth scope mixed (defensive — shouldn't happen in
		// practice but the policy should still work).
		{"read+pad:write allows POST", `["read","pad:write"]`, http.MethodPost, "/api/v1/test", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tokenScopeAllows(tt.scopes, tt.method, tt.path)
			if got != tt.want {
				t.Errorf("tokenScopeAllows(%q, %q, %q) = %v, want %v",
					tt.scopes, tt.method, tt.path, got, tt.want)
			}
		})
	}
}
