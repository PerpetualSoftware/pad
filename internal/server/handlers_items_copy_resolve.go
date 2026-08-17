package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// authorizedCopy is everything a cross-workspace copy request resolves to
// once it has passed resolution and the full authorization ladder. It is the
// single output of resolveAuthorizedCopy and the single input to both
// consumers — the dry run (handleCopyItemPreflight) and the mutation
// (handleCopyItem).
//
// NEVER PUT THIS STRUCT IN A RESPONSE. It embeds two CrossWorkspaceAccess
// verdicts, which carry the resolved workspace, role and denial reason the
// disclosure rules forbid surfacing. See CrossWorkspaceAccess's own note.
type authorizedCopy struct {
	// sourceWorkspaceID is the request's own workspace, resolved by
	// getWorkspaceID — i.e. the workspace in the URL, not the destination.
	sourceWorkspaceID string

	// item is the resolved, visible, editable source item.
	item *models.Item

	// input is the decoded request body. Both endpoints take the same
	// shape, which is why itemCopyPreflightRequest names both.
	input itemCopyPreflightRequest

	// source / destination are the two authorization verdicts, both
	// Allowed. destination is COLLECTION-scoped (the fourth check), so it
	// is the verdict WriteCollectionNotFound was built for.
	source      CrossWorkspaceAccess
	destination CrossWorkspaceAccess

	// targetCollection is the resolved destination collection.
	targetCollection *models.Collection

	// attachmentAuth is the per-attachment visibility check both consumers
	// hand to the planner (TASK-2408). It lives HERE, on the shared
	// resolution, for the same reason the ladder above does: the preflight
	// and the copy must decide "which references are unresolvable" with one
	// function, or the preview stops predicting the copy. Never nil.
	attachmentAuth store.AttachmentAuthorizer

	// actorID is who the copy is attributed to — see copyActorID. It is
	// NOT the actor/source audit pair from actorFromRequest, which is a
	// different thing entirely and belongs only to the mutating path.
	actorID string
}

// resolveAuthorizedCopy performs the resolution and the four-step
// authorization ladder shared by the copy preflight and the copy itself.
//
// It returns ok=false when the request was refused; the response has ALREADY
// been written in that case and the caller must simply return.
//
// WHY THIS EXISTS. Both endpoints ran this sequence statement for statement,
// and the preflight/copy divergence class is empirically the number one
// defect source in PLAN-2357 — five separate divergences were found and fixed
// in Phase 2 alone, one of which was a disagreement about the ORDER of
// refusals rather than the set. The ordering below is therefore not an
// implementation detail of either handler; it is the contract, expressed
// once. TestCopyAuthorizationParityMatrix pins the observable half of that
// contract as an executable claim — if the two endpoints ever answer a
// refusal differently, or in a different order, it fails. Keeping the
// sequence in ONE place is what makes that hard to break in the first
// place; the test is the backstop, not the guarantee.
//
// THE ORDERING, and why each step sits where it does:
//
//  1. Source workspace + item resolution, BEFORE the body is decoded.
//     Inverting this leaks source existence: a caller who cannot see the item
//     would learn whether their JSON was well-formed, and a caller who could
//     see it would not. writeItemResolveError keeps its 404-vs-409 split —
//     409 archived only when the caller can independently SEE the archived
//     row, 404 otherwise.
//
//  2. Checks 1 and 2 (source item visibility, then source edit), composed out
//     of one AuthorizeCrossWorkspaceEdit call with an item scope. The early
//     return before the destination half is part of the ordering, not style:
//     a destination verdict built for a caller who could not read the source
//     is itself a disclosure. WriteHidden collapses absence and
//     forbidden-ness onto one 404 (DR-10b).
//
//     The helper is used for the SOURCE too, even though it is the request's
//     own workspace, because it derives the role from membership rather than
//     from workspaceRole(r) — the same answer the front door gives, computed
//     without the context value the destination half must not touch.
//
//  3. Body decode and required-field checks, which sit HERE — after source
//     authorization, before any destination lookup. Hoisting the destination
//     lookup earlier would disclose destination state to a caller sending a
//     bad body.
//
//  4. Check 3, part one: workspace-level access to the destination. This is
//     the narrow legitimate use of the workspace-only scope — an early reject
//     BEFORE the collection is known, because resolving a collection slug
//     requires the destination workspace's ID and doing that lookup first
//     would answer a question about a workspace the caller may have no
//     business addressing. WriteDenied, not WriteHidden: the caller named
//     this workspace themselves. Note it is not one outcome — it emits 500,
//     permission_denied or forbidden depending on the reason.
//
//  5. Destination collection lookup, then checks 3 (collection visibility)
//     and 4 (collection edit). WriteCollectionNotFound collapses "hidden"
//     onto the same 404 the nil branch emits, or delegates back to
//     WriteDenied — otherwise a restricted member of the destination could
//     enumerate the collections they were excluded from.
//
//  6. Actor attribution, immediately after the fourth check and BEFORE any
//     field or override handling. Position matters: an earlier version
//     resolved it down by the attachment planner, so the two endpoints
//     disagreed about a request that fails BOTH ways — no actor and a
//     malformed override — with the preview reporting the override and the
//     copy the actor.
//
// NOT read here, deliberately: `force`, or any query parameter at all.
// archive_source is JSON input. The intra-workspace move handler's force
// semantics are its own and must not be inherited by proximity.
func (s *Server) resolveAuthorizedCopy(w http.ResponseWriter, r *http.Request) (authorizedCopy, bool) {
	var out authorizedCopy

	sourceWorkspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return out, false
	}
	out.sourceWorkspaceID = sourceWorkspaceID

	itemSlug := chi.URLParam(r, "itemSlug")
	item, err := s.store.ResolveItem(sourceWorkspaceID, itemSlug)
	if err != nil {
		writeInternalError(w, err)
		return out, false
	}
	if item == nil {
		// ResolveItem filters soft-deleted rows, so this covers "absent"
		// AND "archived". writeItemResolveError separates them the same
		// way the move and update paths do — 409 archived when the caller
		// can independently SEE the archived row, 404 otherwise — so the
		// distinction is never made for someone who could not see it.
		s.writeItemResolveError(w, r, sourceWorkspaceID, itemSlug)
		return out, false
	}
	out.item = item

	// ---- Authorization, DR-10a/DR-10b, four checks in order -----------
	src := s.AuthorizeCrossWorkspaceEdit(r, sourceWorkspaceID, CrossWorkspaceItemScope(item))
	if !src.Allowed {
		// WriteHidden: absence and forbidden-ness are one 404. A response
		// that confirmed a hidden item exists would be the leak DR-10b is
		// about.
		src.WriteHidden(w, "Item")
		return out, false
	}
	out.source = src

	// decodeJSON, not json.NewDecoder: it wraps the body in a
	// MaxBytesReader so field_overrides cannot be used to make the server
	// allocate a multi-GB map on an endpoint that is designed to be called
	// repeatedly from a live UI (Codex round 7).
	var input itemCopyPreflightRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "Invalid JSON body")
		return out, false
	}
	if input.TargetWorkspace == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "target_workspace is required")
		return out, false
	}
	if input.TargetCollection == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "target_collection is required")
		return out, false
	}
	out.input = input

	dstWS := s.AuthorizeCrossWorkspaceEdit(r, input.TargetWorkspace, CrossWorkspaceWorkspaceOnlyScope())
	if !dstWS.Allowed {
		// WriteDenied, not WriteHidden: the caller named this workspace
		// themselves, so acknowledging the refusal tells them nothing they
		// did not already assert. It still refuses to separate "absent"
		// from "forbidden".
		dstWS.WriteDenied(w)
		return out, false
	}

	targetColl, err := s.resolveItemCollectionSlug(dstWS.WorkspaceID(), input.TargetCollection)
	if err != nil {
		writeInternalError(w, err)
		return out, false
	}
	if targetColl == nil {
		writeError(w, http.StatusNotFound,
			crossWorkspaceCollectionNotFoundCode, crossWorkspaceCollectionNotFoundMessage)
		return out, false
	}

	dst := s.AuthorizeCrossWorkspaceEdit(r, input.TargetWorkspace, CrossWorkspaceCollectionScope(targetColl.ID))
	if !dst.Allowed {
		dst.WriteCollectionNotFound(w)
		return out, false
	}
	out.destination = dst
	out.targetCollection = targetColl

	// Who the copy is attributed to, resolved through ONE function and
	// refused on one set of terms. The preflight writes nothing, so a
	// placeholder would "work" there — and that is exactly the trap: an
	// earlier version supplied a `"preflight"` literal as a last resort, so
	// the dry run happily previewed a copy the mutation would refuse
	// outright for want of an actor (Codex round 4).
	actorID := copyActorID(r, item)
	if actorID == "" {
		writeCopyActorRequired(w)
		return out, false
	}
	out.actorID = actorID

	// Built against the SOURCE workspace: it authorizes the rows the
	// planner resolves, and the planner resolves only source-workspace
	// rows. Cheap to build (it queries nothing until called) so it is
	// created unconditionally rather than lazily by each consumer.
	out.attachmentAuth = s.attachmentCopyAuthorizer(r, sourceWorkspaceID)

	return out, true
}
