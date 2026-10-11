package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Idempotency keys beyond patch (TASK-3585): create, append, copy, push. The
// patch route has its own claim (claimPatchKey) because its unknown outcome
// is routed to a connection; these routes have no such outcome, so they
// share this helper. The same rules: claimed BEFORE any write-path guard,
// answered inside the write's own transaction, refusals released.

// idemHold is a request's claim on its key, and how to finish it.
type idemHold struct {
	workspaceID, userID, key, token string
}

// completeTx records the write's answer in its transaction, guarded by the
// claim token; its error rolls the write back.
func (s *Server) completeTx(h *idemHold, status int, answer any) func(tx *sql.Tx) error {
	return func(tx *sql.Tx) error {
		b, err := json.Marshal(answer)
		if err != nil {
			return err
		}
		return s.store.CompleteIdempotencyKeyTx(tx, h.workspaceID, h.userID, h.key, h.token, status, string(b))
	}
}

// release gives up the claim; a no-op once the write recorded its answer.
func (s *Server) release(h *idemHold) {
	if h != nil {
		_ = s.store.ReleaseIdempotencyKey(h.workspaceID, h.userID, h.key, h.token)
	}
}

// claimKey claims key for (route, scope, canonical request). It answers the
// request itself and returns ok=false when the key settles it: a stored
// answer (handed to replay, which writes the response), a key reused for a
// different request (422), or one in flight (409). With no key it returns
// (nil, true).
func (s *Server) claimKey(w http.ResponseWriter, r *http.Request, workspaceID, route, scope string, canon any, key string, replay func(status int, stored string)) (*idemHold, bool) {
	if key == "" {
		return nil, true
	}
	body, _ := json.Marshal(canon)
	sum := sha256.Sum256([]byte(route + "\x00" + scope + "\x00" + string(body)))
	userID := currentUserID(r)
	c, err := s.store.ClaimIdempotencyKey(workspaceID, userID, key, route, hex.EncodeToString(sum[:]), idempotencyLease)
	if err != nil {
		writeInternalError(w, err)
		return nil, false
	}
	switch c.Outcome {
	case store.IdemReplay:
		w.Header().Set("Idempotent-Replayed", "true")
		replay(c.Status, c.Response)
		return nil, false
	case store.IdemMismatch:
		writeError2(w, http.StatusUnprocessableEntity, "idempotency_key_reused",
			"this idempotency key was already used for a different request; use a new key", map[string]interface{}{"key": key})
		return nil, false
	case store.IdemInFlight, store.IdemUnknown:
		// Unknown is a patch-only state; on these routes it reads as busy.
		w.Header().Set("Retry-After", "2")
		writeError2(w, http.StatusConflict, "idempotency_in_flight",
			"a request with this idempotency key is still in progress; retry shortly with the same key", map[string]interface{}{"key": key})
		return nil, false
	}
	return &idemHold{workspaceID: workspaceID, userID: userID, key: key, token: c.Token}, true
}

// idempotencyRouteCreate names the create route in a key's request hash.
const idempotencyRouteCreate = "item.create"

// createdAnswer is what a create's key stores: the item it created. A replay
// answers with that item as it stands now.
type createdAnswer struct {
	ItemID string `json:"item_id"`
}

// idempotencyRouteComment names the comment-create route in a key's request
// hash.
const idempotencyRouteComment = "comment.create"

// commentedAnswer is what a comment create's key stores: the comment it made.
type commentedAnswer struct {
	CommentID string `json:"comment_id"`
}

// idempotencyRouteAppend names the append-only item update in a key's
// request hash.
const idempotencyRouteAppend = "item.append"

// appendedAnswer is what an append's key stores: the item and the entries
// the write minted. A replay answers with the item as it stands now, echoing
// those entries.
type appendedAnswer struct {
	ItemID   string                         `json:"item_id"`
	Note     *models.ItemImplementationNote `json:"note,omitempty"`
	Decision *models.ItemDecisionLogEntry   `json:"decision,omitempty"`
}

// isAppendOnlyUpdate reports whether in carries nothing but appends (and
// source, which every CLI write stamps). It compares against the zero
// update so a member added to ItemUpdate later is covered without a list.
func isAppendOnlyUpdate(in models.ItemUpdate) bool {
	if in.AppendImplementationNote == nil && in.AppendDecision == nil {
		return false
	}
	in.AppendImplementationNote, in.AppendDecision, in.OpKey, in.Source = nil, nil, "", ""
	got, err1 := json.Marshal(in)
	zero, err2 := json.Marshal(models.ItemUpdate{})
	return err1 == nil && err2 == nil && string(got) == string(zero)
}

// replayAppend answers a replayed append: the item as it stands now, with
// the entries the first request appended. 410 when the item is gone.
// visIDs is the caller's visible collections, as the update's own response
// uses, so a restricted caller's replay names no item it cannot see.
func (s *Server) replayAppend(w http.ResponseWriter, r *http.Request, visIDs []string, status int, stored string) {
	var a appendedAnswer
	if json.Unmarshal([]byte(stored), &a) != nil {
		writeInternalError(w, fmt.Errorf("stored idempotent answer does not decode"))
		return
	}
	it, err := s.store.GetItem(a.ItemID)
	if err != nil || it == nil {
		writeError2(w, http.StatusGone, "idempotency_target_gone", "the item this key appended to no longer exists", map[string]interface{}{"item_id": a.ItemID})
		return
	}
	if err := s.enrichItemForResponse(r, it, visIDs); err != nil {
		writeInternalError(w, err)
		return
	}
	it.Appended = &models.ItemAppendedEntries{ImplementationNote: a.Note, Decision: a.Decision}
	writeJSON(w, status, it)
}
