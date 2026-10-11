package server

import (
	"database/sql"
	"errors"
	"fmt"
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
func writePatchRefusal(w http.ResponseWriter, ref string, err error) bool {
	var missing *bodypatch.AnchorMissingError
	if errors.As(err, &missing) {
		writeError2(w, http.StatusConflict, "patch_anchor_missing",
			fmt.Sprintf("%s: edit %d's old text is not in the current body; re-read it and quote text that is there", ref, missing.Index),
			map[string]interface{}{"ref": ref, "edit": missing.Index})
		return true
	}
	var amb *bodypatch.AnchorAmbiguousError
	if errors.As(err, &amb) {
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

// patchRoute is routeContentUpdate for a patch: the same settle, but a room
// with an electable applier is refused rather than handed the content.
func (s *Server) patchRoute(
	w http.ResponseWriter,
	r *http.Request,
	item *models.Item,
	input *models.ItemUpdate,
	precheck func(*sql.Tx, *models.Item) error,
	parentLink *store.ParentLinkUpdate,
	pruned *int,
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
		writePatchRoomOpen(w, itemRefOrSlug(*item))
		return contentRouteHandled, nil
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
	Ref         string                   `json:"ref"`
	Seq         int64                    `json:"seq"`
	PatchID     string                   `json:"patch_id"`
	VersionID   string                   `json:"version_id,omitempty"`
	Rebased     bool                     `json:"rebased"`
	Intervening *models.PatchIntervening `json:"intervening,omitempty"`
	Diff        string                   `json:"diff"`
}

func (s *Server) writePatchResponse(w http.ResponseWriter, updated *models.Item, p *itemPatch) {
	resp := patchResponse{
		Ref:       itemRefOrSlug(*updated),
		Seq:       updated.Seq,
		PatchID:   p.id,
		VersionID: updated.CreatedVersionID,
		Rebased:   p.rebased(),
		Diff:      diff.Unified(p.before, p.after, 3),
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
