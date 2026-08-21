package server

import (
	"errors"
	"fmt"
	"log/slog"
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
	// behavior is unchanged by construction. Since PLAN-2613 S1, an
	// UNARMED session is counted the same as a vanished/cross-user one —
	// present in the registry but not counted here — because it cannot
	// actually receive a push either (see deliveredSessionCount). The
	// field name and wire shape are unchanged; only which registry
	// entries qualify.
	//
	// SNAPSHOTTED BEFORE THE PUBLISH, not after (dispatcher review round
	// 1, codex): counting post-publish raced the very thing it reports on
	// — a targeted session could receive the notification and then
	// disconnect before the count read, reporting 0 on a push that had
	// already landed exactly once. handlePushToItem reads presence FIRST
	// and, for a targeted push, skips the publish entirely when the
	// target isn't in that snapshot — see its doc comment — so a 0 here
	// is never a race, it's a guarantee: nothing was sent.
	//
	// THAT GUARANTEE IS PER-PROCESS, and in a Redis-backed deployment
	// that makes this count wrong in BOTH directions for a BROADCAST push
	// (BUG-2651, codex round 3). The count comes from the local presence
	// registry while the bus now delivers everywhere: with one armed
	// session on each of two replicas, the replica handling the POST
	// reports 1 and two sessions receive it; with none of its own, it
	// reports 0 while a remote session receives it anyway.
	//
	// Filed as BUG-2698 alongside the targeted-push half. Not corrected
	// here, because the fix is not a better count — it is the shared-state
	// SessionPresence that PLAN-2558 S3 already gates on. Any local arithmetic would just be a more elaborate way of
	// asking one replica what all of them are doing. Targeted pushes do
	// not have the over-report half of this problem, for the unhappy
	// reason that the same locality stops them being published at all.
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
	//
	// THE "GUARANTEED NO-OP" PREMISE IS NOW MEMORYBUS-ONLY (BUG-2651,
	// codex round 2). It rested on the bus being in-process: a target
	// this instance cannot see was, by construction, a target nobody
	// could deliver to. With watchevents.RedisBus the notification would
	// reach every instance, so a session held on another one WOULD match
	// it — and this skip is what stops that, turning a deliverable push
	// into the no-op the comment describes rather than merely declining
	// to record one.
	//
	// Deliberately NOT changed here. Publishing unconditionally would fix
	// targeted cross-instance delivery and immediately make the
	// delivered_sessions=0 in the response a lie in the other direction,
	// which is a question about what that field promises rather than a
	// bug in this line. It belongs with the shared-state SessionPresence
	// that PLAN-2558 S3 already gates on — fixing the registry makes the
	// snapshot right, and then this skip is correct again for the same
	// reason it was originally.
	deliveredSessions := deliveredSessionCount(s.sessionPresence, userID, targetSessionID)
	if targetSessionID == "" || deliveredSessions > 0 {
		// The ONE direct Bus.Publish call in this package, and the only
		// one that acts on the result (BUG-2699 — see
		// publishWatchNotification's doc comment for the six that
		// deliberately don't, and publishSitesAreRuled_test.go for the
		// check that keeps that split true).
		//
		// A push has no durable backing whatsoever: no inbox, nothing to
		// read back, not even a store row carrying the same fact the way
		// a comment notification has. If the publish is refused, the
		// instruction is gone, and the caller is the only one who can do
		// anything about that.
		if err := s.watchEvents.Publish(watchevents.Notification{
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
		}); err != nil {
			writePushPublishError(w, err, item.Ref)
			return
		}
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
// ARMED-ONLY (PLAN-2613 S1). A session that hasn't declared armed=true
// can never actually receive a KindPush notification —
// watchNotificationVisible denies it server-side regardless of
// TargetUserID/TargetSessionID (handlers_watch_events.go). Counting an
// unarmed session here would make this a PREDICTION of what the
// registry looks like rather than of what will actually be delivered,
// which is the exact honesty gap this function exists to close (see its
// name). Filtering here is deliberately the SAME shape as the existing
// vanished/cross-user misses: a targetSessionID naming a real, connected,
// but unarmed session is not distinguished from one naming no session at
// all — both fall out to a plain 0 with no separate error, since from
// the caller's point of view "there but not listening for this" and
// "not there" are the same actionable answer (nothing was delivered).
// The publish-skip logic below reads this same filtered count, so a
// targeted push at an unarmed session is skipped for the identical
// reason a targeted push at a vanished one is: a guaranteed no-op.
func deliveredSessionCount(presence SessionPresence, userID, targetSessionID string) int {
	if presence == nil {
		return 0
	}
	sessions := presence.ListForUser(userID)
	count := 0
	for _, sess := range sessions {
		if !sess.Armed {
			continue
		}
		if targetSessionID != "" && sess.ID != targetSessionID {
			continue
		}
		count++
	}
	return count
}

// pushPublishUnconfirmedCode is the error code for a push whose publish
// failed in a way that does NOT prove the notification went nowhere
// (BUG-2699).
//
// It is deliberately absent from the web client's
// PUSH_PRE_PUBLISH_ERROR_CODES allow-list (web/src/lib/push/dispatch.ts),
// and that absence is the entire behaviour: isPrePublishRefusal routes
// every unrecognised code to the dialog's outcome-unknown branch, whose
// copy is "we can't tell whether this was sent — pushing twice would
// deliver it twice", and which does NOT re-arm the send button. That is
// the honest UI for this case, so the code must stay off that list. Do
// not "tidy" it on there.
const pushPublishUnconfirmedCode = "push_unconfirmed"

// writePushPublishError maps a Bus.Publish failure onto the response,
// keeping the two outcomes Bus.Publish distinguishes distinguishable all
// the way to the caller (BUG-2699). Collapsing them is the actual hazard
// here: one of them is safe to resend and the other can duplicate a
// dispatch into an agent harness.
//
//   - ErrBusClosed — the bus was already shut down, so nothing was
//     published and nothing could have been. 503 `unavailable`, the SAME
//     code the nil-bus branch at the top of handlePushToItem already
//     returns, because the caller's situation is identical: push is not
//     available right now, nothing went out, try again against a live
//     server. Reusing that code is also what makes the web surface
//     correct with no change at all — `unavailable` is already on its
//     pre-publish-refusal allow-list.
//   - anything else — UNCONFIRMED. go-redis retries a command whose
//     reply was lost to a network error (the reason RedisBus's publish
//     script carries a dedupe token at all), so the script may have run
//     and published while the call still returned non-nil. 502 with
//     pushPublishUnconfirmedCode, which is deliberately NOT on that
//     allow-list.
//
// Neither branch writes a pushResponse. `Pushed` documents itself as
// "accepted and processed", which is exactly what did not happen — and a
// 200 body with pushed:false would let a caller that reads only the
// status code (the CLI's `pad push` does: cmd_push.go returns on the
// error and prints nothing otherwise) treat a lost instruction as sent.
func writePushPublishError(w http.ResponseWriter, err error, itemRef string) {
	if errors.Is(err, watchevents.ErrBusClosed) {
		slog.Warn("push refused: notification bus is closed, nothing was published",
			"item_ref", itemRef, "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"Push is not available right now — the notification was not sent")
		return
	}
	slog.Error("push publish failed with an unconfirmed outcome — the notification may or may not have been delivered",
		"item_ref", itemRef, "error", err)
	writeError(w, http.StatusBadGateway, pushPublishUnconfirmedCode,
		"The push could not be confirmed — it may or may not have been delivered. "+
			"Check your agent session before sending it again; pushing twice would deliver it twice.")
}
