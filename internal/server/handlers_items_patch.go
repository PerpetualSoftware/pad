package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/go-chi/chi/v5"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	// Undo names a patch to invert (PLAN-3578 step 3) instead of edits.
	Undo string `json:"undo,omitempty"`
	// OpKey is the idempotency key in the body (TASK-3585), for callers that
	// cannot set the Idempotency-Key header (the CLI, MCP). Not part of the
	// request hash.
	OpKey string `json:"op_key,omitempty"`
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

	// undoOf is the patch this one inverts, and undoItemID its item: an undo
	// applies only to the item the patch was made on.
	undoOf     string
	undoItemID string
	// record is this patch's item_patches row; the transform (or the room
	// path's post-ack write) fills its edits with context.
	record *models.ItemPatchRecord

	// idem is the claimed idempotency key, when the caller sent one
	// (TASK-3585). retryConn is set on the retry of an unknown outcome: the
	// only connection that may apply it, never re-planned elsewhere.
	idem      *idemClaim
	retryConn string
	baseBody  string
}

// idemClaim is a request's hold on its idempotency key (TASK-3585).
type idemClaim struct {
	workspaceID, userID, key, token string
}

// idempotencyRoutePatch names the patch route in the key's request hash.
const idempotencyRoutePatch = "item.patch"

// idempotencyLease bounds how long a claim blocks a concurrent retry; a
// claim older than this committed nothing (the done write shares the
// write's transaction) and may be taken over.
const idempotencyLease = 60 * time.Second

// completeIdempotencyTx is the AfterWriteTx that records the patch's answer
// with its key, inside the write's transaction.
func (s *Server) completeIdempotencyTx(p *itemPatch, pendingFlush bool) func(*sql.Tx, *models.Item) error {
	return func(tx *sql.Tx, updated *models.Item) error {
		b, err := json.Marshal(patchCore(updated, p, pendingFlush))
		if err != nil {
			return err
		}
		return s.store.CompleteIdempotencyKeyTx(tx, p.idem.workspaceID, p.idem.userID, p.idem.key, p.idem.token, http.StatusOK, string(b))
	}
}

// idempotencyKey reads the caller's key: the header, else op_key. Empty when
// none; an error when it is malformed.
func idempotencyKey(r *http.Request, opKey string) (string, error) {
	k := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if k == "" {
		k = strings.TrimSpace(opKey)
	}
	if k == "" {
		return "", nil
	}
	if len(k) > 200 {
		return "", fmt.Errorf("idempotency key is %d characters; at most 200", len(k))
	}
	for _, c := range k {
		if c < 0x21 || c > 0x7e {
			return "", fmt.Errorf("idempotency key must be printable ASCII without spaces")
		}
	}
	return k, nil
}

// errPatchNotFound: an undo named a patch this item does not have.
var errPatchNotFound = errors.New("patch not found on this item")

// recordEdits stores the applied edits on the patch record.
func (p *itemPatch) recordEdits(applied []bodypatch.AppliedEdit) error {
	b, err := json.Marshal(applied)
	if err != nil {
		return err
	}
	p.record.Edits = string(b)
	return nil
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
	p := &itemPatch{id: uuid.NewString(), expectedSeq: req.ExpectedSeq}
	if req.Undo != "" {
		if len(req.Edits) > 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "undo conflicts with edits: send one")
			return
		}
		workspaceID, ok := s.getWorkspaceID(w, r)
		if !ok {
			return
		}
		rec, err := s.store.GetItemPatch(workspaceID, req.Undo)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		var applied []bodypatch.AppliedEdit
		if rec == nil || json.Unmarshal([]byte(rec.Edits), &applied) != nil || len(applied) == 0 {
			writeError2(w, http.StatusNotFound, "patch_not_found", "no such patch on this item", map[string]interface{}{"undo": req.Undo})
			return
		}
		req.Edits = bodypatch.Invert(applied)
		for _, e := range req.Edits {
			if e.Old == "" {
				// The patch emptied the whole body: no text is left for an
				// anchored undo to name (retro CONVE-735 finding on #2058).
				// Every patch writes a version, so History can restore it.
				writeError2(w, http.StatusBadRequest, "bad_request",
					"that patch emptied the whole body, so there is no text left to anchor an undo on; nothing was written. Restore the version before it from the item's history instead",
					map[string]interface{}{"undo": req.Undo})
				return
			}
		}
		p.undoOf, p.undoItemID = rec.ID, rec.ItemID
	}
	if len(req.Edits) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "edits is required: at least one {old, new}, or undo")
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
		// A no-op edit would advance seq and write no version, against the
		// contract that every patch is a version (retro CONVE-735 finding on
		// #2055). It is refused, writing nothing.
		if e.Old == e.New {
			writeError2(w, http.StatusBadRequest, "bad_request",
				fmt.Sprintf("edit %d changes nothing (old and new are the same); nothing was written", i),
				map[string]interface{}{"edit": i})
			return
		}
	}
	p.edits = req.Edits
	key, kerr := idempotencyKey(r, req.OpKey)
	if kerr != nil {
		writeError(w, http.StatusBadRequest, "bad_request", kerr.Error())
		return
	}
	if key != "" {
		if !s.claimPatchKey(w, r, &req, p, key) {
			return
		}
		// Whatever the outcome below, a claim this request still holds is
		// given up: a refused write releases the key for a corrected retry;
		// a committed one is already done and matches nothing; an unknown
		// one keeps its record and only drops the lease.
		defer func() { _ = s.store.ReleaseIdempotencyKey(p.idem.workspaceID, p.idem.userID, p.idem.key, p.idem.token) }()
		if p.retryConn != "" && s.collab == nil {
			s.writePatchOutcomeUnknown(w, r, p)
			return
		}
	}
	p.record = &models.ItemPatchRecord{ID: p.id}
	summary := fmt.Sprintf("patch: %d edit(s)", len(req.Edits))
	if p.undoOf != "" {
		summary = "undo patch " + p.undoOf
	}
	input := models.ItemUpdate{
		ContentTransform: func(existing *models.Item) (string, error) {
			if p.undoItemID != "" && p.undoItemID != existing.ID {
				return "", errPatchNotFound
			}
			after, applied, err := bodypatch.ApplyWithContext(existing.Content, p.edits)
			if err != nil {
				return "", err
			}
			if err := p.recordEdits(applied); err != nil {
				return "", err
			}
			p.baseSeq, p.before, p.after = existing.Seq, existing.Content, after
			return after, nil
		},
		PatchRecord: p.record,
		// Every patch is a version, past the throttle, so it can be undone
		// from History (as the ChatGPT door's content writes are).
		ForceVersion: true,
		// Refuse over unflushed editor edits whether or not a token came.
		RefusePendingEdits: true,
		ChangeSummary:      summary,
	}
	if p.idem != nil {
		input.AfterWriteTx = s.completeIdempotencyTx(p, false)
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
	if errors.Is(err, errPatchNotFound) {
		writeError2(w, http.StatusNotFound, "patch_not_found", ref+": no such patch on this item", map[string]interface{}{"ref": ref})
		return true
	}
	var empty *bodypatch.EmptyAnchorError
	if errors.As(err, &empty) {
		writeError2(w, http.StatusBadRequest, "bad_request", empty.Error(), map[string]interface{}{"edit": empty.Index})
		return true
	}
	if errors.Is(err, bodypatch.ErrNoChange) {
		writeError2(w, http.StatusBadRequest, "bad_request", ref+": the edits change nothing (together they leave the body as it was); nothing was written", map[string]interface{}{"ref": ref})
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
	if p.undoOf != "" {
		meta = withActivityMetaKey(meta, "patch_undo_of", p.undoOf)
	}
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
	UndoOf    string `json:"undo_of,omitempty"`
	VersionID string `json:"version_id,omitempty"`
	Rebased   bool   `json:"rebased"`
	// AppliedOverSeq is the seq the patch applied over (TASK-3585: a
	// replay recomputes its intervening writes from it).
	AppliedOverSeq int64 `json:"applied_over_seq,omitempty"`
	// ContentState is "applied_pending_flush" when an open editor applied
	// the patch: the row catches up on that tab\'s next flush.
	ContentState string                   `json:"content_state,omitempty"`
	Intervening  *models.PatchIntervening `json:"intervening,omitempty"`
	Diff         string                   `json:"diff"`
}

// patchCore is a patch's answer without its intervening writes: what an
// idempotency key stores (TASK-3585); intervening is computed live.
func patchCore(updated *models.Item, p *itemPatch, pendingFlush bool) patchResponse {
	resp := patchResponse{
		Ref:            itemRefOrSlug(*updated),
		Seq:            updated.Seq,
		PatchID:        p.id,
		UndoOf:         p.undoOf,
		VersionID:      updated.CreatedVersionID,
		Rebased:        p.rebased(),
		AppliedOverSeq: p.baseSeq,
		Diff:           diff.Unified(p.before, p.after, 3),
	}
	if pendingFlush {
		resp.ContentState = contentOutcomeAppliedPendingFlush
	}
	return resp
}

func (s *Server) writePatchResponse(w http.ResponseWriter, updated *models.Item, p *itemPatch, pendingFlush bool) {
	resp := patchCore(updated, p, pendingFlush)
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
	if p.undoItemID != "" && p.undoItemID != item.ID {
		s.writePatchRefusal(w, ref, errPatchNotFound)
		return contentRouteHandled, nil
	}
	edits, err := json.Marshal(p.edits)
	if err != nil {
		writeInternalError(w, err)
		return contentRouteHandled, nil
	}
	var ack *collab.PatchAck
	var aerr error
	if p.retryConn != "" {
		// TASK-3585: only the connection that received it may resolve it.
		ack, aerr = s.collab.ApplyExternalPatchOn(item.ID, p.id, edits, p.retryConn)
		if errors.Is(aerr, collab.ErrNoApplierAvailable) || errors.Is(aerr, collab.ErrNoActiveRoom) {
			s.writePatchOutcomeUnknownFor(w, item, p)
			return contentRouteHandled, nil
		}
	} else {
		ack, aerr = s.collab.ApplyExternalPatch(item.ID, p.id, edits)
	}
	if aerr != nil {
		var unknown *collab.PatchOutcomeUnknownError
		if errors.As(aerr, &unknown) && p.idem != nil {
			// Recorded so a retry with the same key goes back to this
			// connection, whose tab re-acks a patch it applied.
			base := p.baseBody
			if p.retryConn == "" {
				base = item.Content
				p.baseSeq = item.Seq
			}
			if merr := s.store.MarkIdempotencyKeyUnknown(p.idem.workspaceID, p.idem.userID, p.idem.key, p.idem.token, p.id, unknown.ConnID, p.baseSeq, base); merr != nil {
				slog.Error("idempotency: could not record an unknown patch outcome", "item_id", item.ID, "error", merr)
			}
		}
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
	if p.retryConn == "" {
		p.baseSeq = item.Seq
	}
	p.before, p.after = ack.Before, ack.After
	// The tab applied the edits to its canonical before under the same rules;
	// the server re-derives each edit's context from that text so the record
	// can be undone. A mismatch would be a client defect: recorded, logged.
	if again, applied, rerr := bodypatch.ApplyWithContext(ack.Before, p.edits); rerr == nil {
		if again != ack.After {
			slog.Warn("collab: tab's patched body differs from the server's re-derivation", "item_id", item.ID, "patch_id", p.id)
		}
		_ = p.recordEdits(applied)
	}
	row := *input
	row.ContentTransform = nil
	row.Content = nil
	row.RefusePendingEdits = false
	row.ExternalContent = &ack.After
	if p.record.Edits == "" {
		row.PatchRecord = nil // nothing to undo from; the patch still stands
	}
	row.AfterWriteTx = nil
	if p.idem != nil {
		row.AfterWriteTx = s.completeIdempotencyTx(p, true)
	}
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
	case "anchor_missing", "anchor_ambiguous", "unstable_roundtrip", "parse_refused", "room_open", "empty_anchor", "outcome_unknown":
	default:
		reason = "other"
	}
	s.metrics.PatchRefusalsTotal.WithLabelValues(reason).Inc()
}

// claimPatchKey claims the request's idempotency key (TASK-3585) BEFORE any
// write-path guard, so a retry of a write that committed replays it rather
// than meeting its own first success as a conflict (lead ruling). It answers
// the request itself, returning false, when the key settles it: a replay, a
// key reused for a different request (422), or one in flight (409).
func (s *Server) claimPatchKey(w http.ResponseWriter, r *http.Request, req *patchRequest, p *itemPatch, key string) bool {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return false
	}
	itemSlug := chi.URLParam(r, "itemSlug")
	item, err := s.store.ResolveItem(workspaceID, itemSlug)
	if err != nil {
		writeInternalError(w, err)
		return false
	}
	if item == nil {
		s.writeItemResolveError(w, r, workspaceID, itemSlug)
		return false
	}
	if !s.requireItemVisible(w, r, workspaceID, item) || !s.requireEditPermission(w, r, workspaceID, item.ID, item.CollectionID) {
		return false
	}
	canon := *req
	canon.OpKey = ""
	body, _ := json.Marshal(canon)
	sum := sha256.Sum256([]byte(idempotencyRoutePatch + "\x00" + item.ID + "\x00" + string(body)))
	userID := currentUserID(r)
	c, err := s.store.ClaimIdempotencyKey(workspaceID, userID, key, idempotencyRoutePatch, hex.EncodeToString(sum[:]), idempotencyLease)
	if err != nil {
		writeInternalError(w, err)
		return false
	}
	switch c.Outcome {
	case store.IdemReplay:
		var core patchResponse
		if json.Unmarshal([]byte(c.Response), &core) != nil {
			writeInternalError(w, fmt.Errorf("stored idempotent answer does not decode"))
			return false
		}
		if core.Rebased && req.ExpectedSeq != nil {
			if iv, ierr := s.store.ItemSeqChain(item.ID, *req.ExpectedSeq, core.AppliedOverSeq, maxPatchIntervening); ierr == nil {
				core.Intervening = iv
			}
		}
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSON(w, c.Status, core)
		return false
	case store.IdemMismatch:
		writeError2(w, http.StatusUnprocessableEntity, "idempotency_key_reused",
			"this idempotency key was already used for a different request; use a new key", map[string]interface{}{"key": key})
		return false
	case store.IdemInFlight:
		w.Header().Set("Retry-After", "2")
		writeError2(w, http.StatusConflict, "idempotency_in_flight",
			"a request with this idempotency key is still in progress; retry shortly with the same key", map[string]interface{}{"key": key})
		return false
	case store.IdemUnknown:
		p.id = c.PatchID
		p.retryConn = c.ConnID
		p.baseSeq = c.BaseSeq
		p.baseBody = c.BaseBody
	}
	p.idem = &idemClaim{workspaceID: workspaceID, userID: userID, key: key, token: c.Token}
	return true
}

// writePatchOutcomeUnknownFor is writePatchOutcomeUnknown for a resolved item
// (the room path), reading the item fresh for the current seq and body.
func (s *Server) writePatchOutcomeUnknownFor(w http.ResponseWriter, item *models.Item, p *itemPatch) {
	cur, err := s.store.GetItem(item.ID)
	if err != nil || cur == nil {
		writeInternalError(w, fmt.Errorf("read item for an unknown patch outcome: %v", err))
		return
	}
	s.countPatchRefusal("outcome_unknown")
	ref := itemRefOrSlug(*cur)
	details := map[string]interface{}{
		"ref": ref, "patch_id": p.id, "seq": cur.Seq, "base_seq": p.baseSeq,
		"diff": diff.Unified(p.baseBody, cur.Content, 3),
	}
	if cur.ContentState != "" {
		details["content_state"] = cur.ContentState
	}
	writeError2(w, http.StatusConflict, "patch_outcome_unknown",
		ref+": an earlier attempt of this patch reached an editor that never confirmed it, and that editor is gone, so it is not re-applied. Read the diff since then: if your edit is there, it landed",
		details)
}

// writePatchOutcomeUnknown answers a retry of an unconfirmed patch whose
// connection is gone (TASK-3585, lead ruling): it is NOT re-planned (an
// insert-shaped edit keeps its anchor and would apply twice). The caller gets
// the current seq and the diff from the body the patch was sent against to
// now, and decides; the key stays unknown.
func (s *Server) writePatchOutcomeUnknown(w http.ResponseWriter, r *http.Request, p *itemPatch) {
	s.countPatchRefusal("outcome_unknown")
	workspaceID, _ := s.getWorkspaceID(w, r)
	item, err := s.store.ResolveItem(workspaceID, chi.URLParam(r, "itemSlug"))
	if err != nil || item == nil {
		writeInternalError(w, fmt.Errorf("resolve item for an unknown patch outcome: %v", err))
		return
	}
	ref := itemRefOrSlug(*item)
	details := map[string]interface{}{
		"ref":      ref,
		"patch_id": p.id,
		"seq":      item.Seq,
		"base_seq": p.baseSeq,
		"diff":     diff.Unified(p.baseBody, item.Content, 3),
	}
	if item.ContentState != "" {
		details["content_state"] = item.ContentState
	}
	writeError2(w, http.StatusConflict, "patch_outcome_unknown",
		ref+": an earlier attempt of this patch reached an editor that never confirmed it, and that editor is gone, so it is not re-applied. Read the diff since then: if your edit is there, it landed",
		details)
}
