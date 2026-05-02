package mcp

import (
	"context"

	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// oauthWorkspaceLister implements WorkspaceLister for the remote /mcp
// transport. Bridges three concerns the privacy-preserving
// available_workspaces hint (TASK-977) needs to combine:
//
//  1. The requesting user's full workspace membership set (from the
//     store).
//  2. The OAuth token's `allowed_workspaces` allow-list (set at
//     consent time, TASK-952; stashed by MCPBearerAuth via
//     server.WithTokenAllowedWorkspaces, TASK-953).
//  3. The wildcard / specific-list / nil semantics from
//     server.TokenAllowedWorkspacesFromContext.
//
// Privacy invariant: the returned hint list MUST be a subset of the
// token's allow-list. Workspace slugs the user has access to but
// did NOT include in the consent payload MUST NOT appear, even
// when the agent triggers an unknown_workspace error. The whole
// point of the consent UI's per-workspace selection is that the
// app gets ONLY those workspaces; leaking other slugs in error
// envelopes would be a scope violation.
//
// nil allow-list (PAT auth, or pre-TASK-952 OAuth tokens) → return
// all the user's workspaces, matching pre-TASK-977 behaviour.
// Wildcard `["*"]` → same as nil (the user explicitly granted any
// workspace).
//
// Errors propagate up: storage failures yield (nil, err), which the
// upstream bestEffortWorkspaceHints helper swallows so the envelope
// still ships with empty available_workspaces rather than dropping
// the whole error path.
type oauthWorkspaceLister struct {
	// store is the pad store used to read the user's workspaces.
	// Hidden behind an interface in tests; the production wiring
	// passes *store.Store.
	store oauthWorkspaceListerStore
}

// oauthWorkspaceListerStore is the minimal interface oauthWorkspaceLister
// needs from a *store.Store. Defining it inline keeps the test surface
// tiny — tests can supply a one-method fake without spinning up a
// real DB-backed store.
type oauthWorkspaceListerStore interface {
	GetUserWorkspaces(userID string) ([]storeWorkspace, error)
}

// storeWorkspace is the tiny subset of models.Workspace this file
// needs (slug + name). Avoids leaking the full models package into
// the test fake.
type storeWorkspace = WorkspaceHint

// NewOAuthWorkspaceLister wires a lister against a *store.Store.
// Production wiring (cmd/pad/main.go) calls this; tests use the
// alternate constructor that takes an oauthWorkspaceListerStore.
func NewOAuthWorkspaceLister(s *store.Store) WorkspaceLister {
	return &oauthWorkspaceLister{store: realStoreAdapter{s}}
}

// realStoreAdapter shims *store.Store into oauthWorkspaceListerStore.
// Translates models.Workspace into the WorkspaceHint shape this
// package's lister contract uses, dropping fields the hint doesn't
// need (settings, owner, timestamps, etc.).
type realStoreAdapter struct{ s *store.Store }

func (a realStoreAdapter) GetUserWorkspaces(userID string) ([]storeWorkspace, error) {
	rows, err := a.s.GetUserWorkspaces(userID)
	if err != nil {
		return nil, err
	}
	out := make([]storeWorkspace, 0, len(rows))
	for _, ws := range rows {
		out = append(out, WorkspaceHint{Slug: ws.Slug, Name: ws.Name})
	}
	return out, nil
}

// ListWorkspaces returns the workspaces the agent is allowed to
// see in error-envelope hints, filtered by the OAuth allow-list.
//
// Behaviour by token state (read from ctx):
//
//   - No user on context → empty list. Anonymous probes against
//     /mcp shouldn't reach a tool dispatch in the first place
//     (MCPBearerAuth 401s before that), but a defensive empty
//     list keeps the helper safe to call from error paths.
//   - PAT auth (allow-list nil) → return all of the user's
//     workspaces. PATs don't have a workspace allow-list at all.
//   - Wildcard allow-list (`["*"]`) → return all of the user's
//     workspaces. The user explicitly granted "any."
//   - Specific allow-list (`["foo", "bar"]`) → intersect with the
//     user's memberships. Workspaces not in the allow-list are
//     dropped, even if the user is a member of them.
func (l *oauthWorkspaceLister) ListWorkspaces(ctx context.Context) ([]WorkspaceHint, error) {
	user, ok := server.CurrentUserFromContext(ctx)
	if !ok || user == nil {
		return nil, nil
	}
	all, err := l.store.GetUserWorkspaces(user.ID)
	if err != nil {
		return nil, err
	}
	allowed := server.TokenAllowedWorkspacesFromContext(ctx)
	if allowList := buildAllowSet(allowed); allowList != nil {
		filtered := make([]WorkspaceHint, 0, len(all))
		for _, ws := range all {
			if _, ok := allowList[ws.Slug]; ok {
				filtered = append(filtered, ws)
			}
		}
		return filtered, nil
	}
	return all, nil
}

// buildAllowSet returns nil for the "no token-level filter" cases
// (nil allow-list, or wildcard) and a slug-set otherwise. Matches
// the three return-shape contract of TokenAllowedWorkspacesFromContext.
func buildAllowSet(allowed []string) map[string]struct{} {
	if allowed == nil {
		return nil
	}
	for _, entry := range allowed {
		if entry == "*" {
			return nil
		}
	}
	out := make(map[string]struct{}, len(allowed))
	for _, slug := range allowed {
		out[slug] = struct{}{}
	}
	return out
}
