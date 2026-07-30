package server

import (
	"errors"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Cross-workspace authorization (PLAN-2357 / TASK-2358, DR-10).
//
// ############################################################
// # requireEditPermission MUST NEVER be called with a         #
// # workspace ID other than the one the current request's     #
// # URL resolved to. Neither must requireItemVisible,         #
// # requireMinRole, requireRole, or anything else that reads  #
// # workspaceRole(r).                                         #
// ############################################################
//
// requireEditPermission (server.go) accepts `workspaceID` as a
// parameter, so it LOOKS reusable for a second workspace. It is not.
// Its editor/owner fast path is `role := workspaceRole(r)` — and
// RequireWorkspaceAccess (middleware_auth.go) only ever populates
// that context value for the workspace named in the request URL.
// Hand it workspace B's ID and it happily applies the caller's
// workspace A role to workspace B. An editor in A with no membership
// at all in B sails straight through the fast path. That is
// privilege escalation, not a rough edge.
//
// The second half of the same trap: the OAuth/MCP consent allow-list
// is applied AUTOMATICALLY in exactly one place —
// RequireWorkspaceAccess's call to tokenAllowedWorkspaceMatches
// (middleware_auth.go), on the workspace in the URL. Every other
// surface that touches a workspace the URL didn't name has to call it
// by hand, and the ones that do (handlers_search.go's cross-workspace
// search, handlers_workspaces.go's restore, handlers_collab.go's
// item-ID-addressed WebSocket upgrade) are the precedent. Miss it and
// a token the user consented to workspace A alone reads and writes
// workspace B unchecked. The comparison is always against the SECOND
// workspace's CANONICAL slug — consent persists slugs, and the caller
// may have addressed the workspace by UUID.
//
// Everything in this file exists so callers never have to remember
// either of those. Use AuthorizeCrossWorkspaceRead /
// AuthorizeCrossWorkspaceEdit for any workspace that is not the
// request's own.
//
// The closest prior art is Store.GetCrossWorkspaceBacklinks
// (internal/store/wiki_links.go), which hand-rolls the same two
// things: the bearer-vs-cookie admin split (BUG-1616/1617) and the
// manual allow-list test.
//
// handlers_ref_resolver.go is NOT a template. It replays the
// membership logic but skips the allow-list entirely, and it is
// bearer-reachable (the route sits inside the full middleware stack,
// so PATs and CLI session bearers hit it) — so its consent gap is a
// real one, mitigated only by the route disclosing nothing but a
// redirect. Do not copy it for anything that returns data.

// CrossWorkspaceDenialReason explains why an authorization attempt
// against a second workspace failed. The empty value means "allowed".
//
// The reason exists so the CALLER decides the HTTP shape, deliberately
// — see the disclosure note on CrossWorkspaceAccess. Do not derive a
// status code from it ad hoc; use one of the Write* helpers, or add a
// new one here so the posture stays reviewable in one place.
type CrossWorkspaceDenialReason string

const (
	// CrossWorkspaceAllowed is the zero value: no denial.
	CrossWorkspaceAllowed CrossWorkspaceDenialReason = ""

	// CrossWorkspaceInvalidScope means the caller handed in a scope
	// that names nothing (the zero CrossWorkspaceScope). Programmer
	// error; treated as a denial so the failure mode is closed rather
	// than "workspace-level check only".
	CrossWorkspaceInvalidScope CrossWorkspaceDenialReason = "invalid_scope"

	// CrossWorkspaceWorkspaceNotFound means the slug/ID resolved to
	// nothing the caller may address. Soft-deleted workspaces land
	// here too — every resolveWorkspace path filters
	// `deleted_at IS NULL`.
	CrossWorkspaceWorkspaceNotFound CrossWorkspaceDenialReason = "workspace_not_found"

	// CrossWorkspaceNoWorkspaceAccess means the workspace exists but
	// the caller has neither membership nor grants in it.
	CrossWorkspaceNoWorkspaceAccess CrossWorkspaceDenialReason = "no_workspace_access"

	// CrossWorkspaceTokenNotAllowed means the caller is a genuine
	// member of the target workspace, but the bearer token's consent
	// allow-list does not include it. This is the denial a naive
	// implementation misses.
	CrossWorkspaceTokenNotAllowed CrossWorkspaceDenialReason = "token_not_allowed"

	// CrossWorkspaceScopeMismatch means the scope's item or collection
	// does not belong to the resolved workspace. Confused-deputy
	// guard; always fail closed.
	CrossWorkspaceScopeMismatch CrossWorkspaceDenialReason = "scope_mismatch"

	// CrossWorkspaceCollectionNotVisible means the scope's collection
	// is absent, soft-deleted, or hidden from the caller (DR-10a step
	// 1).
	CrossWorkspaceCollectionNotVisible CrossWorkspaceDenialReason = "collection_not_visible"

	// CrossWorkspaceItemNotVisible means the scope's item is hidden
	// from the caller (DR-10b step 1).
	CrossWorkspaceItemNotVisible CrossWorkspaceDenialReason = "item_not_visible"

	// CrossWorkspaceInsufficientPermission means the caller can see
	// the scoped resource but may not write it (DR-10a step 2 /
	// DR-10b step 2).
	CrossWorkspaceInsufficientPermission CrossWorkspaceDenialReason = "insufficient_permission"

	// CrossWorkspaceLookupFailed means a store lookup errored. Always
	// a denial — fail closed.
	CrossWorkspaceLookupFailed CrossWorkspaceDenialReason = "lookup_failed"
)

// CrossWorkspaceAccess is the verdict from AuthorizeCrossWorkspaceRead
// / AuthorizeCrossWorkspaceEdit.
//
// Disclosure posture (PLAN-2357). A caller with no access to workspace
// B must not be able to tell "B does not exist" from "B exists and you
// cannot see it". That is why Reason distinguishes them but the
// response writers do not: WriteHidden collapses every denial to an
// identical 404, and WriteDenied collapses absence and forbidden-ness
// to an identical 403. Read paths should use WriteHidden. Reason is
// for logs, metrics and tests — resist the urge to branch a response
// body on it.
//
// NEVER PUT THIS STRUCT IN A RESPONSE. On a denial it holds exactly
// the things the disclosure rule forbids surfacing: a resolved
// Workspace (with its ID, slug and name), a Role, and a Reason that
// separates "absent" from "forbidden". Every field carries `json:"-"`
// so a stray json.Marshal emits `{}` rather than the leak, but the tag
// is a backstop, not permission — render denials through WriteHidden
// or WriteDenied, and keep Workspace/Role/Reason for server-side logs.
type CrossWorkspaceAccess struct {
	// Allowed is the only field a caller should gate behavior on.
	Allowed bool `json:"-"`

	// Reason is CrossWorkspaceAllowed when Allowed, otherwise the
	// specific denial. See the disclosure note above before using it
	// to shape a response.
	Reason CrossWorkspaceDenialReason `json:"-"`

	// Workspace is the resolved target workspace, or nil when
	// resolution itself failed. Populated even on later denials so
	// callers can log the canonical slug — server-side only.
	Workspace *models.Workspace `json:"-"`

	// Role is the caller's effective role in the TARGET workspace
	// ("owner" / "editor" / "viewer" / "guest"), derived fresh from
	// membership + grants — never from workspaceRole(r). Empty when
	// the caller has no role there.
	Role string `json:"-"`

	// Err carries the underlying store error for
	// CrossWorkspaceLookupFailed. Nil otherwise.
	Err error `json:"-"`
}

// WorkspaceID is a nil-safe accessor for the resolved workspace's ID.
func (a CrossWorkspaceAccess) WorkspaceID() string {
	if a.Workspace == nil {
		return ""
	}
	return a.Workspace.ID
}

// WorkspaceSlug is a nil-safe accessor for the resolved workspace's
// canonical slug — the value the token allow-list was tested against.
func (a CrossWorkspaceAccess) WorkspaceSlug() string {
	if a.Workspace == nil {
		return ""
	}
	return a.Workspace.Slug
}

// WriteHidden writes the non-disclosing denial: an identical 404 for
// every failure mode, so absence and forbidden-ness are
// indistinguishable. This is the default posture for read paths —
// notably the forward-pointer gate, where even a differently-shaped
// response reveals that a destination exists.
//
// subject names the thing the caller asked for ("Item", "Workspace",
// …) and MUST NOT be derived from the target workspace's state; pass a
// constant.
//
// Internal errors are reported as 404 too, matching
// refResolverNotFound: a 500 here would be a usable oracle only in
// contrived cases, but the contract is cheaper to keep total. The
// error is still returned in Err for the caller to log.
func (a CrossWorkspaceAccess) WriteHidden(w http.ResponseWriter, subject string) {
	if subject == "" {
		subject = "Resource"
	}
	writeError(w, http.StatusNotFound, "not_found", subject+" not found")
}

// WriteDenied writes the acknowledging denial, for paths where the
// caller supplied the target slug themselves and a bare 404 would be
// unhelpful — the copy endpoint, primarily.
//
// It still refuses to distinguish "workspace absent" from "workspace
// forbidden": both are 403 with the same message. The only branch is
// the token allow-list, which reports itself because (a) it is the
// caller's OWN consent scope, not information about the target, (b)
// the middleware's primary path already emits exactly that message,
// and (c) it is only ever reported to a caller who genuinely has a
// role in the target — the authorize helpers rank
// CrossWorkspaceNoWorkspaceAccess ahead of it precisely so that
// message can never confirm the existence of a workspace the caller
// is a stranger to.
//
// Lookup failures are a 500 here rather than another 403. That is an
// observable difference, and it can only occur once resolution has
// already succeeded — so it does weakly imply the target exists. Two
// reasons it stays: an attacker cannot induce a store failure on
// demand, and silently laundering 500s into 403s would make a broken
// database look like a permissions problem. It is a real trade, so
// treat it as a PRECONDITION of choosing this writer: only call
// WriteDenied on a path where the caller supplied the target
// themselves. Everywhere else — anything a caller could use to probe
// for workspaces — use WriteHidden, which has no such variant.
func (a CrossWorkspaceAccess) WriteDenied(w http.ResponseWriter) {
	switch a.Reason {
	case CrossWorkspaceLookupFailed:
		writeInternalError(w, a.Err)
	case CrossWorkspaceTokenNotAllowed:
		writeError(w, http.StatusForbidden, "permission_denied",
			"Token is not authorized for this workspace")
	default:
		writeError(w, http.StatusForbidden, "forbidden",
			"You do not have access to the requested workspace")
	}
}

// CrossWorkspaceScope names WHAT is being reached in the target
// workspace. It is a required argument, and its zero value is invalid.
//
// This is deliberate. Pad's role checks answer a WORKSPACE-level
// question, but almost every real authorization question is
// collection- or item-level (PLAN-2357 hit the same trap three times:
// DR-10a on the destination, DR-10b on the source, and again on the
// forward-pointer read). Requiring an explicit scope means a caller
// cannot fall into a workspace-level-only check by omission — they
// have to write CrossWorkspaceWorkspaceOnlyScope() and read its
// warning first.
//
// Construct with CrossWorkspaceItemScope, CrossWorkspaceCollectionScope
// or CrossWorkspaceWorkspaceOnlyScope.
type CrossWorkspaceScope struct {
	item               *models.Item
	collectionID       string
	workspaceLevelOnly bool
}

// CrossWorkspaceItemScope scopes the check to one already-loaded item.
// Collection visibility is checked first and item-level grants second,
// in that order, by checkItemVisible — the same rule set the
// middleware-gated handlers use.
//
// Pass the item you already resolved; the helper deliberately does not
// re-resolve it, so it cannot accidentally apply a different lookup
// path than the caller did. It MUST be a fully populated item — ID,
// WorkspaceID and CollectionID are all required and are checked
// against the resolved target workspace. Soft-deleted (archived) items
// are permitted: reading an archived source item is the point of the
// forward pointer.
func CrossWorkspaceItemScope(item *models.Item) CrossWorkspaceScope {
	return CrossWorkspaceScope{item: item}
}

// CrossWorkspaceCollectionScope scopes the check to one collection —
// creating into it, or disclosing its schema.
//
// Visibility here is FULL-collection-access semantics, deliberately
// stricter than the nav-lenient VisibleCollectionIDs set: a caller
// whose only claim on the collection is an item-level grant inside it
// does not qualify. That is the DR-10a hole — VisibleCollectionIDs
// folds item-grant collections in "so the collection appears in
// navigation", and ResolveUserPermission's own restricted-member gate
// consults that same lenient set, so a restricted editor with one
// stray item grant would otherwise be cleared to create into a
// collection deliberately hidden from them.
func CrossWorkspaceCollectionScope(collectionID string) CrossWorkspaceScope {
	return CrossWorkspaceScope{collectionID: collectionID}
}

// CrossWorkspaceWorkspaceOnlyScope requests a WORKSPACE-LEVEL CHECK
// ONLY, which is almost never sufficient on its own.
//
// It answers "does this caller have any role in workspace B, and does
// their token allow B" — nothing more. It does NOT answer whether they
// may see or write any particular collection or item there. A
// restricted member, or a guest holding one unrelated item grant, will
// pass this and still have no right to touch the resource you are
// about to touch.
//
// Legitimate uses are narrow: an early reject before the collection or
// item is known, or a check whose resource is genuinely the workspace
// itself. If you are about to read or write a collection or an item,
// you want CrossWorkspaceCollectionScope or CrossWorkspaceItemScope
// instead — and passing this and stopping is the exact mistake DR-10a,
// DR-10b and the forward-pointer leak all describe.
func CrossWorkspaceWorkspaceOnlyScope() CrossWorkspaceScope {
	return CrossWorkspaceScope{workspaceLevelOnly: true}
}

func (sc CrossWorkspaceScope) valid() bool {
	switch {
	case sc.workspaceLevelOnly:
		return sc.item == nil && sc.collectionID == ""
	case sc.item != nil:
		return sc.collectionID == ""
	case sc.collectionID != "":
		return true
	default:
		return false
	}
}

// itemID / collectionID feed ResolveUserPermission's resolution order.
func (sc CrossWorkspaceScope) itemID() string {
	if sc.item == nil {
		return ""
	}
	return sc.item.ID
}

func (sc CrossWorkspaceScope) permCollectionID() string {
	if sc.item != nil {
		return sc.item.CollectionID
	}
	return sc.collectionID
}

// AuthorizeCrossWorkspaceRead decides whether the caller may READ the
// scoped resource in a workspace OTHER than the request's own.
//
// workspaceSlugOrID may be either form; it is resolved the same way
// RequireWorkspaceAccess resolves the URL slug, and the token
// allow-list is then tested against the resolved CANONICAL slug (the
// supplied value may have been a UUID).
//
// Order of evaluation, all of it fail-closed:
//
//  1. scope is well-formed;
//  2. workspace resolves and is not soft-deleted;
//  3. caller's role in THAT workspace, from membership and grants,
//     honoring the bearer-vs-cookie admin split (BUG-1616/1617);
//  4. token consent allow-list against that workspace's slug;
//  5. the scoped visibility check — collection-then-item for an item
//     scope, full-collection-access for a collection scope.
//
// NOT ATOMIC. The verdict describes the world at the instant it was
// computed, against the item you handed in. Nothing here takes a lock
// or a transaction, and every input can change underneath you: the
// item can be moved into a hidden collection or archived, the
// collection can be soft-deleted, the caller's membership or grants
// can be revoked, the workspace itself can be soft-deleted. A MUTATING
// caller must re-read both sides inside its write transaction and
// re-apply the check there (PLAN-2357 DR-9 requires exactly that of
// the copy path; see UpdateItemWithPreCheck / MoveItemWithPreCheck for
// the established shape). For a read path a stale verdict is a much
// smaller problem, but do not cache one across requests.
//
// See the file header for why requireEditPermission and friends cannot
// be used here.
func (s *Server) AuthorizeCrossWorkspaceRead(r *http.Request, workspaceSlugOrID string, scope CrossWorkspaceScope) CrossWorkspaceAccess {
	return s.authorizeCrossWorkspace(r, workspaceSlugOrID, scope, false)
}

// AuthorizeCrossWorkspaceEdit decides whether the caller may WRITE the
// scoped resource in a workspace OTHER than the request's own.
//
// It runs every step AuthorizeCrossWorkspaceRead runs, in the same
// order, and only then evaluates write permission — visibility first,
// permission second (DR-10a / DR-10b). Never reorder those: the
// permission check alone clears a restricted member for a collection
// they were never allowed to see.
//
// The four-step ordering DR-10b specifies for a cross-workspace copy
// composes out of two calls — and the SOURCE verdict must be handled
// before the destination is touched at all:
//
//	src := s.AuthorizeCrossWorkspaceEdit(r, srcWS, CrossWorkspaceItemScope(item))
//	if !src.Allowed {
//	    src.WriteHidden(w, "Item")
//	    return
//	}
//	dst := s.AuthorizeCrossWorkspaceEdit(r, dstWS, CrossWorkspaceCollectionScope(collID))
//	if !dst.Allowed {
//	    dst.WriteDenied(w)
//	    return
//	}
//
// which is source-item-visible → source-edit → destination-collection-
// visible → destination-collection-edit. The early return is part of
// the ordering, not style: evaluating the destination after a failed
// source check builds a verdict about workspace B for a caller who was
// not even allowed to read the source, and anything that verdict
// reaches — a log line, an error message, a timing difference — is a
// disclosure the source check was supposed to have prevented.
//
// Both calls apply to a dry-run as much as to the mutation; a preflight
// that confirms a hidden item exists, or echoes a hidden collection's
// schema, is itself the leak.
//
// NOT ATOMIC — see AuthorizeCrossWorkspaceRead. This is a PRE-check,
// not a write barrier. Treat an allow as "proceed to the transaction",
// never as "the write is now authorized": between this call and the
// insert, the destination collection can be soft-deleted (CreateItem
// does not reject that), the source item can be moved or archived, and
// the caller's membership or grants can be revoked. Re-read and
// re-check inside the write transaction.
//
// See the file header for why requireEditPermission cannot be used
// here.
func (s *Server) AuthorizeCrossWorkspaceEdit(r *http.Request, workspaceSlugOrID string, scope CrossWorkspaceScope) CrossWorkspaceAccess {
	return s.authorizeCrossWorkspace(r, workspaceSlugOrID, scope, true)
}

func crossWorkspaceDeny(reason CrossWorkspaceDenialReason, ws *models.Workspace, role string, err error) CrossWorkspaceAccess {
	return CrossWorkspaceAccess{Reason: reason, Workspace: ws, Role: role, Err: err}
}

func (s *Server) authorizeCrossWorkspace(r *http.Request, workspaceSlugOrID string, scope CrossWorkspaceScope, needEdit bool) CrossWorkspaceAccess {
	// 1. Scope well-formedness. A zero CrossWorkspaceScope names
	//    nothing; refuse rather than silently degrade to a
	//    workspace-level check.
	if !scope.valid() {
		return crossWorkspaceDeny(CrossWorkspaceInvalidScope, nil, "",
			errors.New("cross-workspace authorization called with an empty or ambiguous scope"))
	}
	if workspaceSlugOrID == "" {
		return crossWorkspaceDeny(CrossWorkspaceWorkspaceNotFound, nil, "", nil)
	}

	user := currentUser(r)
	isBearer := isBearerAuth(r)

	// 2. Resolve the target. resolveWorkspace applies the same ACL
	//    scoping RequireWorkspaceAccess applies to the URL slug, and
	//    every path it takes (GetWorkspaceByID, GetWorkspaceBySlug,
	//    GetWorkspacesBySlugForUser) filters `deleted_at IS NULL`, so a
	//    soft-deleted target resolves to nil → denied. Note the
	//    UUID branch is NOT ACL-scoped; the role check below is what
	//    actually authorizes, exactly as in the middleware.
	ws, err := s.resolveWorkspace(workspaceSlugOrID, user)
	if err != nil {
		return crossWorkspaceDeny(CrossWorkspaceLookupFailed, nil, "", err)
	}
	if ws == nil {
		return crossWorkspaceDeny(CrossWorkspaceWorkspaceNotFound, nil, "", nil)
	}

	// 3. Role in the TARGET workspace, computed fresh. Never
	//    workspaceRole(r) — that is workspace A's answer.
	role, err := s.crossWorkspaceRole(r, ws, user, isBearer)
	if err != nil {
		return crossWorkspaceDeny(CrossWorkspaceLookupFailed, ws, "", err)
	}

	// 4. Token consent allow-list, against the CANONICAL slug.
	//
	//    Evaluated here but reported AFTER the role check, so a
	//    "token not authorized" response can only ever reach a caller
	//    who actually has a role in the target. Otherwise a
	//    bearer-borne platform admin — for whom resolveWorkspace does a
	//    global slug lookup — could use the distinct message to
	//    confirm the existence of a workspace they never joined.
	tokenAllowed := tokenAllowedWorkspaceMatches(r.Context(), ws.Slug)

	if role == "" {
		s.recordMCPAuthzDenial(r, "not_a_member")
		return crossWorkspaceDeny(CrossWorkspaceNoWorkspaceAccess, ws, "", nil)
	}
	if !tokenAllowed {
		s.recordMCPAuthzDenial(r, "workspace_not_in_allowlist")
		return crossWorkspaceDeny(CrossWorkspaceTokenNotAllowed, ws, role, nil)
	}

	// 5. Scoped visibility. Workspace-level rights are not sufficient
	//    and this is where that is enforced.
	switch {
	case scope.item != nil:
		item := scope.item
		// Confused-deputy guard: the item must actually live in the
		// workspace we just authorized, and it must be fully
		// identified. Without this a caller could pass workspace B
		// (which they can write) alongside an item from workspace C
		// — and a sparse or hand-built models.Item with an empty
		// WorkspaceID or CollectionID would sail through both the
		// visibility filter (isCollectionVisible short-circuits on an
		// unrestricted caller) and ResolveUserPermission (which falls
		// back to the workspace membership role when collectionID is
		// empty). Every field is required; no "unset means it's fine"
		// branch (Codex round 1 P1).
		if item.ID == "" || item.WorkspaceID != ws.ID || item.CollectionID == "" {
			return crossWorkspaceDeny(CrossWorkspaceScopeMismatch, ws, role, nil)
		}
		// The item's collection must exist, live in the same
		// workspace, and not be archived. checkItemVisible does NOT
		// establish any of that — soft-deleting a collection leaves
		// its items in place and neither GetItem nor
		// VisibleCollectionIDs filters on the collection's deleted_at,
		// so an item under an archived collection would otherwise pass
		// (Codex round 2 P1).
		//
		// Only the caller-independent facts, though: the item scope
		// deliberately does NOT apply the collection scope's
		// full-collection-access rule. A guest whose sole claim is an
		// item grant SHOULD be able to read that one item — that is
		// what item grants mean — while still being barred from
		// operating on the collection as a whole. checkItemVisible
		// below is what draws that line.
		if _, deny, ok := s.crossWorkspaceLiveCollection(ws, role, item.CollectionID); !ok {
			return deny
		}
		visible, vErr := s.checkItemVisible(ws.ID, item, user, role, isBearer)
		if vErr != nil {
			return crossWorkspaceDeny(CrossWorkspaceLookupFailed, ws, role, vErr)
		}
		if !visible {
			return crossWorkspaceDeny(CrossWorkspaceItemNotVisible, ws, role, nil)
		}

	case scope.collectionID != "":
		coll, deny, ok := s.crossWorkspaceLiveCollection(ws, role, scope.collectionID)
		if !ok {
			return deny
		}
		visible, vErr := s.checkCollectionFullyVisible(r, ws.ID, coll.ID)
		if vErr != nil {
			return crossWorkspaceDeny(CrossWorkspaceLookupFailed, ws, role, vErr)
		}
		if !visible {
			return crossWorkspaceDeny(CrossWorkspaceCollectionNotVisible, ws, role, nil)
		}
	}

	if !needEdit {
		return CrossWorkspaceAccess{Allowed: true, Workspace: ws, Role: role}
	}

	// 6. Write permission, only after visibility passed.
	allowed, pErr := s.crossWorkspaceEditAllowed(ws, user, isBearer, role, scope)
	if pErr != nil {
		return crossWorkspaceDeny(CrossWorkspaceLookupFailed, ws, role, pErr)
	}
	if !allowed {
		return crossWorkspaceDeny(CrossWorkspaceInsufficientPermission, ws, role, nil)
	}
	return CrossWorkspaceAccess{Allowed: true, Workspace: ws, Role: role}
}

// crossWorkspaceLiveCollection loads the scope's collection and
// establishes the caller-independent facts: it exists, it is not
// soft-deleted, and it belongs to the resolved workspace.
//
// Returns (verdict, false) on a denial and (coll, true) otherwise.
// Both scopes go through it, so neither can be satisfied by a
// collection that is archived or lives somewhere else.
func (s *Server) crossWorkspaceLiveCollection(ws *models.Workspace, role, collectionID string) (*models.Collection, CrossWorkspaceAccess, bool) {
	coll, err := s.store.GetCollection(collectionID)
	if err != nil {
		return nil, crossWorkspaceDeny(CrossWorkspaceLookupFailed, ws, role, err), false
	}
	// GetCollection filters soft-deleted rows, so nil covers "absent"
	// and "archived" alike — both are unreachable.
	if coll == nil {
		return nil, crossWorkspaceDeny(CrossWorkspaceCollectionNotVisible, ws, role, nil), false
	}
	if coll.WorkspaceID != ws.ID {
		return nil, crossWorkspaceDeny(CrossWorkspaceScopeMismatch, ws, role, nil), false
	}
	return coll, CrossWorkspaceAccess{}, true
}

// crossWorkspaceRole derives the caller's role in an arbitrary
// workspace, reproducing RequireWorkspaceAccess's rules without the
// request-scoped context value that middleware populates. Returns ""
// when the caller has no role there. Errors are real store failures —
// callers must treat them as denials.
//
// Governing rule: this must never grant MORE than the front door. If
// RequireWorkspaceAccess would 403 a caller at
// /api/v1/workspaces/{target}, reaching the same workspace sideways
// through a cross-workspace operation must fail too. The branches
// below therefore track middleware_auth.go's order exactly:
//
//  1. fresh install (UserCount == 0, no user) → "owner", because the
//     whole instance is open until the first admin exists;
//  2. legacy workspace-scoped API token (no user, token pinned to a
//     workspace) → "editor", but ONLY for the workspace it is pinned
//     to. For any other workspace the token is a stranger — the
//     correct answer for a cross-workspace reach;
//  3. platform admin over a COOKIE session → "owner" on every
//     workspace. Over any bearer surface (CLI, PAT, MCP) the bypass is
//     suppressed and the admin falls through to membership
//     (BUG-1616/1617);
//  4. membership row → its role;
//  5. bearer-borne platform admin who is not a member → "", with NO
//     grants fallback (BUG-1616/1617);
//  6. non-member holding any grant → "guest";
//  7. otherwise "".
//
// Note this deliberately does NOT reuse resolverWorkspaceRole
// (handlers_ref_resolver.go), even though the two are close. That
// helper short-circuits to "owner" on ws.OwnerID for every auth
// surface (BUG-1618, a deliberate widening for the cookie-only
// redirect route). RequireWorkspaceAccess has no such exception —
// ownership and membership are separate persisted state, and the
// middleware requires the membership row — so honoring the shortcut
// here would let a bearer token reach a workspace the front door
// refuses. Codex round 2 P1.
func (s *Server) crossWorkspaceRole(r *http.Request, ws *models.Workspace, user *models.User, isBearer bool) (string, error) {
	if user == nil {
		count, err := s.store.UserCount()
		if err != nil {
			return "", err
		}
		if count == 0 {
			return "owner", nil
		}
		if tokenWsID := tokenWorkspaceID(r); tokenWsID != "" && tokenWsID == ws.ID {
			return "editor", nil
		}
		return "", nil
	}
	if user.Role == "admin" && !isBearer {
		return "owner", nil
	}
	member, err := s.store.GetWorkspaceMember(ws.ID, user.ID)
	if err != nil {
		return "", err
	}
	if member != nil {
		return member.Role, nil
	}
	// Bearer-borne platform admin who isn't a member gets NO
	// grant-based fallback — the membership-only stance
	// (BUG-1616/1617). Without this a single stray collection or item
	// grant would yield "guest" below, and checkItemVisible's own
	// admin bypass would then widen that back out.
	if user.Role == "admin" && isBearer {
		return "", nil
	}
	hasGrants, err := s.store.UserHasGrantsInWorkspace(ws.ID, user.ID)
	if err != nil {
		return "", err
	}
	if hasGrants {
		return "guest", nil
	}
	return "", nil
}

// crossWorkspaceEditAllowed answers the write half, and only ever runs
// after the scoped visibility check has already passed.
//
// It reproduces requireEditPermission's decision — editor/owner base
// role wins, otherwise fall back to ResolveUserPermission so grants can
// override an insufficient role — with exactly two substitutions, which
// are the whole point of this file:
//
//   - `role` is derived for the TARGET workspace by crossWorkspaceRole,
//     never read from workspaceRole(r);
//   - it runs only AFTER the scoped visibility check.
//
// That ordering is what makes the base-role branch safe. DR-10a's
// escalation is the fast path used ALONE: an editor whose membership is
// collection_access="specific" passing the role check for a collection
// hidden from them. authorizeCrossWorkspace has already applied the
// collection- or item-scoped visibility filter by the time we get here,
// so the base role is being applied to a resource the caller is
// established to be able to see — the same composition handleMoveItem
// uses (requireItemVisible then requireEditPermission).
//
// Grants are treated as widening only, never narrowing. Delegating
// unconditionally to ResolveUserPermission would be tempting — it is
// the resource-scoped answer — but it resolves item and collection
// grants BEFORE membership, so an incidental `view` grant would demote
// a member whose base role is editor or owner and deny an edit the
// front door allows (Codex round 5). Fail-closed, but wrong, and
// divergence from requireEditPermission in either direction is how this
// helper eventually rots.
//
// Two bypasses ResolveUserPermission cannot express are handled here:
//
//   - tokenized nil-user roles ("owner" on a fresh install, "editor"
//     for a legacy workspace-pinned token) — crossWorkspaceRole only
//     hands those out when the caller is genuinely authorized, and
//     ResolveUserPermission has no user ID to work with;
//   - the platform-admin bypass, which is cookie-session only. A
//     bearer-borne admin falls through and is judged on their actual
//     membership.
func (s *Server) crossWorkspaceEditAllowed(ws *models.Workspace, user *models.User, isBearer bool, role string, scope CrossWorkspaceScope) (bool, error) {
	if user == nil {
		// Only reachable for the synthesized roles above; a nil user
		// with no role was already denied.
		return role == "owner" || role == "editor", nil
	}
	if user.Role == "admin" && !isBearer {
		return true, nil
	}
	// Base-role branch, mirroring requireEditPermission. "guest" is
	// excluded: a guest has no role-based permission at all, only
	// grants.
	if role != "guest" && roleLevel(role) >= roleLevel("editor") {
		return true, nil
	}
	perm, err := s.store.ResolveUserPermission(ws.ID, user.ID, scope.itemID(), scope.permCollectionID())
	if err != nil {
		return false, err
	}
	return permissionLevel(perm) >= permissionLevel("edit"), nil
}

// checkCollectionFullyVisible is the bool-returning core of
// requireCollectionFullyVisible — same rules, no response writing, and
// a workspace ID that need not be the request's own. See
// requireCollectionFullyVisible for why the full-access narrowing is
// stricter than plain visibleCollectionIDs.
//
// Both of its inputs are workspace-explicit: visibleCollectionIDs and
// guestResourceFilter read the request only for the current user and
// the bearer flag, never for a workspace-scoped context value.
func (s *Server) checkCollectionFullyVisible(r *http.Request, workspaceID, collectionID string) (bool, error) {
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		return false, err
	}
	if visibleIDs != nil {
		// Restricted caller: when any item-level grant is in play,
		// narrow from the nav-lenient set to full-access collections
		// only, so an item-grant-only collection cannot qualify for a
		// collection-wide operation.
		fullCollIDs, grantedItemIDs, gErr := s.guestResourceFilter(r, workspaceID)
		if gErr != nil {
			return false, gErr
		}
		if len(grantedItemIDs) > 0 {
			// isCollectionVisible reads nil as "unrestricted", so a
			// caller whose full-access set is genuinely empty — a guest
			// whose only claim is an item grant — must be handed an
			// empty NON-NIL slice, not nil. ResolveBacklinksVisibility
			// already guarantees that, but the guarantee lives two
			// packages away and inverting it here would silently grant
			// every collection in the workspace. Re-assert it locally
			// (Codex round 3 P1: reported as a live bug on the
			// assumption the guarantee wasn't there; it is, and
			// TestCrossWorkspace_GuestItemGrantOnly pins the behavior —
			// but the sentinel flip is too dangerous to leave implicit).
			if fullCollIDs == nil {
				fullCollIDs = []string{}
			}
			visibleIDs = fullCollIDs
		}
	}
	return isCollectionVisible(collectionID, visibleIDs), nil
}
