package server

import (
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// TestMergeAllowedWorkspaces covers the OR-merge policy for the
// dual-read introspection gate (PLAN-1519 / TASK-1520). The gate
// consults two sources during the migration to per-connection state:
//
//  1. session.Extra["allowed_workspaces"] — legacy per-token shape.
//  2. oauth_connections + oauth_connection_workspaces — new per-grant
//     shape projected via store.OAuthConnectionAccess.
//
// The acceptance criterion from PLAN-1519 Phase A:
//
//	> Dual-read gate verified by test: token with allow-list in
//	> session.Extra still passes; token with empty session.Extra but
//	> row in oauth_connection_workspaces also passes.
//
// We cover both directly, plus the wildcard and union edge cases that
// fall out of "allow if either source allows" semantics.
func TestMergeAllowedWorkspaces(t *testing.T) {
	cases := []struct {
		name   string
		extra  []string
		access store.OAuthConnectionAccess
		want   []string // nil = unrestricted; [] would be fail-closed
	}{
		// --- Pre-migration baseline: no connection row exists yet. ---
		{
			name:   "extra nil + no connection — unrestricted",
			extra:  nil,
			access: store.OAuthConnectionAccess{HasConnection: false},
			want:   nil,
		},
		{
			name:   "extra wildcard + no connection — unrestricted",
			extra:  []string{"*"},
			access: store.OAuthConnectionAccess{HasConnection: false},
			want:   nil,
		},
		{
			name:   "extra explicit list + no connection — Extra path covers it",
			extra:  []string{"docapp", "ws-2"},
			access: store.OAuthConnectionAccess{HasConnection: false},
			want:   []string{"docapp", "ws-2"},
		},

		// --- Post-migration: connection row exists with wildcard flag. ---
		{
			name:   "extra nil + connection wildcard — unrestricted",
			extra:  nil,
			access: store.OAuthConnectionAccess{HasConnection: true, AllCurrentWorkspaces: true},
			want:   nil,
		},
		{
			name:   "extra explicit + connection wildcard — wildcard wins",
			extra:  []string{"docapp"},
			access: store.OAuthConnectionAccess{HasConnection: true, AllCurrentWorkspaces: true},
			want:   nil,
		},

		// --- Post-migration: connection row with explicit allow-list. ---
		// This is the headline acceptance criterion: empty session.Extra
		// + row in oauth_connection_workspaces must still pass.
		{
			name:   "extra nil + connection explicit — new path covers it",
			extra:  nil,
			access: store.OAuthConnectionAccess{HasConnection: true, WorkspaceSlugs: []string{"docapp"}},
			want:   []string{"docapp"},
		},
		{
			name:   "extra explicit + connection explicit — union",
			extra:  []string{"docapp", "ws-2"},
			access: store.OAuthConnectionAccess{HasConnection: true, WorkspaceSlugs: []string{"ws-2", "ws-3"}},
			want:   []string{"docapp", "ws-2", "ws-3"},
		},
		{
			name:   "extra wildcard + connection explicit — wildcard wins",
			extra:  []string{"*"},
			access: store.OAuthConnectionAccess{HasConnection: true, WorkspaceSlugs: []string{"docapp"}},
			want:   nil,
		},

		// --- Fail-closed edge: connection scoped to nothing. ---
		// Empty non-nil slice on the new-path side AND empty/nil Extra
		// → no slugs in the union. mergeAllowedWorkspaces returns a
		// non-nil empty slice so downstream sees "consent existed but
		// scoped to nothing" rather than "no gate." MCPBearerAuth would
		// then stash an empty allow-list, denying every workspace —
		// matching the legacy token_workspace_allowlist_test.go policy
		// where `[]` denies anything.
		{
			name:   "extra nil + connection scoped to empty — fail-closed",
			extra:  nil,
			access: store.OAuthConnectionAccess{HasConnection: true, WorkspaceSlugs: []string{}},
			want:   []string{},
		},

		// --- Dedup: same slug on both sides counts once. ---
		{
			name:   "duplicate slug across sources — deduped",
			extra:  []string{"docapp"},
			access: store.OAuthConnectionAccess{HasConnection: true, WorkspaceSlugs: []string{"docapp"}},
			want:   []string{"docapp"},
		},

		// --- "*" on the Extra side mid-list still wins (defensive). ---
		{
			name:   "extra mixed wildcard mid-list — wildcard wins",
			extra:  []string{"docapp", "*"},
			access: store.OAuthConnectionAccess{HasConnection: true, WorkspaceSlugs: []string{"ws-2"}},
			want:   nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeAllowedWorkspaces(tc.extra, tc.access)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("mergeAllowedWorkspaces(%v, %+v) = %v (nil=%v), want %v (nil=%v)",
					tc.extra, tc.access, got, got == nil, tc.want, tc.want == nil)
			}
		})
	}
}

// BenchmarkMergeAllowedWorkspaces measures the per-call overhead of
// the policy function on the introspection hot path. The store-side
// lookup is a separate cost (one PK + one indexed join when the
// wildcard short-circuit doesn't fire); this benchmark just covers
// the in-memory merge once the projection lands.
//
// PLAN-1519 Phase A acceptance bullet: "Hot-path benchmark: dual-read
// overhead measured and documented." Run with:
//
//	go test -bench BenchmarkMergeAllowedWorkspaces -benchmem ./internal/server/
//
// Expected order-of-magnitude: hundreds of nanoseconds per call (small
// fixed-size slices, no I/O). If this ever climbs into microseconds
// the union allocation is the place to look.
func BenchmarkMergeAllowedWorkspaces(b *testing.B) {
	extra := []string{"docapp", "ws-2", "ws-3"}
	access := store.OAuthConnectionAccess{
		HasConnection:  true,
		WorkspaceSlugs: []string{"ws-2", "ws-3", "ws-4"},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = mergeAllowedWorkspaces(extra, access)
	}
}
