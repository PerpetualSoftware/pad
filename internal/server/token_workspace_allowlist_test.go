package server

import (
	"context"
	"testing"
)

// TestTokenAllowedWorkspaceMatches covers the policy table for the
// OAuth-token workspace allow-list (TASK-953):
//
//   - nil allow-list → no gate; every slug allowed (PAT auth or
//     pre-TASK-952 OAuth tokens fall here).
//   - ["*"] wildcard → every slug allowed.
//   - ["foo", "bar"] → only "foo" and "bar" allowed; "baz" denied.
//   - explicit empty list → denies every slug (fail-closed; in
//     practice the consent flow rejects empty lists at parse time,
//     but the helper must not "open up" if one slips through).
//   - ["foo", "*"] → wildcard wins; every slug allowed.
//
// Cheap pure-function unit test; the integration flow is covered
// separately in handlers_mcp_test.go's MCP+OAuth test surface.
func TestTokenAllowedWorkspaceMatches(t *testing.T) {
	cases := []struct {
		name    string
		allowed []string
		slug    string
		want    bool
	}{
		// nil → no gate (PAT auth or pre-TASK-952 token).
		{"nil allows anything 1", nil, "foo", true},
		{"nil allows anything 2", nil, "anything", true},

		// Wildcard.
		{"wildcard allows foo", []string{"*"}, "foo", true},
		{"wildcard allows arbitrary", []string{"*"}, "anything-else", true},

		// Explicit allow-list — only listed slugs match.
		{"explicit foo matches foo", []string{"foo"}, "foo", true},
		{"explicit foo,bar matches foo", []string{"foo", "bar"}, "foo", true},
		{"explicit foo,bar matches bar", []string{"foo", "bar"}, "bar", true},
		{"explicit foo,bar denies baz", []string{"foo", "bar"}, "baz", false},
		{"explicit foo denies bar", []string{"foo"}, "bar", false},

		// Defensive: empty (non-nil) list → fail-closed, no slug
		// matches. Consent flow rejects empty allow-lists at parse
		// time, so this case shouldn't occur in production.
		{"empty list denies anything", []string{}, "foo", false},

		// Mixed wildcard + specific entries — wildcard wins. A
		// tampered token (or future feature) could have both; the
		// safer interpretation is "any" because that's what the
		// wildcard explicitly grants.
		{"wildcard + specific allows arbitrary", []string{"foo", "*"}, "anything", true},
		{"wildcard first allows arbitrary", []string{"*", "foo"}, "anything", true},

		// Edge cases.
		{"empty slug never matches explicit list", []string{"foo"}, "", false},
		{"empty slug matches wildcard", []string{"*"}, "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.allowed != nil {
				ctx = WithTokenAllowedWorkspaces(ctx, tc.allowed)
			}
			got := tokenAllowedWorkspaceMatches(ctx, tc.slug)
			if got != tc.want {
				t.Errorf("tokenAllowedWorkspaceMatches(%v, %q) = %v, want %v",
					tc.allowed, tc.slug, got, tc.want)
			}
		})
	}
}

// TestWithTokenAllowedWorkspaces_DefensiveCopy verifies the helper
// copies the input slice so a caller mutating after the call doesn't
// corrupt the per-request token state. This is the canonical
// defense-in-depth pattern for context values that callers might
// reuse across requests.
func TestWithTokenAllowedWorkspaces_DefensiveCopy(t *testing.T) {
	original := []string{"foo", "bar"}
	ctx := WithTokenAllowedWorkspaces(context.Background(), original)

	// Mutate the original after stashing.
	original[0] = "MUTATED"

	got := TokenAllowedWorkspacesFromContext(ctx)
	if len(got) != 2 || got[0] != "foo" || got[1] != "bar" {
		t.Errorf("context value should be insulated from caller mutation; got %v", got)
	}
}

// TestTokenAllowedWorkspacesFromContext_ReturnsCopy verifies the
// reader returns a fresh slice — a caller mutating the returned
// value must not corrupt the context-stored allow-list either.
func TestTokenAllowedWorkspacesFromContext_ReturnsCopy(t *testing.T) {
	ctx := WithTokenAllowedWorkspaces(context.Background(), []string{"foo", "bar"})

	got := TokenAllowedWorkspacesFromContext(ctx)
	got[0] = "MUTATED"

	// Re-read; the stored value must be unchanged.
	got2 := TokenAllowedWorkspacesFromContext(ctx)
	if got2[0] != "foo" {
		t.Errorf("returned slice should be a copy; got2[0] = %q after caller mutation", got2[0])
	}
}
