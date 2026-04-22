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

		// Multiple scopes
		{"read+write allows POST", `["read","write"]`, http.MethodPost, "/api/v1/test", true},
		{"read only blocks PUT", `["read"]`, http.MethodPut, "/api/v1/test", false},

		// Empty array still allows all — legacy "unrestricted" form. New
		// tokens should use ["*"].
		{"empty array allows all", `[]`, http.MethodGet, "/api/v1/test", true},
		{"empty array allows POST", `[]`, http.MethodPost, "/api/v1/test", true},

		// Unrecognized scopes — TASK-667 deny-by-default. A typo like
		// "read-only" must NOT silently grant full access.
		{"unknown scope only denies GET", `["docs"]`, http.MethodGet, "/api/v1/test", false},
		{"unknown scope only denies POST", `["repo"]`, http.MethodPost, "/api/v1/test", false},
		{"read-only typo denies GET", `["read-only"]`, http.MethodGet, "/api/v1/test", false},
		{"unknown+read allows GET", `["docs","read"]`, http.MethodGet, "/api/v1/test", true},
		{"unknown+read blocks POST", `["docs","read"]`, http.MethodPost, "/api/v1/test", false},
		{"unknown+wildcard still allows POST", `["docs","*"]`, http.MethodPost, "/api/v1/test", true},
		{"unknown+write still allows DELETE", `["docs","write"]`, http.MethodDelete, "/api/v1/test", true},
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
