package server

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/go-chi/chi/v5"
)

// refResolverRefPattern matches the wiki-link ref shape: a letter-led
// alphanumeric prefix, a hyphen, and a positive integer. Mirrors the
// renderer's client-side regex so a 404 here is congruent with what the
// editor renders as a broken link. Anchored to reject ambiguous inputs
// before any DB lookup (the validator runs BEFORE workspace resolution,
// so a malformed REF can't reveal whether the workspace exists).
var refResolverRefPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*-\d+$`)

// handleResolveCrossWorkspaceRef implements IDEA-1492's resolver route.
//
//	GET /-/r/{workspace}/{REF}
//
// The `/-/r/` prefix is structurally impossible to collide with any
// page route under /{username}/{workspace}/{collection}/... because
// username and collection slugs both require a leading letter. This
// shape sidesteps Codex round-2 P1.4 (pre-existing `ref`-slugged
// collections in upgraded workspaces) without a migration.
//
// 404 cases (in order of evaluation):
//
//  1. REF doesn't match the wiki-link pattern. Rejected before the DB hit
//     so anonymous probes can't enumerate workspace existence by ref shape.
//  2. Workspace slug doesn't resolve, OR resolves but the current viewer
//     lacks access. We deliberately return 404 (not 403) — the brief calls
//     for "don't leak workspace existence", so members of a different
//     workspace see the same response as anonymous viewers.
//  3. Ref resolves to no item in the target workspace.
//  4. The item's collection isn't visible to the viewer.
//  5. The workspace's owner has no username on record — the canonical
//     redirect target requires one, and `"/" + "" + "/" + slug + …`
//     would produce a protocol-relative URL (`//slug/…`) the browser
//     interprets as a network-path reference. Returning 404 here is
//     safer than emitting a broken redirect (Codex round-2 P1.3).
func (s *Server) handleResolveCrossWorkspaceRef(w http.ResponseWriter, r *http.Request) {
	workspaceSlug := chi.URLParam(r, "workspace")
	ref := chi.URLParam(r, "ref")

	// 1. Validate REF shape FIRST — cheap, no DB hit, and doesn't leak
	//    whether the workspace exists. A malformed REF on a real workspace
	//    looks the same as a malformed REF on a phantom workspace.
	if !refResolverRefPattern.MatchString(ref) {
		s.refResolverNotFound(w, r)
		return
	}

	// 2. Resolve the workspace. Uses currentUser(r) so the ACL is
	//    consistent: members + guests with grants see the workspace;
	//    everyone else gets nil (→ 404).
	ws, err := s.resolveWorkspace(workspaceSlug, currentUser(r))
	if err != nil {
		// Internal error path — write a generic 404 rather than 500 to keep
		// the no-leak contract intact for ambiguous failures.
		s.refResolverNotFound(w, r)
		return
	}
	if ws == nil {
		s.refResolverNotFound(w, r)
		return
	}

	// 3. Resolve the ref within the workspace. parseRefForRedirect
	//    canonicalizes the prefix to uppercase so it lines up with
	//    GetItemByRef's exact-prefix path; refs that fail the stricter
	//    A-Z prefix rule still resolve via the workspace-unique number
	//    fallback inside GetItemByRef.
	prefix, number, ok := parseRefForRedirect(ref)
	if !ok {
		s.refResolverNotFound(w, r)
		return
	}
	item, err := s.store.GetItemByRef(ws.ID, prefix, number)
	if err != nil || item == nil {
		s.refResolverNotFound(w, r)
		return
	}

	// 4. ACL: replay RequireWorkspaceAccess's role-derivation logic for
	//    the viewer, then delegate to checkItemVisible — the same context-
	//    free helper the middleware-gated routes use. Drift between the
	//    resolver's ACL and the rest of the system is structurally
	//    impossible.
	visible, err := s.resolverItemVisible(r, ws, item)
	if err != nil || !visible {
		s.refResolverNotFound(w, r)
		return
	}

	// 5. Determine the username segment for the canonical redirect target.
	//    The new URL shape has no URL-path username, so we always synthesize
	//    from the workspace owner. Empty result → 404 rather than a broken
	//    `//slug/…` protocol-relative URL (Codex round-2 P1.3).
	username := s.resolverOwnerUsername(ws)
	if username == "" {
		s.refResolverNotFound(w, r)
		return
	}

	// 6. Build the canonical item URL and 302 to it. Matches itemUrlId()
	//    in the frontend so the redirect target is indistinguishable from a
	//    direct in-app navigation.
	dest := "/" + username + "/" + ws.Slug + "/" + item.CollectionSlug + "/"
	if item.ItemNumber != nil && *item.ItemNumber > 0 && item.CollectionPrefix != "" {
		dest += item.CollectionPrefix + "-" + strconv.Itoa(*item.ItemNumber)
	} else {
		dest += item.Slug
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

// refResolverNotFound writes a 404 with no body details. Centralized so all
// failure paths produce identical responses — preventing oracle-style probes
// that compare response bodies to distinguish "workspace missing" from
// "ref missing" from "no access" from "owner-username missing".
func (s *Server) refResolverNotFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "Not found")
}

// resolverItemVisible derives the viewer's workspace role outside the
// RequireWorkspaceAccess middleware path, then delegates to
// checkItemVisible. The role-derivation mirrors RequireWorkspaceAccess's
// rules byte-for-byte:
//
//   - Pre-setup mode (UserCount == 0) → implicit "owner" so the
//     route is reachable before the first admin exists.
//   - Admin user → "owner" (admin gets owner-equivalent access to every
//     workspace).
//   - Workspace owner → "owner".
//   - Member with explicit role → that role ("owner" / "editor" / "viewer").
//   - Non-member with workspace grants → "guest".
//   - Otherwise → not visible (returns false, nil — distinct from an error).
//
// The returned (false, err) pair is reserved for genuine DB errors; the
// caller still maps both to a 404 to honor the no-leak contract.
func (s *Server) resolverItemVisible(r *http.Request, ws *models.Workspace, item *models.Item) (bool, error) {
	user := currentUser(r)

	// Pre-setup mode: no users yet, the whole system is open (matches
	// RequireAuth + RequireWorkspaceAccess's fresh-install bypass). We
	// short-circuit to visible HERE rather than calling checkItemVisible
	// with a synthetic role because the round-2-extended checkItemVisible
	// honors role-only bypasses ("owner"/"editor") for tokenized access,
	// but the pre-setup path is conceptually distinct from a real
	// authenticated owner — bypassing here keeps the intent explicit.
	if user == nil {
		count, err := s.store.UserCount()
		if err != nil {
			return false, err
		}
		if count == 0 {
			return true, nil
		}
		// Authenticated-instance anonymous viewer: no item-read access via
		// the resolver. Share links own the public-read surface via
		// /s/{token}.
		return false, nil
	}

	// Derive the role the same way RequireWorkspaceAccess does.
	role := s.resolverWorkspaceRole(ws, user)
	if role == "" {
		// Not a member, no grants, not admin/owner. Not visible.
		return false, nil
	}
	return s.checkItemVisible(ws.ID, item, user, role)
}

// resolverWorkspaceRole reproduces RequireWorkspaceAccess's role lookup
// for a (workspace, user) pair without the *http.Request scaffolding.
// Returns "" when the user has no role and no grants in the workspace.
// The "owner" return value covers admins (admin gets owner-equivalent
// access) AND the actual workspace owner — checkItemVisible treats
// "owner" uniformly so the conflation is safe.
func (s *Server) resolverWorkspaceRole(ws *models.Workspace, user *models.User) string {
	if user.Role == "admin" || ws.OwnerID == user.ID {
		return "owner"
	}
	member, err := s.store.GetWorkspaceMember(ws.ID, user.ID)
	if err == nil && member != nil {
		return member.Role
	}
	// Not a member — guest path requires at least one grant.
	hasGrants, err := s.store.UserHasGrantsInWorkspace(ws.ID, user.ID)
	if err == nil && hasGrants {
		return "guest"
	}
	return ""
}

// resolverOwnerUsername returns the workspace owner's username — the
// only source for the leading path segment of the canonical redirect
// target (since `/-/r/{workspace}/{ref}` URLs carry no username
// themselves). Returns "" when the owner record is missing or has no
// username on file. Callers MUST treat empty as "can't build a valid
// redirect" and 404 — emitting `/` + "" + `/slug/...` yields a
// protocol-relative URL the browser interprets as a network-path
// reference (Codex round-2 P1.3).
func (s *Server) resolverOwnerUsername(ws *models.Workspace) string {
	if ws.OwnerUsername != "" {
		return ws.OwnerUsername
	}
	if ws.OwnerID == "" {
		return ""
	}
	user, err := s.store.GetUser(ws.OwnerID)
	if err != nil || user == nil {
		return ""
	}
	return user.Username
}

// parseRefForRedirect splits a validated ref (the regex caller already
// confirmed `[A-Za-z][A-Za-z0-9]*-\d+`) into its uppercase prefix and
// number. GetItemByRef's primary path matches on exact prefix; its
// fallback path (workspace-unique number alone) handles items that have
// been moved to a different collection, so a digit-bearing prefix that
// doesn't match store.parseItemRef's stricter A-Z rule still resolves via
// the number lookup.
func parseRefForRedirect(s string) (string, int, bool) {
	up := strings.ToUpper(s)
	dash := strings.LastIndex(up, "-")
	if dash <= 0 || dash == len(up)-1 {
		return "", 0, false
	}
	prefix := up[:dash]
	num, err := strconv.Atoi(up[dash+1:])
	if err != nil || num <= 0 {
		return "", 0, false
	}
	return prefix, num, true
}
