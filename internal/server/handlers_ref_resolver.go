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
// GET /{username}/{workspace}/ref/{REF} → 302 to the canonical item URL.
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
//
// The 302 redirect target mirrors the same path the SvelteKit page router
// would produce for a regular item link, so the post-redirect URL is
// indistinguishable from a direct in-app navigation.
func (s *Server) handleResolveCrossWorkspaceRef(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	workspaceSlug := chi.URLParam(r, "workspace")
	ref := chi.URLParam(r, "ref")

	// 1. Validate REF shape FIRST — cheap, no DB hit, and doesn't leak
	//    whether the workspace exists. A malformed REF on a real workspace
	//    looks the same as a malformed REF on a phantom workspace.
	if !refResolverRefPattern.MatchString(ref) {
		s.refResolverNotFound(w, r)
		return
	}

	// 2. Resolve the workspace through the same path as /workspaces/{slug}.
	//    Uses currentUser(r) so the ACL is consistent: members + guests with
	//    grants see the workspace; everyone else gets nil (→ 404).
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

	// 3. Resolve the ref within the workspace. We re-parse here (rather than
	//    just splitting on "-") to mirror store.parseItemRef's uppercase-
	//    prefix rule — store.GetItemByRef is keyed on the uppercase prefix
	//    so the redirect must canonicalize the URL ref the same way the
	//    storage layer does.
	prefix, number, ok := parseRefForRedirect(ref)
	if !ok {
		// Defense in depth — refResolverRefPattern already vetted the shape,
		// but parseItemRef has its own rules (uppercase-only prefix). If we
		// disagree, fall back to the same 404 path rather than 500.
		s.refResolverNotFound(w, r)
		return
	}
	item, err := s.store.GetItemByRef(ws.ID, prefix, number)
	if err != nil {
		s.refResolverNotFound(w, r)
		return
	}
	if item == nil {
		s.refResolverNotFound(w, r)
		return
	}

	// 4. ACL: if the viewer's collection visibility excludes this item's
	//    collection, treat as 404 (same shape as workspace-no-access). We
	//    inline a lightweight visibility check rather than reuse
	//    requireItemVisible because that helper assumes RequireWorkspaceAccess
	//    middleware has already populated context — this route runs outside
	//    that group.
	if !s.refResolverItemVisible(r, ws.ID, item) {
		s.refResolverNotFound(w, r)
		return
	}

	// 5. Build the canonical item URL and 302 to it. We prefer the URL-path
	//    `username` from the request (matches the incoming URL shape) over
	//    the workspace's owner username, so a user hitting /alice/ws/ref/X
	//    stays in alice's URL namespace even if the workspace was later
	//    transferred. SvelteKit's router handles the redirect target the
	//    same regardless.
	dest := "/" + username + "/" + ws.Slug + "/" + item.CollectionSlug + "/"
	if item.ItemNumber != nil && *item.ItemNumber > 0 && item.CollectionPrefix != "" {
		// Items addressed by ref (formatItemRef-style) when available — this
		// matches itemUrlId() in the frontend, so the redirect target is
		// identical to a direct in-app navigation.
		dest += item.CollectionPrefix + "-" + refItoa(*item.ItemNumber)
	} else {
		dest += item.Slug
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

// refResolverNotFound writes a 404 with no body details. Centralized so all
// failure paths produce identical responses — preventing oracle-style probes
// that compare response bodies to distinguish "workspace missing" from
// "ref missing" from "no access".
func (s *Server) refResolverNotFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "Not found")
}

// refResolverItemVisible returns true iff the current viewer can see the
// given item under their workspace role + grant set. Returns false for
// anonymous viewers on a workspace that requires auth, for guests without a
// grant on the item's collection, and for members whose collection access
// excludes the item's collection. Returns true when the viewer has full
// workspace access (admin / owner / member with "all" collection access).
//
// Mirrors requireItemVisible but operates without the RequireWorkspaceAccess
// middleware's context-populated state — this route is reachable
// unauthenticated, so we compute visibility from the user + workspace
// directly. Errors during the grant lookup fall back to "not visible" to
// keep the no-leak contract.
func (s *Server) refResolverItemVisible(r *http.Request, workspaceID string, item *models.Item) bool {
	user := currentUser(r)

	// Pre-setup mode: a fresh instance with no users yet has the whole
	// system open (matches RequireAuth's bypass). Anonymous probes are
	// equivalent to "logged in as the eventual admin" here, so the
	// visibility gate has nothing to enforce.
	if user == nil {
		count, err := s.store.UserCount()
		if err == nil && count == 0 {
			return true
		}
		// Authenticated-instance anonymous viewer: no item-read access via
		// the resolver route. Share links cover the public-read surface
		// through /s/{token}.
		return false
	}

	// Admin users see everything.
	if user.Role == "admin" {
		return true
	}

	// Owner of the workspace.
	ws, err := s.store.GetWorkspaceByID(workspaceID)
	if err != nil || ws == nil {
		return false
	}
	if ws.OwnerID == user.ID {
		return true
	}

	// Members with collection access. GetWorkspaceMember returns nil for
	// non-members; in that case we fall through to the guest grant check.
	member, _ := s.store.GetWorkspaceMember(workspaceID, user.ID)
	if member != nil {
		// "all" access — every collection is visible.
		if member.CollectionAccess == "" || member.CollectionAccess == "all" {
			return true
		}
		// Restricted access — check member_collection_access.
		colls, _ := s.store.GetMemberCollectionAccess(workspaceID, user.ID)
		for _, cID := range colls {
			if cID == item.CollectionID {
				return true
			}
		}
		// Member-with-restricted-access can also have explicit item grants.
		_, grantedItemIDs, _ := s.store.GuestVisibleResources(workspaceID, user.ID)
		for _, iID := range grantedItemIDs {
			if iID == item.ID {
				return true
			}
		}
		return false
	}

	// Guest path: full collection grants or item grants.
	fullCollIDs, grantedItemIDs, err := s.store.GuestVisibleResources(workspaceID, user.ID)
	if err != nil {
		return false
	}
	for _, cID := range fullCollIDs {
		if cID == item.CollectionID {
			return true
		}
	}
	for _, iID := range grantedItemIDs {
		if iID == item.ID {
			return true
		}
	}
	return false
}

// refItoa wraps strconv.Itoa for redirect-URL composition. Kept as a one-line
// helper so the build path through this file stays grep-able for future
// readers asking "where does the redirect target get composed".
func refItoa(n int) string {
	return strconv.Itoa(n)
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
