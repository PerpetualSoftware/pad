package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/PerpetualSoftware/pad/internal/collab"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/PerpetualSoftware/pad/internal/bodypatch"
	"github.com/PerpetualSoftware/pad/internal/diff"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/google/uuid"
)

// PLAN-3578 step 1: content-anchored patch writes on an item with no live
// editor. POST /workspaces/{ws}/items/{ref}/patch {edits, expected_seq?}.
//
// A patch is an ordinary PATCH underneath (updateItem, through the same hook a
// built-in update uses), so it takes every guard a PATCH takes. What differs:
//   - the body is computed inside the store transaction from the row it
//     replaces (models.ItemUpdate.ContentTransform), so anchors match the
//     committed body, never an earlier read;
//   - expected_seq does not refuse: anchors are the precondition (ruling iii).
//     A moved seq is applied over and reported as rebased, with the
//     intervening writes and the diff;
//   - an item with a live editor is refused (patch_room_open). The open-room
//     path is step 2; a whole-document replace is never the fallback;
//   - unflushed editor edits refuse it, token or not: items.content is then
//     behind the document, and the anchors would match stale text.

type patchRequest struct {
	Edits       []bodypatch.Edit `json:"edits"`
	ExpectedSeq *int64           `json:"expected_seq,omitempty"`
}

// itemPatch carries one patch through updateItem. The transform fills the
// in-transaction fields; the response and the activity read them.
type itemPatch struct {
	id          string
	edits       []bodypatch.Edit
	expectedSeq *int64

	baseSeq int64  // the row's seq inside the transaction, before the write
	before  string // the body the edits applied to
	after   string // the body they produced
}

// maxPatchIntervening caps the intervening writes a rebased patch lists
// (lead ruling); the diff is the real check.
const maxPatchIntervening = 50

func (s *Server) handlePatchItem(w http.ResponseWriter, r *http.Request) {
	var req patchRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if len(req.Edits) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "edits is required: at least one {old, new}")
		return
	}
	if len(req.Edits) > bodypatch.MaxEdits {
		writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("%d edits; at most %d per patch", len(req.Edits), bodypatch.MaxEdits))
		return
	}
	for i, e := range req.Edits {
		if e.Old == "" {
			writeError2(w, http.StatusBadRequest, "bad_request",
				fmt.Sprintf("edit %d: old is empty; every edit must quote the text it replaces", i),
				map[string]interface{}{"edit": i})
			return
		}
	}
	p := &itemPatch{id: uuid.NewString(), edits: req.Edits, expectedSeq: req.ExpectedSeq}
	input := models.ItemUpdate{
		ContentTransform: func(existing *models.Item) (string, error) {
			after, err := bodypatch.Apply(existing.Content, p.edits)
			if err != nil {
				return "", err
			}
			p.baseSeq, p.before, p.after = existing.Seq, existing.Content, after
			return after, nil
		},
		// Every patch is a version, past the throttle, so it can be undone
		// from History (as the ChatGPT door's content writes are).
		ForceVersion: true,
		// Refuse over unflushed editor edits whether or not a token came.
		RefusePendingEdits: true,
		ChangeSummary:      fmt.Sprintf("patch: %d edit(s)", len(req.Edits)),
	}
	s.updateItem(w, r, &builtinItemUpdate{input: input, patch: p})
}

// writePatchRefusal answers a bodypatch refusal with its typed 409; false
// when err is not one.
func (s *Server) writePatchRefusal(w http.ResponseWriter, ref string, err error) bool {
	var missing *bodypatch.AnchorMissingError
	if errors.As(err, &missing) {
		s.countPatchRefusal("anchor_missing")
		writeError2(w, http.StatusConflict, "patch_anchor_missing",
			fmt.Sprintf("%s: edit %d's old text is not in the current body; re-read it and quote text that is there", ref, missing.Index),
			map[string]interface{}{"ref": ref, "edit": missing.Index})
		return true
	}
	var amb *bodypatch.AnchorAmbiguousError
	if errors.As(err, &amb) {
		s.countPatchRefusal("anchor_ambiguous")
		writeError2(w, http.StatusConflict, "patch_anchor_ambiguous",
			fmt.Sprintf("%s: edit %d's old text occurs %d times; quote more surrounding text, or pass line_hint", ref, amb.Index, len(amb.Lines)),
			map[string]interface{}{"ref": ref, "edit": amb.Index, "lines": amb.Lines})
		return true
	}
	var empty *bodypatch.EmptyAnchorError
	if errors.As(err, &empty) {
		writeError2(w, http.StatusBadRequest, "bad_request", empty.Error(), map[string]interface{}{"edit": empty.Index})
		return true
	}
	return false
}

func writePatchRoomOpen(w http.ResponseWriter, ref string) {
	writeError2(w, http.StatusConflict, "patch_room_open",
		fmt.Sprintf("%s is open in an editor; a patch to an open item is not supported yet. Retry once the editor closes, or send a whole-body update", ref),
		map[string]interface{}{"ref": ref})
}

// patchRoute is routeContentUpdate for a patch: the same settle. A room with
// a live writer gets the patch through a patch-capable applier tab (step 2);
// with no live writer the patch is computed in the store transaction (step 1).
func (s *Server) patchRoute(
	w http.ResponseWriter,
	r *http.Request,
	item *models.Item,
	input *models.ItemUpdate,
	precheck func(*sql.Tx, *models.Item) error,
	parentLink *store.ParentLinkUpdate,
	pruned *int,
	p *itemPatch,
) (contentRoute, *models.Item) {
	var updated *models.Item
	outcome, paErr := settleContentRoute(
		r.Context(),
		func() bool { return s.collab.HasElectableApplier(item.ID) },
		func() error {
			return s.collab.PruneAndApply(item.ID, func() error {
				pc := composePruneWithPrecheck(s, item.ID, precheck, pruned)
				u, uerr := s.store.UpdateItemWithParentLink(item.ID, *input, pc, parentLink)
				if uerr != nil {
					return uerr
				}
				updated = u
				return nil
			})
		},
		applierSettleBudget, applierSettlePoll,
	)
	switch outcome {
	case settleElectApplier:
		return s.patchViaApplier(w, item, input, parentLink, p)
	case settleDirectWrote:
		return contentRouteDirectWrote, updated
	case settleUnsettled:
		writeRoomSettlingError(w, itemRefOrSlug(*item))
		return contentRouteHandled, nil
	default:
		if s.writeTypedItemRefusal(w, item, paErr, input.ChatGPTDoor) {
			return contentRouteHandled, nil
		}
		writeInternalError(w, paErr)
		return contentRouteHandled, nil
	}
}

// patchActivityMeta adds the patch's own keys to the update's activity
// entry: its id, how many edits, and whether it rebased.
func (p *itemPatch) activityMeta(meta string) string {
	meta = withActivityMetaKey(meta, "patch_id", p.id)
	meta = withActivityMetaKey(meta, "patch_edits", strconv.Itoa(len(p.edits)))
	if p.rebased() {
		meta = withActivityMetaKey(meta, "patch_rebased_from_seq", strconv.FormatInt(*p.expectedSeq, 10))
	}
	return meta
}

func (p *itemPatch) rebased() bool {
	return p.expectedSeq != nil && *p.expectedSeq != p.baseSeq
}

// patchResponse is the answer to a patch.
type patchResponse struct {
	Ref       string `json:"ref"`
	Seq       int64  `json:"seq"`
	PatchID   string `json:"patch_id"`
	VersionID string `json:"version_id,omitempty"`
	Rebased   bool   `json:"rebased"`
	// ContentState is "applied_pending_flush" when an open editor applied
	// the patch: the row catches up on that tab\'s next flush.
	ContentState string                   `json:"content_state,omitempty"`
	Intervening  *models.PatchIntervening `json:"intervening,omitempty"`
	Diff         string                   `json:"diff"`
}

func (s *Server) writePatchResponse(w http.ResponseWriter, updated *models.Item, p *itemPatch, pendingFlush bool) {
	resp := patchResponse{
		Ref:       itemRefOrSlug(*updated),
		Seq:       updated.Seq,
		PatchID:   p.id,
		VersionID: updated.CreatedVersionID,
		Rebased:   p.rebased(),
		Diff:      diff.Unified(p.before, p.after, 3),
	}
	if pendingFlush {
		resp.ContentState = contentOutcomeAppliedPendingFlush
	}
	if resp.Rebased {
		iv, err := s.store.ItemSeqChain(updated.ID, *p.expectedSeq, p.baseSeq, maxPatchIntervening)
		if err != nil {
			// The write committed; an unreadable chain is reported as
			// incomplete rather than failing an answer about a done write.
			iv = &models.PatchIntervening{FromSeq: *p.expectedSeq, ToSeq: p.baseSeq}
		}
		resp.Intervening = iv
	}
	writeJSON(w, http.StatusOK, resp)
}

// stampSeqChain records on an item activity entry the write's place in the
// item's seq chain (PLAN-3578): it moved the item from prev to seq, and wrote
// versionID when non-empty.
func stampSeqChain(meta string, prev, seq int64, versionID string) string {
	meta = withActivityMetaKey(meta, store.ActivityMetaPrevSeq, strconv.FormatInt(prev, 10))
	meta = withActivityMetaKey(meta, store.ActivityMetaSeq, strconv.FormatInt(seq, 10))
	if versionID != "" {
		meta = withActivityMetaKey(meta, store.ActivityMetaVersionID, versionID)
	}
	return meta
}

// patchViaApplier sends the patch to an open editor (PLAN-3578 step 2), which
// applies it to the LIVE document as a narrowed replace and reports the
// canonical before/after. Unflushed editor edits do not refuse it: the tab
// applies to the document that holds them. Once the tab confirms, a row write
// records the patched body as a version attributed to this request
// (ExternalContent, as the markdown applier path does since BUG-3327) and
// moves the seq, so the activity entry joins the seq chain. A patch carries
// no field changes, so nothing can refuse that write after the apply.
func (s *Server) patchViaApplier(w http.ResponseWriter, item *models.Item, input *models.ItemUpdate, parentLink *store.ParentLinkUpdate, p *itemPatch) (contentRoute, *models.Item) {
	ref := itemRefOrSlug(*item)
	edits, err := json.Marshal(p.edits)
	if err != nil {
		writeInternalError(w, err)
		return contentRouteHandled, nil
	}
	ack, aerr := s.collab.ApplyExternalPatch(item.ID, p.id, edits)
	if aerr != nil {
		var refused *collab.PatchRefusedError
		switch {
		case errors.As(aerr, &refused):
			s.writeTabPatchRefusal(w, ref, refused)
		case errors.Is(aerr, collab.ErrNoActiveRoom), errors.Is(aerr, collab.ErrNoApplierAvailable):
			// No patch-capable editor (an older tab, or the room emptied):
			// nothing was sent.
			s.countPatchRefusal("room_open")
			writePatchRoomOpen(w, ref)
		case errors.Is(aerr, collab.ErrApplierAmbiguous):
			writeError(w, http.StatusConflict, "applier_ambiguous",
				"A concurrent version restore made this patch's outcome ambiguous; re-read the item before retrying.")
		default:
			// Timed out, or the tab acknowledged without bodies: the patch may
			// have landed. Say so rather than claim either way.
			slog.Warn("collab: patch apply did not confirm", "item_id", item.ID, "error", aerr)
			writeError2(w, http.StatusConflict, "content_not_applied",
				ref+": the open editor did not confirm the patch; it may or may not have applied. Re-read the item before retrying.",
				map[string]interface{}{"ref": ref, "content_outcome": contentOutcomeUnknown})
		}
		return contentRouteHandled, nil
	}
	p.baseSeq, p.before, p.after = item.Seq, ack.Before, ack.After
	row := *input
	row.ContentTransform = nil
	row.Content = nil
	row.RefusePendingEdits = false
	row.ExternalContent = &ack.After
	updated, uerr := s.store.UpdateItemWithParentLink(item.ID, row, nil, parentLink)
	if uerr != nil {
		// The document has the patch; only its version row is missing.
		slog.Error("collab: patch applied but its version row failed", "item_id", item.ID, "error", uerr)
		writeInternalError(w, uerr)
		return contentRouteHandled, nil
	}
	return contentRouteApplierWrote, updated
}

// writeTabPatchRefusal answers a refusal the applier tab made.
func (s *Server) writeTabPatchRefusal(w http.ResponseWriter, ref string, e *collab.PatchRefusedError) {
	s.countPatchRefusal(e.Reason)
	details := map[string]interface{}{"ref": ref}
	if e.Edit != nil {
		details["edit"] = *e.Edit
	}
	if len(e.Lines) > 0 {
		details["lines"] = e.Lines
	}
	if e.Line > 0 {
		details["line"] = e.Line
	}
	switch e.Reason {
	case "anchor_missing":
		writeError2(w, http.StatusConflict, "patch_anchor_missing", ref+": an edit's old text is not in the open editor's current body; re-read it and quote text that is there", details)
	case "anchor_ambiguous":
		writeError2(w, http.StatusConflict, "patch_anchor_ambiguous", ref+": an edit's old text occurs more than once in the open editor's body; quote more surrounding text, or pass line_hint", details)
	default:
		// unstable_roundtrip, parse_refused, or a reason this server does not
		// know: the tab could not apply the patch without changing text the
		// agent never saw.
		details["reason"] = e.Reason
		writeError2(w, http.StatusConflict, "patch_unstable_roundtrip",
			ref+": the open editor cannot apply this patch without changing content you did not edit; send a whole-body update with expected_seq instead",
			details)
	}
}

// countPatchRefusal counts a refused patch by reason (lead ruling: the
// uncovered share must be visible).
func (s *Server) countPatchRefusal(reason string) {
	if s.metrics == nil {
		return
	}
	switch reason {
	case "anchor_missing", "anchor_ambiguous", "unstable_roundtrip", "parse_refused", "room_open", "empty_anchor":
	default:
		reason = "other"
	}
	s.metrics.PatchRefusalsTotal.WithLabelValues(reason).Inc()
}
