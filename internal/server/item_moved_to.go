package server

import (
	"log/slog"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The archived-source "moved to" pointer (PLAN-2357 / TASK-2359).
//
// An item that was MOVED to another workspace — copied, then archived — should
// be able to say where it went. This file is the read-side of that: a single
// optional block on the single-item GET response naming the destination in
// displayable terms (workspace slug + item ref), so the consumer can render a
// link without a second call.
//
// It is NOT a redirect and NOT a resolver change. GET on an archived item
// still returns the archived item; this only decorates it.
//
// ############################################################
// # Populate MovedTo from handleGetItem ONLY.                 #
// ############################################################
//
// Not from enrichItemForResponse — that runs on create, update, restore and
// move responses too, and every additional surface is another place the ACL
// gate below has to be re-proved. Not from any list, search, activity,
// backlink or delta path — those return many items and would multiply the
// per-destination authorization into an N×M cost while widening the
// disclosure surface for nothing. And explicitly not from the public
// share-link DTO (handlers_share_links.go), which is hand-rolled and
// therefore isolated by construction; TestMovedTo_ShareLinkNeverCarriesPointer
// pins that isolation so a future refactor to `writeJSON(w, ..., item)` there
// cannot silently start publishing destinations to anonymous visitors.
//
// THE DISCLOSURE RULE. A caller who may not read a destination must get a
// response byte-identical to the response for an archived item with no move
// record at all. Not a null, not an empty array, not a boolean, not a count.
// The whole key is absent. `omitempty` on a nil slice is what delivers that,
// so never assign an empty non-nil slice here.
//
// TIMING IS OUT OF SCOPE, DELIBERATELY. Status, headers and bytes are
// identical, but a withheld destination costs an extra item load and an extra
// cross-workspace authorization, so a caller who can repeatedly re-fetch an
// archived source could in principle distinguish the two cases by latency.
// Closing that would mean performing movedToScanLimit dummy authorizations on
// every archived-item GET — a real per-read cost paid against a channel that
// is noisy over a network, and one Pad already exposes wherever a response is
// conditionally enriched (visible-parent lookup, derived closure, guest
// resource filters). The contract this file enforces is the RESPONSE
// contract, matching CrossWorkspaceAccess.WriteHidden, which makes the same
// kind of trade explicitly for internal errors. If Pad ever adopts a
// constant-time posture it belongs as its own item across all of those
// surfaces; do not solve it here alone.

// movedToScanLimit bounds how many MOVE rows one GET will consider.
//
// Each candidate costs a destination load plus a workspace resolve, a role
// derivation, a collection load and a visibility check
// (AuthorizeCrossWorkspaceRead is deliberately per-item and does not batch).
// Rows are only ever written by a copy/move operation — one row each — so
// reaching this many MOVES of a single source means archive → restore → move
// again, twenty-five times over. "Takes deliberate effort" is not a bound,
// though, and an unbounded per-read fan-out on a route any member can call is
// not something to leave to good manners.
//
// It is pushed down into the SQL (ListArchivedItemWorkspaceMovesBySource)
// rather than applied while looping, so it bounds rows RETURNED — scanned into
// structs and then authorized one by one — and not merely rows kept. A long
// tail of plain COPIES, which can never contribute to this block, is excluded
// by the same query. It is not a claim about the planner; see that method's
// doc for what the LIMIT does and does not promise.
//
// The cap is applied to the newest rows and BEFORE the ACL filter, which means
// a caller who could have read only destination #26 gets nothing. That is the
// honest shape of a cost bound: filtering first would require authorizing
// every row, which is the thing being bounded. Newest-first is the right
// truncation because the newest move is the one a banner most wants.
const movedToScanLimit = 25

// movedToDestinations returns the destinations an archived item was moved to
// that THIS caller is independently authorized to read, newest first, or nil.
//
// Six gates, ALL of which must hold for a destination to appear. They are
// numbered as conditions, not as an execution sequence: the code evaluates
// gate 4's provenance query before gate 3's source-collection check so it
// pays for a collection load only once there is actually something to
// disclose. All six are conjunctive and none is order-sensitive, so the
// reordering is a cost choice with no effect on the verdict — but do not read
// the list as a trace.
//
//  1. the SOURCE is archived (see the restore decision below);
//  2. the caller is not a grants-only GUEST in the source workspace (see below);
//  3. the source's own collection is live (see below);
//  4. the provenance row is a MOVE, not a plain copy (DR-2a: archived_source);
//  5. the destination item still exists and is live;
//  6. the caller passes AuthorizeCrossWorkspaceRead with an ITEM scope on the
//     destination item itself.
//
// Gate 6 is the point of the whole function, and the ITEM scope is the point
// of gate 6. A workspace-level check (CrossWorkspaceWorkspaceOnlyScope) is NOT
// sufficient and using one here would be the bug: a restricted member of the
// destination workspace, or a guest holding one unrelated item grant there,
// has a role in that workspace while having no right whatsoever to see the
// copied item's collection. Naming the destination to them leaks both the
// existence and the location of an item they may not read.
//
// THE SET, NOT THE NEWEST ROW — and no "current" marker. PLAN-2357's DR-2a
// phrases the pointer as resolving to "the newest archived_source row, the
// older one is history", which reads as first-match. TASK-2359 refines it into
// the per-destination filter implemented here, and the refinement is the
// correct one for two reasons. First, filtering is per caller: with a
// first-match rule a caller who may read the older destination but not the
// newer gets nothing at all, and useful information is replaced by silence
// rather than by a smaller answer. Second, both destinations genuinely exist —
// move → restore → move again leaves real items in both workspaces — so
// naming both is accurate; only the claim "it currently lives at X" would be
// wrong, and this block never makes that claim.
//
// Which is also why there is deliberately NO per-entry `current` flag. It
// would be the obvious way to let Phase 3 word a banner precisely, and it is a
// disclosure vector: `current: false` on the sole visible entry announces that
// a NEWER destination exists and is being withheld — the exact inference the
// omit-entirely rule forbids. Phase 3 must therefore word the banner as
// provenance ("this item was moved; copies exist at …"), not as a current
// location, because an ACL-filtered list cannot promise its head is the
// newest move.
//
// PRECONDITION, ENFORCED RATHER THAN ASSUMED: the request must have resolved
// to the SOURCE ITEM'S OWN WORKSPACE. handleGetItem is the only caller and it
// satisfies this trivially, but the signature does not — hand this an item
// from workspace B on a request scoped to workspace A and gate 2 would test
// the caller's role in A while the source lives in B, so a grants-only guest
// of B who happens to be an owner of A would sail through the very gate that
// exists to stop them. That is the same confused-deputy shape
// CrossWorkspaceItemScope guards against on the destination side, and "only
// one caller today" is not a guard. Mismatch returns nil.
//
// GATE 2, GRANTS-ONLY GUESTS ON THE SOURCE. PLAN-2357 names this alongside
// share links: "a share link on the source, or a guest holding only a
// source-item grant, must never see moved_to". Both are narrow, delegated
// access — someone was handed one item, not that item's cross-workspace
// provenance — and the rule holds even when the guest could independently read
// the destination, because the question is what the SOURCE grant conveys.
//
// This is the one place workspaceRole(r) is the right thing to read: the
// source IS the request's own workspace, which is the only workspace that
// context value ever describes. It is emphatically not usable for the
// destination, and gate 6 derives that role fresh — see the header of
// authz_cross_workspace.go for what happens when the two are confused.
//
// GATE 3, THE SOURCE'S COLLECTION. handleGetItem has already run requireItemVisible on
// the source, which is what authorizes reading the item at all — but that check
// does NOT establish that the source's collection is live. Soft-deleting a
// collection leaves its items in place, and neither ResolveItemIncludeDeleted
// nor VisibleCollectionIDs filters on the collection's deleted_at, so an
// archived item under an archived collection is still fetchable today. That is
// pre-existing behavior for the item BODY and this function does not change it
// — widening or narrowing the item read is out of scope here.
//
// What it does do is refuse to ADD a cross-workspace disclosure on top of it.
// authorizeCrossWorkspace applies exactly this rule to the destination side
// (crossWorkspaceLiveCollection, "Codex round 2 P1" in that file), and the
// source deserves the symmetric treatment: an item in a collection the
// workspace has retired should not be the thing that reveals where a copy of
// it lives in another workspace. One extra collection load, paid only for an
// archived item that actually has move rows.
//
// RESTORE DECISION (TASK-2359). The block is OMITTED for a non-archived
// source, full stop. Restoring a moved-out source leaves two live items with
// the same content in two workspaces — legitimate, and the same end state a
// plain copy produces — but at that instant the source has not "moved"
// anywhere, so continuing to assert a move would be false. Past-tense
// provenance ("this was also copied to B") is real, but it is the BACK-pointer
// question and it applies equally to plain copies, which this block must never
// claim as moves; it belongs on a provenance surface of its own rather than
// smuggled through a field whose name asserts a move. Omitting also keeps the
// disclosure surface minimal: a live item's response shape is then identical
// for moved-and-restored and never-moved items, exactly as the denial case is.
//
// A store failure degrades to nil rather than failing the GET: the item read
// is the caller's actual request, and a provenance lookup error is not a
// reason to withhold it. Fail-closed on disclosure, fail-open on the read.
//
// NOT ATOMIC — inherited from AuthorizeCrossWorkspaceRead. The verdict
// describes the world at the instant it was computed; this is a read path, so
// a stale verdict is a narrow window, but do not cache one.
func (s *Server) movedToDestinations(r *http.Request, item *models.Item) []models.ItemMovedTo {
	// Gate 1: only an archived source can have moved. See the restore
	// decision above.
	if item == nil || item.DeletedAt == nil || item.ID == "" {
		return nil
	}
	itemID := item.ID

	// Precondition. workspaceRole(r) below describes the workspace the request
	// URL resolved to and nothing else, so it is only an answer about the
	// SOURCE while these two agree. See the note above.
	resolvedWS, _ := r.Context().Value(ctxResolvedWorkspaceID).(string)
	if resolvedWS == "" || resolvedWS != item.WorkspaceID {
		slog.Warn("movedToDestinations: called outside the source item's own workspace context; omitting pointer",
			"item_id", item.ID, "item_workspace_id", item.WorkspaceID, "request_workspace_id", resolvedWS)
		return nil
	}

	// Gate 2. Checked before the provenance query so a delegated-access
	// caller never even causes the lookup.
	if workspaceRole(r) == "guest" {
		return movedToOmitted(itemID, "source_guest")
	}

	// Gate 4 (DR-2a) is pushed into SQL: only archived_source rows, newest
	// first, at most movedToScanLimit of them. A plain copy is back-pointer
	// material only and must never claim the source moved, so it is excluded
	// by the query rather than by the loop — that way a long tail of copies
	// costs nothing on a read of the source.
	moves, err := s.store.ListArchivedItemWorkspaceMovesBySource(item.ID, movedToScanLimit)
	if err != nil {
		slog.Warn("movedToDestinations: provenance lookup failed; omitting pointer",
			"item_id", item.ID, "error", err)
		return nil
	}
	if len(moves) == 0 {
		return movedToOmitted(itemID, "no_move_rows")
	}

	// Gate 3, paid only now that there is something to disclose. See the
	// source-side note above for why requireItemVisible is not sufficient.
	coll, cErr := s.store.GetCollection(item.CollectionID)
	if cErr != nil {
		slog.Warn("movedToDestinations: source collection lookup failed; omitting pointer",
			"item_id", item.ID, "error", cErr)
		return nil
	}
	// GetCollection filters soft-deleted rows, so nil covers "absent" and
	// "archived" alike.
	if coll == nil || coll.WorkspaceID != item.WorkspaceID {
		return movedToOmitted(itemID, "source_collection_not_live")
	}

	// Nil, never an empty slice — see the disclosure rule in the file header.
	var out []models.ItemMovedTo

	for i := range moves {
		m := moves[i]

		// Defense in depth: the query already filters this, but DR-2a is the
		// invariant the whole block rests on and a consumer-side assert costs
		// nothing.
		if !m.ArchivedSource {
			continue
		}

		// Gate 5: the destination must still be there. GetItem filters
		// soft-deleted rows, so an archived destination drops out — pointing a
		// banner at an item that is itself archived is worse than saying
		// nothing, and the caller loses no access they had.
		target, terr := s.store.GetItem(m.TargetItemID)
		if terr != nil {
			slog.Warn("movedToDestinations: destination lookup failed; skipping",
				"item_id", item.ID, "target_item_id", m.TargetItemID, "error", terr)
			continue
		}
		if target == nil {
			slog.Debug("movedToDestinations: destination item is gone or archived; skipping",
				"item_id", itemID, "target_item_id", m.TargetItemID)
			continue
		}
		// Fail closed if the row and the item disagree about where the
		// destination lives. Authorizing workspace X while describing an item
		// that now sits in workspace Y is the confused-deputy shape
		// CrossWorkspaceItemScope guards against; catching it here means the
		// mismatch is never even offered to the helper.
		if target.WorkspaceID != m.TargetWorkspaceID {
			slog.Warn("movedToDestinations: provenance row disagrees with destination item's workspace; skipping",
				"item_id", item.ID, "target_item_id", m.TargetItemID,
				"row_workspace_id", m.TargetWorkspaceID, "item_workspace_id", target.WorkspaceID)
			continue
		}

		// Gate 6. ITEM scope, never workspace-only. Addressing the workspace
		// by ID is fine: the helper resolves it and tests the consent
		// allow-list against the resolved CANONICAL slug.
		access := s.AuthorizeCrossWorkspaceRead(r, m.TargetWorkspaceID, CrossWorkspaceItemScope(target))
		if !access.Allowed {
			// Server-side only. Nothing about this denial — not its reason,
			// not the workspace it names — may reach the response. The
			// verdict struct is `json:"-"` throughout precisely so a stray
			// marshal cannot undo that; these fields are for operators, which
			// is the use CrossWorkspaceAccess's own contract reserves them
			// for.
			//
			// A LOOKUP FAILURE IS NOT A DENIAL, though it is handled as one.
			// The caller may well have been entitled to this destination and
			// lost it to a broken store, so it is logged loudly and with the
			// error, while an ordinary "you may not see this" stays at Debug
			// where it belongs — those fire routinely and by design.
			if access.Reason == CrossWorkspaceLookupFailed {
				slog.Warn("movedToDestinations: destination authorization failed; withholding",
					"item_id", item.ID, "target_item_id", m.TargetItemID,
					"target_workspace", access.WorkspaceSlug(), "error", access.Err)
				continue
			}
			slog.Debug("movedToDestinations: destination withheld",
				"item_id", item.ID, "reason", string(access.Reason),
				"target_workspace", access.WorkspaceSlug())
			continue
		}

		ws := access.Workspace
		if ws == nil {
			// Unreachable for an allowed verdict; refuse rather than emit a
			// half-populated entry.
			continue
		}

		// WorkspaceName and WorkspaceOwnerUsername go beyond the bare
		// "slug + ref" locator the task asks for. That is a deliberate
		// judgement — they turn a bare slug into "Moved to Pad Web" with a
		// working /{username}/{workspace}/… link, which is the whole "no
		// second call" requirement — and it holds for every caller gate 6
		// admits, though for two different reasons:
		//
		//   - MEMBERS and GRANT-HOLDERS already enumerate both fields.
		//     GetUserWorkspaces (workspace_members.go) returns w.name and the
		//     owner's username for members, and its guest branch does the same
		//     for anyone holding a grant on a LIVE item in a LIVE collection —
		//     which is exactly what gate 6 admits, since gate 5 has already
		//     established the destination item is live. Nothing new.
		//
		//   - The three SYNTHESIZED roles crossWorkspaceRole hands out are not
		//     in that enumeration, so for them this is genuinely new data — and
		//     harmless in each case. A cookie-session platform admin is "owner"
		//     everywhere and reaches any workspace through the admin surfaces
		//     regardless. A nil user on a fresh install predates the first
		//     account, when the whole instance is open by design. A legacy
		//     workspace-pinned token is only ever "editor" for the ONE
		//     workspace it is pinned to, so the destination is its own
		//     workspace. (A BEARER-borne admin is deliberately NOT on this
		//     list: crossWorkspaceRole suppresses that bypass, BUG-1616/1617.)
		//
		// If the guest enumeration ever narrows, or a fourth synthesized role
		// appears, re-run this argument — and drop these two fields first if it
		// no longer holds. The slug and ref alone satisfy the task.
		out = append(out, models.ItemMovedTo{
			WorkspaceSlug:          ws.Slug,
			WorkspaceName:          ws.Name,
			WorkspaceOwnerUsername: ws.OwnerUsername,
			CollectionSlug:         target.CollectionSlug,
			Ref:                    target.Ref,
			ItemSlug:               target.Slug,
			Title:                  target.Title,
			MovedAt:                m.CreatedAt,
		})
	}

	if out == nil {
		return movedToOmitted(itemID, "all_destinations_filtered")
	}
	return out
}

// movedToOmitted records WHY the block was omitted and returns nil.
//
// "The banner isn't showing" has half a dozen legitimate causes — no move
// rows, a plain-copy-only history, an archived destination, an archived source
// collection, a delegated-access caller, every destination filtered — and
// without this they are indistinguishable to an operator, because the whole
// point of the disclosure rule is that the RESPONSE cannot tell them apart
// either. So the distinction lives in the log instead.
//
// Debug, not Info: on a healthy instance the overwhelmingly common outcome is
// "no move rows", which would otherwise fire on every archived-item read. The
// reasons are fixed strings and the only identifier is the caller's OWN item —
// nothing here names a resource the caller was denied.
func movedToOmitted(itemID, reason string) []models.ItemMovedTo {
	slog.Debug("movedToDestinations: no pointer emitted",
		"item_id", itemID, "reason", reason)
	return nil
}
