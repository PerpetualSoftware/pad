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
	// OWN live sessions from the S1 presence registry — narrows, within a
	// set already limited to ARMED sessions that can see the item (PLAN-2558 S5,
	// TASK-2588; GET /api/v1/sessions is where a caller learns the id).
	// Omitted (the pre-S5 shape) means broadcast to every session in that
	// same set — armed, item-visible, the caller's own — unchanged. API + TS client + web
	// picker only in this slice, per CONVE-1741 — no CLI flag and no MCP
	// surface for SETTING it.
	//
	// The read side is no longer true and was corrected in place rather
	// than left as archaeology (codex round 10): internal/cli's PushResult
	// now mirrors delivered_sessions, because `pad push --format json` was
	// silently dropping a field this response documents at length. It
	// still never SENDS target_session_id.
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
	// same staleness windows as GET /api/v1/sessions (session_presence.go's
	// LiveSession doc comment — up to ~30s behind an ungracefully-dropped
	// CLIENT, and on a Redis-backed deployment up to ~90s behind a dead
	// server INSTANCE) and there is still no ack from the receiving side. A
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
	// THAT GUARANTEE USED TO BE PER-PROCESS, which made this count wrong
	// in BOTH directions for a BROADCAST push once BUG-2651 gave the bus
	// cross-instance reach: the count came from the answering instance's
	// presence registry while the bus delivered everywhere. With one armed
	// session on each of two replicas the answering replica reported 1 and
	// two received it; with none of its own it reported 0 while a remote
	// session received it anyway.
	//
	// CLOSED BY BUG-2698 for the INSTANCE-LOCALITY half: with PAD_REDIS_URL
	// set, s.sessionPresence is the shared RedisSessionPresence, so this
	// count is read from every instance's sessions rather than one
	// instance's. It is still a PREDICTION with the staleness above — a
	// shared registry does not make it a receipt.
	//
	// STILL AN UPPER BOUND, not a match count, and an earlier draft of this
	// comment claimed otherwise (codex round 6). deliveredSessionCount
	// filters on user, armed, and target id. Actual delivery ALSO applies
	// each stream's own visibility — watchNotificationVisible's
	// vis.allows(CollectionID, ItemID), computed per connection from the
	// credentials that opened it. Two streams of the SAME user can differ
	// there: a cookie session and a workspace-scoped API token do not see
	// the same collections. So a session counted here can still drop the
	// push, and a targeted one at such a session is published and lost
	// while this field reports 1.
	//
	// Pre-existing and unchanged by BUG-2698 — the in-process count had the
	// identical blind spot — but tracked now rather than left implied
	// (BUG-2725). Fixing it means the registry carrying per-session
	// visibility, which has its own staleness question, since access can be
	// revoked while the connection is held open.
	//
	// Note this was never wrong in the over-reporting direction for a
	// TARGETED push, for the unhappy reason that the same locality stopped
	// those from being published at all.
	//
	// NULL MEANS "PUBLISHED, COUNT UNKNOWN" — never "zero" (codex round 1
	// P1). It is emitted only when the presence registry could not be READ
	// (a Redis outage) on a BROADCAST push: the publish still happened,
	// because presence never gated a broadcast, but there is no honest
	// number to put here and 0 would claim nobody received it. A TARGETED
	// push in that state is refused with a 503 instead and never reaches
	// this struct, so a null is always a broadcast.
	//
	// Consumers: treat null as "unknown", not as falsy. Both clients carry
	// it — internal/cli's PushResult mirrors it as a *int (untagged, so
	// `pad push --format json` prints an explicit null rather than
	// omitting the key), and the web's type is `number | null | undefined`
	// with all three states documented there. An ABSENT key is a third
	// thing again: a server predating session targeting.
	DeliveredSessions *int `json:"delivered_sessions"`
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
// each of the pushing user's OWN monitor sessions that is ACCEPTING pushes
// and can see the item — a connected session that hasn't opted in, or that
// lacks access, does not receive it — via
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
	// per-connection and never reused (both SessionPresence
	// implementations mint a fresh uuid per Add call), so a
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
	// THAT PREMISE BROKE ONCE AND IS NOW RESTORED, which is worth spelling
	// out because the skip below looks like a bug at a glance. It rested
	// on the bus being in-process: a target THIS instance could not see
	// was, by construction, a target nobody could deliver to. BUG-2651's
	// RedisBus made the notification reach every instance, at which point
	// a session held on another one WOULD have matched it — and this skip
	// was the only thing stopping it, turning a deliverable push into the
	// no-op the comment describes (BUG-2698).
	//
	// BUG-2698 fixed it at the registry rather than here. With
	// PAD_REDIS_URL set, s.sessionPresence is the shared
	// RedisSessionPresence, so "absent from this snapshot" means absent
	// from EVERY instance again, and the skip is correct for exactly the
	// reason it was originally written. Note that the shortcut — publish
	// unconditionally for targeted pushes — would have fixed delivery and
	// immediately made delivered_sessions=0 a lie in the other direction.
	// The line below is deliberately unchanged by that fix.
	//
	// One detail the "never reused" argument now leans on more heavily:
	// both implementations mint a fresh uuid per Add
	// (MemorySessionPresence.Add, RedisSessionPresence.Add), so a
	// reconnecting client never returns under a previous id on either.
	deliveredSessionsUnknown := false
	deliveredSessions, presenceErr := deliveredSessionCount(s.sessionPresence, userID, targetSessionID,
		s.pushSessionVisibility(userID, workspaceID, item.CollectionID, item.ID))
	if presenceErr != nil {
		// PRESENCE GATES A TARGETED PUSH BUT ONLY COUNTS A BROADCAST, and
		// that asymmetry is exactly why an unreadable registry gets two
		// different answers rather than one uniform refusal (codex round 1
		// P1; dispatcher ruling).
		//
		// TARGETED: the gate cannot be evaluated at all. Publishing would
		// deliver but report a count we do not have; skipping silently is
		// the original bug. So refuse — 503 `unavailable`, nothing
		// published, which is true and is already the code the web client
		// treats as safe to resend.
		if targetSessionID != "" {
			slog.Warn("push refused: cannot read session presence to evaluate the target",
				"item_ref", item.Ref, "error", presenceErr)
			writeError(w, http.StatusServiceUnavailable, "unavailable",
				"Push is not available right now — the target session could not be resolved, so nothing was sent")
			return
		}
		// BROADCAST: presence never gated this — the publish is
		// unconditional and always has been. Refusing here would
		// manufacture an outage for a delivery that would have succeeded,
		// coupling push availability to presence availability, which is a
		// dependency that does not otherwise exist. Publish, and report the
		// count as UNKNOWN rather than as 0.
		slog.Warn("push: session presence unreadable, broadcasting with an unknown delivery count",
			"item_ref", item.Ref, "error", presenceErr)
		deliveredSessionsUnknown = true
	}
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

	resp := pushResponse{
		Ref:       item.Ref,
		Workspace: ws.Slug,
		Pushed:    true,
		Message:   message,
	}
	if !deliveredSessionsUnknown {
		resp.DeliveredSessions = &deliveredSessions
	}
	writeJSON(w, http.StatusOK, resp)
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
//
// UNREADABLE PRESENCE IS NOT ZERO (codex round 1, P1). The error is
// returned rather than folded into a 0, because 0 is a load-bearing
// answer on this path: it makes the caller SKIP the publish for a
// targeted push. Reporting 0 for a registry that could not be read would
// therefore drop the instruction and answer success — the same defect
// BUG-2698 filed, arriving through the fix for it. A nil registry still
// yields (0, nil): that is a server built without presence, which is a
// known configuration rather than an unknown state.
// ITEM VISIBILITY (BUG-2725). The three filters above were only three
// of delivery's FOUR gates. watchNotificationVisible checks
// vis.allows(CollectionID, ItemID) FIRST, before armed and before
// TargetUserID, and this function did not — so it counted sessions
// whose stream would drop the notification on visibility. Broadcast
// over-reported; a TARGETED push was worse, since the publish-skip below
// reads this count: the gate passed, the push went out, the stream
// dropped it, and the response said delivered_sessions: 1. An
// instruction lost behind a success.
//
// Dave ruled (day-49) that visibility is RE-RESOLVED here rather than
// snapshotted into the registry, for the reason watchNotificationVisible
// re-reads it per notification: membership and grants are revocable, and
// a value cached at connect goes wrong exactly when revocation is what
// makes it matter.
//
// THE COST OBJECTION IS RETIRED STRUCTURALLY, NOT ARGUED. "Re-resolve
// per counted session" reads like N access checks per push. It is at
// most TWO, and visibleFor is what makes that true by construction
// rather than by careful calling. The enumeration behind the bound
// (receipt, so a successor can re-derive it instead of trusting it):
// computeWatchAccessVisibility's inputs are platform role, workspace
// membership, CollectionAccess, GuestVisibleResources,
// GetMemberCollectionAccess, ListSystemCollectionIDs — all per-USER,
// identical for every session this function counts, since it only ever
// counts one user's own sessions — plus bearerAuth, which is
// per-CONNECTION and the sole varying input. One varying boolean means
// at most two distinct answers, so visibleFor memoizes both and the
// loop cannot ask for a third however many sessions it walks.
//
// WHAT THIS STILL DOES NOT FIX, stated rather than implied: the
// UNDER-count. A stream past maxSessionsPerUser (session_presence_redis.go)
// receives broadcasts while never entering the registry, so it is
// invisible here no matter how exactly this predicate is reproduced.
// delivered_sessions remains an ESTIMATE with error in both directions,
// and every consumer-facing description of it says so. Dave's ruling
// scoped this unit to the over-side.
func deliveredSessionCount(presence SessionPresence, userID, targetSessionID string, visibleFor *sessionVisibility) (int, error) {
	if presence == nil {
		return 0, nil
	}
	sessions, err := presence.ListForUser(userID)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, sess := range sessions {
		if !sess.Armed {
			continue
		}
		if targetSessionID != "" && sess.ID != targetSessionID {
			continue
		}
		// Ordered LAST among the filters deliberately: it is the only one
		// that can cost a store round trip, and armed/target reject most
		// non-matching sessions for free. The ORDER is an optimization;
		// the SET of filters is the contract.
		visible, err := visibleFor.allows(sess.BearerAuth)
		if err != nil {
			// AN UNRESOLVABLE VISIBILITY IS NOT "NOT VISIBLE" (codex round
			// 1, P1). The first version of this fix swallowed the store
			// error and returned false, reasoning that over-counting was
			// the defect being fixed so uncertainty should resolve
			// downward. That argument optimizes the wrong axis, and this
			// function's own doc comment above already says why: 0 is
			// load-bearing here, because a targeted push reporting 0 SKIPS
			// the publish. Resolving a DB blip to "not visible" would
			// therefore drop the instruction and answer success — the same
			// defect BUG-2698 filed, arriving a second time through the fix
			// for BUG-2725. The established policy is to propagate, so the
			// caller can 503 a targeted push and report a broadcast's count
			// as unknown; this now does that rather than inventing a
			// narrower rule for the same situation.
			return 0, err
		}
		if !visible {
			continue
		}
		count++
	}
	return count, nil
}

// sessionVisibility answers "would a stream on this transport, held by
// this user, see this notification's item?" — and answers it at most
// twice, once per possible transport, however many times it is asked
// (BUG-2725).
//
// The memo is the point, not an optimization detail. It is what makes
// deliveredSessionCount's ≤2-resolutions bound a property of the type
// rather than a discipline the caller has to maintain: a future edit
// that moves the allows() call, adds a second loop, or counts sessions
// twice cannot turn this into N resolutions.
//
// A nil *sessionVisibility allows everything. That is for callers with
// no notification in hand (there are none on the push path today) and
// for tests exercising the armed/target filters in isolation; it is
// deliberately NOT the behaviour any production call site gets, and
// there is a test asserting the push handler passes a real one.
//
// The error is memoized alongside the answer, so a failing resolve is
// not retried once per session — a store that is down stays down for
// the length of one push, and asking it twice per transport would only
// multiply the latency of a request that is going to fail anyway.
type sessionVisibility struct {
	// resolve is called at most once per distinct transport.
	resolve func(bearerAuth bool) (bool, error)
	cached  [2]bool
	err     [2]error
	done    [2]bool
}

func (v *sessionVisibility) allows(bearerAuth bool) (bool, error) {
	if v == nil || v.resolve == nil {
		return true, nil
	}
	i := 0
	if bearerAuth {
		i = 1
	}
	if !v.done[i] {
		v.cached[i], v.err[i] = v.resolve(bearerAuth)
		v.done[i] = true
	}
	return v.cached[i], v.err[i]
}

// resolutions reports how many distinct transports have actually been
// resolved. Exists for the test that pins the ≤2 bound — an assertion
// that the bound HOLDS is worth more than a comment claiming it does,
// and this is the only way to observe it from outside.
func (v *sessionVisibility) resolutions() int {
	if v == nil {
		return 0
	}
	n := 0
	for _, d := range v.done {
		if d {
			n++
		}
	}
	return n
}

// pushSessionVisibility builds the re-resolving predicate
// deliveredSessionCount applies, reproducing watchNotificationVisible's
// FIRST gate exactly: the same computeWatchAccessVisibility, the same
// allows(collectionID, itemID), for the same user, in the notification's
// own workspace.
//
// The user is re-fetched rather than taken from the pushing request's
// context, mirroring watchVisCache.refreshUser: a disabled or deleted
// user's streams must not be counted as reachable.
//
// TWO OUTCOMES THAT LOOK ALIKE AND ARE NOT (codex round 1, P1):
//
//   - A user who is definitively GONE or DISABLED resolves to "not
//     visible". That is an answer, and it is the right one — such a
//     caller's streams are not going to receive anything.
//   - A store that could not be READ returns an ERROR, which
//     deliveredSessionCount propagates so the caller can 503 a targeted
//     push and mark a broadcast's count unknown. Collapsing this into
//     "not visible" would make a DB blip skip the publish and answer
//     success, which is BUG-2698's defect reappearing through BUG-2725's
//     fix. The registry-unreadable branch a few lines up already draws
//     exactly this line; this follows it rather than inventing a second,
//     narrower rule for the same situation.
//
// KNOWN, BOUNDED DISAGREEMENT WITH THE STREAM (codex round 1, P1, and
// accepted rather than fixed — see the PR for the ruling). This resolves
// visibility FRESH, while a live stream reads watchVisCache, which
// refreshes on its own reval tick. So for at most one tick the two can
// differ:
//
//   - access GRANTED within the tick: counted visible here, still denied
//     on the stream, so a broadcast transiently over-reports — the
//     original defect, now bounded to one tick instead of unbounded;
//   - access REVOKED within the tick: counted invisible here while the
//     stream would still accept, so a targeted push is skipped. This
//     direction is NEW with this change, and it is the safer of the two:
//     it declines to dispatch an instruction into a session that is
//     losing access, and it reports 0 rather than claiming delivery.
//
// Closing the gap would mean reading each live connection's cache state,
// which is per-connection and in-process (and cross-instance under
// Redis) — not reachable from here. Documented with its bound instead of
// papered over.
func (s *Server) pushSessionVisibility(userID, workspaceID, collectionID, itemID string) *sessionVisibility {
	return &sessionVisibility{
		resolve: func(bearerAuth bool) (bool, error) {
			user, err := s.store.GetUser(userID)
			if err != nil {
				slog.Warn("push: could not read the pushing user for visibility",
					"user_id", userID, "error", err)
				return false, err
			}
			if user == nil || user.IsDisabled() {
				return false, nil
			}
			return s.computeWatchAccessVisibility(bearerAuth, user, workspaceID).allows(collectionID, itemID), nil
		},
	}
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
