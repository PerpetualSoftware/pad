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

		// Invalid/unparseable JSON (backward compat)
		{"invalid json allows all", "not-json", http.MethodPost, "/api/v1/test", true},

		// Multiple scopes
		{"read+write allows POST", `["read","write"]`, http.MethodPost, "/api/v1/test", true},
		{"read only blocks PUT", `["read"]`, http.MethodPut, "/api/v1/test", false},

		// Empty array
		{"empty array blocks all", `[]`, http.MethodGet, "/api/v1/test", false},
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
