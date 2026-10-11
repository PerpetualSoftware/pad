package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

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
	userID := currentUserID(r)
	c, err := s.store.ClaimIdempotencyKey(workspaceID, userID, key, route, idemRequestHash(route, scope, canon), idempotencyLease)
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

// idempotencyRouteCopy names the cross-workspace copy route in a key's
// request hash. Its answer is the whole first ItemCopyResult: the warnings
// it carries (dropped fields, attachments cloned) cannot be recomputed later.
const idempotencyRouteCopy = "item.copy"

// idempotencyRoutePush names the push route in a key's request hash.
const idempotencyRoutePush = "item.push"

// pushOutcomeUnknownCode answers a same-key retry of a push whose publish
// could not be confirmed (TASK-3585): it may have been delivered, so it is
// not published again.
const pushOutcomeUnknownCode = "push_outcome_unknown"

// pushOutcomeUnknownEnvelope is the stored answer for that retry, in the
// API's error envelope.
func pushOutcomeUnknownEnvelope(ref string) any {
	return map[string]any{"error": map[string]any{
		"code": pushOutcomeUnknownCode,
		"message": "An earlier push with this key could not be confirmed; it may or may not have been delivered, so it is not sent again. " +
			"Check your agent session; to send it anyway, use a NEW key.",
		"details": map[string]any{"ref": ref},
	}}
}

// completeKey records an answer for a write that has no transaction of its
// own (push). A failure is logged, not returned: the write already
// happened, and the push_id still lets a monitor drop a re-publish.
func (s *Server) completeKey(h *idemHold, status int, answer any) {
	b, err := json.Marshal(answer)
	if err == nil {
		err = s.store.CompleteIdempotencyKey(h.workspaceID, h.userID, h.key, h.token, status, string(b))
	}
	if err != nil {
		slog.Warn("idempotency key not completed after a write with no transaction", "key", h.key, "error", err)
	}
}

// pushIDFor is the push_id a publish carries (TASK-3585). With a key it is
// derived from (workspace, user, key), so a re-publish after a claim
// takeover carries the same id; without one it is fresh.
func pushIDFor(workspaceID, userID, key string) string {
	if key == "" {
		return uuid.NewString()
	}
	sum := sha256.Sum256([]byte(workspaceID + "\x00" + userID + "\x00" + key))
	return "push_" + hex.EncodeToString(sum[:16])
}

// idemRequestHash is the request half of a key's identity: the route, its
// scope (the item) and the canonical request body.
func idemRequestHash(route, scope string, canon any) string {
	body, _ := json.Marshal(canon)
	sum := sha256.Sum256([]byte(route + "\x00" + scope + "\x00" + string(body)))
	return hex.EncodeToString(sum[:])
}
