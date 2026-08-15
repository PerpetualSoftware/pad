package server

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// pushRequest is the body of POST .../items/{itemSlug}/push.
type pushRequest struct {
	Message string `json:"message"`
	// TargetSessionID optionally narrows delivery to one of the caller's
	// OWN live sessions from the S1 presence registry (PLAN-2558 S5,
	// TASK-2588; GET /api/v1/sessions is where a caller learns the id).
	// Omitted (the pre-S5 shape) means broadcast to every one of the
	// caller's connected sessions, unchanged. API + TS client + web
	// picker only in this slice, per CONVE-1741 — no CLI flag, no MCP
	// surface; internal/cli's PushResult mirror simply never sends or
	// reads this field.
	TargetSessionID string `json:"target_session_id,omitempty"`
}

// pushResponse is the body of a successful push (dispatcher review round
// 2, codex P2: `pad push --format json` needs a real shape, not a
// discarded response). Workspace is included per the round-2 P1 fix's
// same rationale — the stream is user-scoped across every workspace the
// caller belongs to, so a JSON consumer needs it disambiguated same as
// the monitor line does.
type pushResponse struct {
	Ref       string `json:"ref"`
	Workspace string `json:"workspace"`
	// Pushed means "accepted and processed", not "delivered" — it is
	// true even when a TARGETED push's publish was skipped because the
	// id matched no live session (dispatcher ruling, TASK-2588 round 2:
	// broadcast-with-no-listeners has always returned exactly this shape
	// — true, with nothing to receive it — and a targeted miss is not
	// given a different contract just because DeliveredSessions can now
	// say more about it). DeliveredSessions is the delivery signal; do
	// not read Pushed as one.
	Pushed  bool   `json:"pushed"`
	Message string `json:"message"`
	// DeliveredSessions counts how many of the caller's own live sessions
	// (S1 presence registry, `target_session_id`-filtered if one was
	// given) matched — PLAN-2558 S5, TASK-2588. This is a PREDICTION read
	// from the registry, not a delivery receipt: it carries the exact
	// same staleness window as GET /api/v1/sessions (session_presence.go's
	// LiveSession doc comment — up to ~30s behind an ungracefully-dropped
	// connection) and there is still no ack from the receiving side. A
	// vanished or cross-user target_session_id is 0, the same as "nothing
	// connected" — deliberately not a distinct error, so the CLI's pre-S5
	// behavior is unchanged by construction.
	//
	// SNAPSHOTTED BEFORE THE PUBLISH, not after (dispatcher review round
	// 1, codex): counting post-publish raced the very thing it reports on
	// — a targeted session could receive the notification and then
	// disconnect before the count read, reporting 0 on a push that had
	// already landed exactly once. handlePushToItem reads presence FIRST
	// and, for a targeted push, skips the publish entirely when the
	// target isn't in that snapshot — see its doc comment — so a 0 here
	// is never a race, it's a guarantee: nothing was sent.
	DeliveredSessions int `json:"delivered_sessions"`
}

// maxPushTargetSessionIDLen bounds target_session_id (dispatcher review
// round 1, codex). It's trimmed but otherwise unvalidated — see its doc
// comment in pushRequest — and decodeJSON allows request bodies up to
// 2 MiB, so without a cap an authenticated caller could park arbitrarily
// large garbage strings in the bus's 1024-entry replay buffer on every
// push. 256 runes is comfortably above any id the S1 presence registry
// actually issues (a uuid.NewString() is 36) — a registry-issued id can
// never be rejected by this bound, so nothing a real client sends is
// ever affected; this exists purely to keep an unmatchable payload out
// of shared memory, not to constrain the id format (still opaque, still
// no format enforced beyond length).
const maxPushTargetSessionIDLen = 256

// maxPushMessageLen bounds a push's instruction text, measured in runes
// AFTER whitespace collapse (dispatcher review round 1). Two
// constraints in tension set this: a push message is a free-form
// instruction, not a short label — truncating one the way
// truncateForSummary shortens a comment preview would silently corrupt
// what the user actually asked for, and unlike a comment (whose full
// body is still fetchable via `pad item show`), a push has no
// persistence to recover the untruncated text from (see
// handlePushToItem's doc comment). But Notification.Summary rides a
// single stdout line into a plugin monitor / terminal session (`pad
// watch --stream --for-session`'s one-line-per-event wire contract), so
// it can't be unbounded either. 4096 runes gives several paragraphs of
// headroom — comfortably more than any reasonable single instruction —
// while keeping that one line a sane size; a message over the cap is
// rejected with a 400 rather than silently truncated.
const maxPushMessageLen = 4096

// handlePushToItem publishes a self-addressed watchevents.KindPush
// notification (IDEA-2544 Phase 1) — the "push this to my agent" verb:
// an explicit, user-authored instruction bound to an item, delivered to
// every one of the pushing user's OWN connected monitor sessions via
// GET /api/v1/events/stream, or to exactly one of them when the request
// names a target_session_id (PLAN-2558 S5, TASK-2588). Unlike watch/
// assignment notifications, this has no durable backing (Dave's product
// call: fire-and-forget is acceptable for v1 — no inbox, no "no session
// connected" warning; the bus's replay buffer is the only resilience a
// push gets).
//
// Self-addressed only: pushing into someone else's session is a consent
// question, not a code question (IDEA-2544 plan), so TargetUserID is
// always set to the CALLER's own ID, never a request-supplied target —
// there is no cross-user push. TargetSessionID (S5) does not relax this:
// it can only narrow delivery WITHIN the sessions ListForUser(userID)
// already scopes to, never address a session outside it — see
// deliveredSessionCount.
//
// POST /api/v1/workspaces/{slug}/items/{itemSlug}/push
func (s *Server) handlePushToItem(w http.ResponseWriter, r *http.Request) {
	if s.watchEvents == nil {
		// Unlike handleCreateWatch (a durable store write that works fine
		// without the bus), a push has NO persistence to fall back on —
		// if there's no bus, the message is unrecoverably lost. Fail
		// loudly here rather than returning 200 for an instruction that
		// silently went nowhere.
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Push is not available")
		return
	}

	// Full workspace object, not just the ID (getWorkspaceID), so the
	// response can echo the CANONICAL slug — the caller may have passed
	// an ID in the URL, and pushResponse.Workspace exists specifically
	// to disambiguate which workspace a JSON consumer should resolve the
	// ref against (same rationale as the monitor line's workspace
	// prefix), so it needs to be the real slug, not an echo of whatever
	// the URL happened to contain.
	ws, ok := s.getWorkspace(w, r)
	if !ok {
		return
	}
	workspaceID := ws.ID

	itemSlug := chi.URLParam(r, "itemSlug")
	item, err := s.store.ResolveItem(workspaceID, itemSlug)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if item == nil {
		s.writeItemResolveError(w, r, workspaceID, itemSlug)
		return
	}
	if !s.requireItemVisible(w, r, workspaceID, item) {
		return
	}

	userID := currentUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}

	var input pushRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Collapse newlines to spaces, matching truncateForSummary's
	// rationale in handlers_comments.go: Notification.Summary is a
	// single-line wire contract (`pad watch --stream --for-session`
	// prints exactly one stdout line per event). Trimmed BEFORE the
	// empty check so a whitespace-only -m ("   ") is rejected the same
	// as a genuinely empty one, rather than publishing a blank
	// instruction.
	message := strings.Join(strings.Fields(input.Message), " ")
	if message == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "message must not be empty")
		return
	}
	if length := len([]rune(message)); length > maxPushMessageLen {
		writeError(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("message must be %d characters or fewer after whitespace collapse (got %d)", maxPushMessageLen, length))
		return
	}

	actor, _ := actorFromRequest(r)
	actorName := actorNameFromRequest(r)
	// Trimmed, not otherwise validated beyond the length cap below: a
	// session id is opaque to this handler (see deliveredSessionCount and
	// Notification.TargetSessionID) — an id that names no live session of
	// userID's just matches nothing, there is no format to enforce.
	targetSessionID := strings.TrimSpace(input.TargetSessionID)
	if length := len([]rune(targetSessionID)); length > maxPushTargetSessionIDLen {
		writeError(w, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("target_session_id must be %d characters or fewer (got %d)", maxPushTargetSessionIDLen, length))
		return
	}

	// Presence is read BEFORE the publish, not after (dispatcher review
	// round 1, codex — see DeliveredSessions' doc comment for the race a
	// post-publish count had). For a TARGETED push whose id isn't in this
	// snapshot, the publish is skipped entirely: session ids are
	// per-connection and never reused (session_presence.go's
	// MemorySessionPresence.Add mints a fresh uuid per Add call), so a
	// target absent right now can never later be matched by the SAME
	// connection reconnecting under that id, nor by the bus's replay
	// buffer (which only serves a resumed connection presenting its own
	// prior Last-Event-ID, not an arbitrary target id). The notification
	// would therefore be a guaranteed no-op — skipping it is what makes
	// delivered_sessions=0 an honest guarantee rather than a snapshot
	// that a slower reader could still race. Broadcast (targetSessionID
	// == "") keeps the original fire-and-forget posture and always
	// publishes, same as pre-S5 — its count is a pre-publish snapshot of
	// who's connected, not a promise that count still holds by the time
	// delivery happens, which is the same staleness every presence
	// answer on this surface already carries.
	deliveredSessions := deliveredSessionCount(s.sessionPresence, userID, targetSessionID)
	if targetSessionID == "" || deliveredSessions > 0 {
		s.watchEvents.Publish(watchevents.Notification{
			WorkspaceID:     workspaceID,
			ItemID:          item.ID,
			CollectionID:    item.CollectionID,
			ItemRef:         item.Ref,
			Kind:            watchevents.KindPush,
			Actor:           actor,
			ActorName:       actorName,
			Summary:         message,
			TargetUserID:    userID,
			TargetSessionID: targetSessionID,
		})
	}

	writeJSON(w, http.StatusOK, pushResponse{
		Ref:               item.Ref,
		Workspace:         ws.Slug,
		Pushed:            true,
		Message:           message,
		DeliveredSessions: deliveredSessions,
	})
}

// deliveredSessionCount answers "how many of userID's own live sessions
// will this push's delivery predicate match?" (PLAN-2558 S5, TASK-2588).
// It reads the SAME self-scoped list session_presence.go's
// SessionPresence.ListForUser already restricts every other consumer to
// (handlers_sessions.go's GET /api/v1/sessions is the other one) — that
// scoping is what makes a targetSessionID belonging to a DIFFERENT user
// structurally indistinguishable from a vanished one: it is simply never
// in userID's own list, so it falls out to 0 without any cross-user
// lookup or special-casing here.
//
// A nil presence (no registry wired) answers 0 rather than guessing —
// consistent with handleListSessions' own refusal to report an empty
// list as "nobody connected" when it genuinely cannot tell; here there
// is no error channel to say "can't tell" through (pushResponse.Pushed
// is still true — the notification really was published), so 0 is the
// closest honest answer available.
func deliveredSessionCount(presence SessionPresence, userID, targetSessionID string) int {
	if presence == nil {
		return 0
	}
	sessions := presence.ListForUser(userID)
	if targetSessionID == "" {
		return len(sessions)
	}
	for _, sess := range sessions {
		if sess.ID == targetSessionID {
			return 1
		}
	}
	return 0
}
