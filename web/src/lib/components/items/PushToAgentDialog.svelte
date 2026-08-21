<!--
@component
PushToAgentDialog — compose an instruction about an item and push it to the
user's own connected agent sessions (PLAN-2558 S3, IDEA-2544 Phase 3), or to
one specific session (PLAN-2558 S5, TASK-2588) via the target picker below
the presence line.

THE TARGET PICKER REUSES THE PRESENCE READ, IT DOESN'T ADD ONE. `sessions`
below is already fetched for the presence line's own count; the picker's
options are that same array, so there is no second `GET /api/v1/sessions`
and the two surfaces can never disagree about who's connected. A targeted
send whose `delivered_sessions` comes back 0 (the addressed session vanished
between the last poll and the click — the same staleness window the presence
line already carries) toasts and re-polls rather than closing: zero delivery
means nothing was sent, so nothing is duplicated by trying again against a
freshly-read list.

Built on the shared `Modal` primitive, same as CopyItemDialog: the composer
needs more room than the pane menu's drill-down affords, and the native
`<dialog>` supplies focus trap + Escape + top-layer. Consumer obligations from
Modal's docs are honored — it is NOT wrapped in `{#if open}`, and `labelledby`
points at the heading.

THE POINT OF THIS DIALOG IS THE PRESENCE LINE, NOT THE TEXTAREA. `pad push` is
fire-and-forget: no durable inbox, no ack, no "nobody was listening" warning
(handlers_push.go — Dave's product call, and defensible for a CLI verb typed by
someone who knows their own session is running). A button in a web UI has no
such user. So this dialog answers "is anything ACCEPTING?" BEFORE the click, and
words the answer at exactly the confidence the server can support.

CONNECTED IS NOT ACCEPTING (PLAN-2613 S4, D3). Only an armed session receives a
push — the server filters delivery to armed sessions — so every count and gate
below keys on the ACCEPTING (armed) subset, not the raw connected list, and the
line shows the split honestly rather than hiding the difference:

  - accepting > 0 → "M sessions accepting pushes" (and "of N connected" when
                   some are connected-but-unarmed). NOT "this will be
                   delivered": the registry can name a session that is already
                   gone (two bounds — see this file's DEPLOYMENT CONSTRAINT
                   note), and even a live armed session gets no delivery
                   receipt. Send is enabled; the caveat is
                   stated, not implied.
  - accepting == 0 → send is DISABLED, because a push to no armed session is
                   definitively lost (no inbox), and a push to a connected-but-
                   unarmed session is dropped server-side — sending either way
                   would be the silent fire-and-forget D3 forbids. Two empty
                   states: nobody connected at all ("start a session"), or
                   sessions connected but none opted in ("run /pad:connect to
                   enable"). Both offer the clipboard instead (the same fallback
                   PLAN-2558 S4 rules for quick actions), so the surface is
                   never a dead end.
  - can't tell   → send is ENABLED, with the uncertainty stated. A 503 (no
                   presence registry wired) or a network failure means the
                   server cannot answer, and rendering that as zero is the
                   precise lie handleListSessions refuses to tell by returning
                   503 rather than an empty list. Blocking the user on our own
                   inability to check would inherit the same false confidence
                   in the other direction.

The bound is enforced client-side (`$lib/push/message`) against the server's
own rune-after-collapse accounting, so an over-length message is caught in the
composer rather than coming back as a 400.

DEPLOYMENT CONSTRAINT, LIFTED (BUG-2698) — kept here because the copy below
was written under it. This dialog used to carry a warning that the presence
registry and the event bus were both per-PROCESS, so behind more than one padd
process a load balancer could route the presence GET and the push POST to
different instances and every claim on this surface — including "No agent
session is connected" — could be wrong. BUG-2651 made the bus shared and
BUG-2698 made the registry shared, both on PAD_REDIS_URL, so a session
connected to any instance is now visible and addressable from any other.

What that changes for this file is nothing structural, which is the point: the
copy below was deliberately written for the single-process case rather than
hedged, and it is now correct for both. What it does NOT change is staleness, and the
shared registry adds a SECOND window on top of the old one: a session whose
CLIENT died ungracefully is listed for up to ~30s (the keepalive interval), and
a session whose SERVER INSTANCE died is listed for up to ~90s (the registry's
TTL — with per-process presence those entries vanished with the process). So
`delivered_sessions` remains a prediction, "N connected" can name a session on
an instance that no longer exists, and the outcome-unknown branch below stays
load-bearing. See LiveSession's doc comment for both bounds.

Nor is the count a match count: it filters on user, armed, and target id, while
actual delivery ALSO applies each stream's own item visibility. A session it
counts can still drop the push. Read it as what was ADDRESSED (BUG-2725).

`delivered_sessions` can also arrive NULL: the server published a broadcast but
could not read the presence registry to count it. Null means unknown, not zero
— the targeted case is refused with a 503 rather than published, so a null is
always a broadcast that went out.
-->
<script lang="ts">
	import { untrack } from 'svelte';
	import Modal from '$lib/components/common/Modal.svelte';
	import Button from '$lib/components/common/Button.svelte';
	import { api, PadApiError } from '$lib/api/client';
	import { toastStore } from '$lib/stores/toast.svelte';
	import { relativeTime } from '$lib/utils/markdown';
	import {
		PUSH_MESSAGE_MAX_LEN,
		collapsePushMessage,
		defaultPushMessage,
		isPushMessageEmpty,
		isPushMessageTooLong,
		pushMessageLength,
		trimPushMessage
	} from '$lib/push/message';
	// Shared with the quick-action dispatch (PLAN-2558 S4) — one whitelist, so
	// the two surfaces can't drift on which failures are safe to re-offer.
	import { isPrePublishRefusal } from '$lib/push/dispatch';
	import type { LiveSession } from '$lib/types';

	interface Props {
		open: boolean;
		/** Dismissal request from Escape / backdrop / Cancel / a completed send.
		 *  The parent flips `open` and restores focus to the ⋯ trigger. */
		onclose: () => void;
		wsSlug: string;
		/** The item's slug, used to address the endpoint. */
		itemSlug: string;
		/** Human ref for the heading and the prefill, e.g. "TASK-14". */
		itemRef: string;
		itemTitle: string;
	}

	let { open, onclose, wsSlug, itemSlug, itemRef, itemTitle }: Props = $props();

	const uid = $props.id();
	const titleId = `push-dialog-title-${uid}`;
	const textareaId = `push-dialog-message-${uid}`;
	const counterId = `push-dialog-counter-${uid}`;
	const noteId = `push-dialog-note-${uid}`;
	const targetId = `push-dialog-target-${uid}`;

	/**
	 * Presence re-read cadence while the dialog is open. A session can connect
	 * or drop mid-compose, and the whole value of this surface is that the
	 * count is true at the moment of the click rather than at mount.
	 *
	 * 10s is affordable: GET /api/v1/sessions falls through to the general API
	 * limiter (600 req/min per user, burst 60) and is in no strict per-path
	 * bucket, so a poll open for an hour costs 360 of a 36,000-request budget.
	 * It is also the right ORDER of magnitude for the underlying signal — a
	 * dropped CLIENT takes up to the ~30s keepalive interval to disappear, so
	 * polling much faster would buy precision the data doesn't have.
	 *
	 * Deliberately reasoned against the ~30s client bound and not the ~90s
	 * dead-INSTANCE one (see the header): re-polling does not shorten the
	 * second, since every instance reads the same shared entry and returns
	 * the same stale answer. A faster poll would buy nothing there either.
	 */
	const PRESENCE_POLL_MS = 10_000;

	/** How long to stay in 'checking' before admitting we cannot tell. Shorter
	 *  than one poll interval, so a stalled first read never leaves Push dead
	 *  and unexplained while the user waits for a beat that may not help. */
	const PRESENCE_STALL_MS = 5_000;

	/**
	 * How stale a 'known' answer may get before it degrades to 'can't tell'.
	 *
	 * 30s is not arbitrary: it is the server's own worst-case presence
	 * staleness (watchEventsKeepaliveInterval — an ungraceful disconnect is
	 * invisible until the next keepalive write fails). Past that, an
	 * un-refreshed count is no more informative than no count, so continuing to
	 * render it as fact would be the same overclaim in slow motion. Three poll
	 * intervals must fail in a row to reach it.
	 */
	const PRESENCE_MAX_AGE_MS = 30_000;

	/**
	 * What we know about who is listening.
	 *  - 'checking': first read of this opening is in flight, no answer yet.
	 *  - 'known':    a 200 answered; `count` is authoritative (modulo the
	 *                staleness every consumer of this data carries — see the
	 *                header for both bounds).
	 *  - 'unknown':  the server could not answer (503 / 401) or the request
	 *                failed. NOT zero — see this component's header.
	 */
	type PresenceState = 'checking' | 'known' | 'unknown';
	let presenceState = $state<PresenceState>('checking');
	let sessions = $state<LiveSession[]>([]);
	let presenceReason = $state('');

	let message = $state('');
	/**
	 * The chosen target, or '' for broadcast (PLAN-2558 S5, TASK-2588).
	 * Populated from `sessions` (the same presence read the count above
	 * uses — no separate fetch), so it degrades exactly the way the
	 * count does: an id that drops out of `sessions` on the next poll
	 * simply stops being an option, same as any other session leaving.
	 */
	let selectedSessionId = $state('');
	let sending = $state(false);
	let sendError = $state('');
	/**
	 * Set when a send failed in a way that leaves the OUTCOME UNKNOWN — the
	 * request was dispatched but the server never told us what happened to it.
	 * Push is not re-armed in that state: the endpoint has no idempotency key,
	 * so a second click can publish the same instruction twice. Same rule
	 * CopyItemDialog applies for the same reason (DR-13), and drawn at the same
	 * line — "was the mutation dispatched?", not "did the server say it failed?".
	 */
	let outcomeUnknown = $state(false);

	/**
	 * Fences for async writes. Plain `let`s, never $state: read and written only
	 * inside handlers, never in reactive position.
	 *
	 * TWO counters, because they fence different things:
	 *  - `presenceGen` identifies one OPENING of the dialog. A response in
	 *    flight when the user closes and reopens (or the parent remounts on an
	 *    item switch) must not write into the new opening.
	 *  - `presenceSeq` identifies one REQUEST. Polls are 10s apart but nothing
	 *    bounds how long one takes, so a stalled poll can resolve AFTER a later
	 *    one and overwrite a fresher count with an older one — re-enabling Push
	 *    against a session list that is already gone. Only a strictly newer
	 *    response is applied.
	 */
	let presenceGen = 0;
	let presenceSeq = 0;
	let presenceAppliedSeq = 0;
	/** Wall-clock of the last SUCCESSFUL presence read, for the staleness
	 *  expiry in the poll below. */
	let lastAnsweredAt = 0;

	/**
	 * True once this component instance has been torn down.
	 *
	 * `presenceGen` cannot cover this case, and assuming it could was the bug
	 * (codex round 2): the parent remounts this dialog under `{#key itemSlug}`,
	 * so item B gets a NEW instance with its OWN counters. A's in-flight send
	 * therefore still sees `gen === presenceGen` — A's own, untouched — and
	 * calls the SHARED parent `onclose`, closing the composer the user just
	 * opened for B. A per-instance liveness flag is the thing that actually
	 * distinguishes "still mine to close" from "I no longer exist".
	 */
	let destroyed = false;
	$effect(() => () => {
		destroyed = true;
	});

	/**
	 * PLAN-2613 S4 (D3): connected is not the same as ACCEPTING. Only an
	 * armed session actually receives a push (the server filters delivery to
	 * armed sessions), so `acceptingSessions` — not the full `sessions` list —
	 * is what the picker targets, what the count promises, and what enables
	 * Send. Showing the split ("N connected, M accepting") is the honest
	 * form: a connected-but-unarmed session is real and worth surfacing so
	 * the user can enable it (/pad:connect), not hidden so a push silently
	 * goes nowhere. `count` on the wire is redundant; derive from the array.
	 */
	const connectedCount = $derived(sessions.length);
	const acceptingSessions = $derived(sessions.filter((s) => s.armed));
	const acceptingCount = $derived(acceptingSessions.length);
	const collapsed = $derived(collapsePushMessage(message));
	const messageLength = $derived(pushMessageLength(message));
	const tooLong = $derived(isPushMessageTooLong(message));
	const empty = $derived(isPushMessageEmpty(message));

	/**
	 * True when the collapse will visibly change what the user typed. Surfaced
	 * because `Notification.Summary` is a single-line wire contract — a composer
	 * that silently reflows three paragraphs into one line misrepresents the
	 * channel, and the user should find that out here rather than by reading
	 * their own message in a terminal.
	 */
	// Compared against the GO-trimmed message, not `String.trim()`. The two
	// disagree on exactly the characters `$lib/push/message` exists to get right
	// (JS trims a leading U+FEFF that the server keeps; it leaves a U+0085 the
	// server strips), so using `trim()` here would warn about a collapse that
	// isn't going to happen — and stay silent about one that is.
	const willCollapse = $derived(collapsed !== '' && collapsed !== trimPushMessage(message));

	/** Nothing is ACCEPTING pushes, and we are sure of it. The only state that
	 *  blocks send on presence grounds — 'unknown' deliberately does not.
	 *  Gated on acceptingCount, not connectedCount (PLAN-2613 S4): a push to a
	 *  connected-but-unarmed session is silently dropped by the server, so
	 *  sending with zero accepting would be the fire-and-forget D3 forbids,
	 *  even when other sessions are connected. */
	const noAccepting = $derived(presenceState === 'known' && acceptingCount === 0);

	const canSend = $derived(
		!sending &&
			!outcomeUnknown &&
			!empty &&
			!tooLong &&
			!noAccepting &&
			presenceState !== 'checking'
	);

	/**
	 * Keep `selectedSessionId` valid whenever `sessions` changes. Called
	 * right after every `sessions = ...` reassignment that ISN'T already
	 * paired with an explicit reset of the selection (TASK-2588 round 2,
	 * codex).
	 *
	 * A `<select>` whose bound value names an `<option>` that no longer
	 * exists typically falls back to DISPLAYING the first remaining
	 * option (here, "All connected sessions") while the underlying bound
	 * value stays the stale id — so the user visually sees "broadcast"
	 * selected while the wire would still carry the dead target_session_id
	 * on send. Reconciling here (not via a $effect that reads
	 * `selectedSessionId`, which would read the same state it writes —
	 * CONVE-1688) closes that at every point `sessions` can change: a
	 * live poll dropping the selected session, and the staleness-expiry
	 * path below that clears `sessions` directly.
	 */
	function reconcileSelectedSession() {
		// The target must be an ARMED session (PLAN-2613 S4) — an unarmed one
		// can't receive the push. Reads `sessions` directly (freshly assigned
		// by the caller) rather than the acceptingSessions derived, so it
		// doesn't depend on derived recompute timing, and never in reactive
		// position (CONVE-1688). A selection that dropped OR lost its armed
		// bit falls back to broadcast.
		if (selectedSessionId && !sessions.some((s) => s.id === selectedSessionId && s.armed)) {
			selectedSessionId = '';
		}
	}

	async function refreshPresence(gen: number): Promise<void> {
		const seq = ++presenceSeq;
		/**
		 * Apply only if this opening is still current AND no newer response has
		 * already landed.
		 *
		 * Deliberately "latest ARRIVED", not "latest ISSUED" (codex round 2).
		 * If request 1 stalls and request 2 is issued, a late-but-first arrival
		 * from request 1 IS applied — because the alternative, dropping it
		 * because a newer request exists, leaves the UI holding older data when
		 * that newer request is the one that never settles. Any answer beats no
		 * answer; what must never happen is an OLDER answer overwriting a newer
		 * one, and that is what the `>` fence prevents. `presenceAppliedSeq`
		 * resets to 0 per opening, which is safe because `gen` already rejects
		 * responses from a previous one.
		 */
		const stillCurrent = () => gen === presenceGen && seq > presenceAppliedSeq;
		try {
			const resp = await api.sessions.list();
			if (!stillCurrent()) return;
			presenceAppliedSeq = seq;
			lastAnsweredAt = Date.now();
			sessions = resp.sessions ?? [];
			reconcileSelectedSession();
			presenceState = 'known';
			presenceReason = '';
		} catch (err) {
			if (!stillCurrent()) return;
			presenceAppliedSeq = seq;
			// Every failure lands here as 'unknown', never as zero. The message
			// distinguishes the one case a self-hosted user can act on (the
			// server has no presence registry) from a transient read failure.
			sessions = [];
			reconcileSelectedSession();
			presenceState = 'unknown';
			presenceReason =
				err instanceof PadApiError && err.code === 'unavailable'
					? 'This server has no session presence registry.'
					: 'Could not reach the server to check.';
		}
	}

	// Fresh-on-open reset + presence polling.
	//
	// CONVE-1688: the tracked scope reads `open` (a prop) and nothing else. Every
	// $state write — and every prop read that must NOT re-trigger this effect —
	// is inside `untrack`, so the effect cannot self-invalidate, and a title edit
	// arriving over SSE mid-compose cannot clobber what the user has typed.
	$effect(() => {
		if (!open) return;

		const gen = untrack(() => {
			presenceGen += 1;
			presenceAppliedSeq = 0;
			message = defaultPushMessage(itemRef, itemTitle);
			selectedSessionId = '';
			sending = false;
			sendError = '';
			outcomeUnknown = false;
			sessions = [];
			presenceState = 'checking';
			presenceReason = '';
			return presenceGen;
		});

		void refreshPresence(gen);
		const timer = setInterval(() => {
			// Expire a 'known' answer that has stopped being refreshed (codex
			// round 2). The stall timeout below only rescues the FIRST read; a
			// later poll that hangs would otherwise freeze the count at its last
			// value forever, leaving Push armed against a session list nobody has
			// confirmed in minutes. Past PRESENCE_MAX_AGE_MS our answer carries no
			// more authority than "can't tell", so we say that instead.
			if (presenceState === 'known' && Date.now() - lastAnsweredAt > PRESENCE_MAX_AGE_MS) {
				sessions = [];
				reconcileSelectedSession();
				presenceState = 'unknown';
				presenceReason = 'The last check was a while ago and hasn’t refreshed.';
				// Retire every request already in flight (codex round 3). Those
				// were issued BEFORE the expiry, so their answers describe the
				// server as it was back then — letting one land now would restore
				// the very count we just declared too old to trust. The poll
				// issued immediately below carries a newer seq and still applies.
				presenceAppliedSeq = presenceSeq;
			}
			void refreshPresence(gen);
		}, PRESENCE_POLL_MS);
		// Nothing bounds how long `/sessions` may take, and 'checking' disables
		// Push — so a request that never settles would strand the composer with
		// a dead button and no explanation. After this long, stop waiting and
		// say what is true: we cannot tell. A later response still lands (the
		// sequence fence lets it) and upgrades the answer.
		const stall = setTimeout(() => {
			if (gen !== presenceGen || presenceState !== 'checking') return;
			presenceState = 'unknown';
			presenceReason = 'The server hasn’t answered.';
		}, PRESENCE_STALL_MS);
		return () => {
			clearInterval(timer);
			clearTimeout(stall);
		};
	});

	function handleDismiss() {
		if (sending) return;
		// Invalidate any in-flight presence read so it cannot write after close.
		presenceGen += 1;
		onclose();
	}

	async function handleSend() {
		if (!canSend) return;
		// Fence the continuation two ways. `gen` catches a close-and-reopen
		// within THIS instance; `destroyed` catches the parent remounting the
		// dialog for a different item, where a new instance has its own `gen`
		// and only liveness distinguishes A's stale continuation from B's.
		const gen = presenceGen;
		const stillMine = () => !destroyed && gen === presenceGen;
		// Captured before the await, same as every other read of reactive
		// state in this function — `selectedSessionId` can't actually change
		// while `sending` disables the picker, but the pattern is load-bearing
		// elsewhere in this file and cheap to keep consistent here too.
		const target = selectedSessionId;
		sending = true;
		sendError = '';
		try {
			// NEVER retried automatically, at this call site or any other: the
			// endpoint carries no idempotency key. Broadcast keeps the exact
			// pre-S5 3-argument call — only a non-empty target adds the 4th.
			const result = target
				? await api.items.push(wsSlug, itemSlug, collapsed, target)
				: await api.items.push(wsSlug, itemSlug, collapsed);
			sending = false;
			// STRICT `=== undefined`, never `== undefined`, and both branches
			// below are guarded by `target` for the same reason: since
			// BUG-2698 the field can also be NULL, meaning "published, count
			// unknown". A loose comparison would catch that null and route a
			// successful broadcast into the mixed-version branch. It cannot
			// arrive here in practice — a null is only ever emitted for a
			// broadcast, and a targeted push with an unreadable registry is
			// refused with a 503 — but the guard is one character wide, so
			// pin it rather than rely on that.
			if (target && result.delivered_sessions === undefined) {
				// Mixed-version hazard (TASK-2588 round 2, codex). The server
				// ships EMBEDDED in the binary (web/build is baked into the Go
				// build), so a version skew between this tab's JS and the
				// server it's talking to can only exist transiently — a stale
				// tab surviving a server swap — never as a sustained topology.
				// That's still worth a cheap check, not a capability-
				// negotiation system: a response to a targeted send with no
				// delivered_sessions AT ALL means the server that answered
				// doesn't know about targeting (pre-S5, or a proxy that
				// stripped the field) — server-side, that server unconditionally
				// PUBLISHES every push it accepts (the pre-S5 contract), so this
				// was NOT skipped the way a same-version miss is. Tell the user
				// honestly rather than either silently treating it as delivered
				// or, worse, running the miss-flow against a `0` that was never
				// actually reported — this branch must run BEFORE the `=== 0`
				// check below, and must never fall through to it.
				toastStore.show('server didn’t confirm targeting — sent as broadcast', 'info');
				if (!stillMine()) return;
				handleDismiss();
				return;
			}
			if (target && result.delivered_sessions === 0) {
				// A targeted push that reached nobody. As of TASK-2588 round 1
				// the server SKIPS the publish entirely for this case (see
				// pushResponse.DeliveredSessions' doc comment) — nothing was
				// sent, so zero delivery is a guarantee, not a race, and
				// nothing would be duplicated by resending. Unlike every other
				// outcome here, it is therefore safe to leave Push re-armed
				// rather than closing: drop the stale selection back to
				// broadcast and re-read presence so the picker reflects what
				// is actually still connected, then let the user resend to a
				// live target.
				toastStore.show('that session is gone — refresh the list', 'error');
				if (!stillMine()) return;
				selectedSessionId = '';
				void refreshPresence(gen);
				return;
			}
			// The toast fires even if this instance is gone — the push really
			// happened, and suppressing the confirmation would be the dishonest
			// half. Honest past tense: the notification was PUBLISHED. Whether an
			// agent read it is not something the server can tell us.
			toastStore.show(`Pushed to ${itemRef} — delivery isn’t confirmed`, 'success');
			if (!stillMine()) return;
			handleDismiss();
		} catch (err) {
			sending = false;
			if (!stillMine()) return;
			// See PUSH_PRE_PUBLISH_ERROR_CODES: a recognised pre-publish refusal
			// is safe to correct and resend. Everything else — a rejected fetch,
			// a non-JSON 502, a gateway envelope we don't recognise — means the
			// request went out and we never learned its fate. The handler
			// publishes BEFORE it writes the response, so the message may well
			// have been delivered; re-arming Push would offer a duplicate.
			if (isPrePublishRefusal(err)) {
				sendError = err instanceof Error ? err.message : 'Failed to push the message.';
			} else {
				outcomeUnknown = true;
				sendError =
					'The server didn’t give a clear answer, so we can’t tell whether this was sent. ' +
					'Check your agent session before sending it again — pushing twice would deliver it twice.';
			}
		}
	}

	/** Clipboard fallback for the no-listeners state — the same escape hatch
	 *  PLAN-2558 S4 rules for quick actions, so the surface is never a dead end. */
	async function handleCopyInstead() {
		const gen = presenceGen;
		try {
			await navigator.clipboard.writeText(collapsed);
			toastStore.show('Copied to clipboard', 'success');
			// Same two-part fence as handleSend — a late dismiss from a
			// destroyed instance would close the NEXT item's composer.
			if (destroyed || gen !== presenceGen) return;
			handleDismiss();
		} catch {
			toastStore.show('Failed to copy to clipboard', 'error');
		}
	}

	function sessionName(session: LiveSession): string {
		// An unlabelled session is shown by a short id rather than hidden: the
		// list's job is to account for every listener, and "unlabelled" is not
		// the same as "not there".
		const base = session.label || `session ${session.id.slice(0, 8)}`;
		return session.pid ? `${base} (pid ${session.pid})` : base;
	}
</script>

<Modal
	{open}
	onclose={handleDismiss}
	labelledby={titleId}
	maxWidth="560px"
	closeOnBackdrop={!sending}
	class="push-dialog"
>
	<div class="modal-header">
		<h2 id={titleId}>Push {itemRef} to an agent</h2>
		<button
			class="close-btn"
			type="button"
			aria-label="Close"
			disabled={sending}
			onclick={handleDismiss}>&#10005;</button
		>
	</div>

	<div class="modal-body">
		<!-- ── Who is listening ──────────────────────────────────────────────
		     role="status" so a count that changes under the user (a session
		     drops mid-compose) is announced rather than silently flipping the
		     Send button's disabled state. -->
		<section class="presence" role="status">
			{#if presenceState === 'checking'}
				<p class="muted">Checking for connected agent sessions…</p>
			{:else if presenceState === 'unknown'}
				<p class="notice notice-warn">
					<strong>Can’t tell whether any agent session is connected.</strong>
					{presenceReason} You can still send — but nothing here knows whether it will
					land anywhere.
				</p>
			{:else if acceptingCount === 0}
				<!-- Nothing accepting. Two distinct empty states (PLAN-2613 S4):
				     nobody connected at all, vs. sessions connected but none opted
				     in — the second is the honest-counts case D3 exists for. -->
				{#if connectedCount === 0}
					<p class="notice notice-warn">
						<strong>No agent session is connected.</strong>
						A push isn’t stored anywhere — with nothing listening it would be lost, not
						queued. Start a session (<code>pad watch --stream</code>) and connect it
						(<code>/pad:connect</code>) so it accepts pushes, or copy the message and
						paste it yourself.
					</p>
				{:else}
					<p class="notice notice-warn">
						<strong
							>{connectedCount}
							{connectedCount === 1 ? 'session' : 'sessions'} connected, 0 accepting pushes.</strong
						>
						A push only reaches a session that has opted in. Run
						<code>/pad:connect</code> (or <code>pad session arm</code>) in a connected
						session to enable it, or copy the message and paste it yourself.
					</p>
				{/if}
			{:else}
				<p class="presence-ok">
					<strong
						>{acceptingCount}
						{acceptingCount === 1 ? 'session' : 'sessions'} accepting pushes</strong
					>{#if connectedCount > acceptingCount}
						<span class="muted"> (of {connectedCount} connected)</span>{/if}
					<!-- Two separate hedges, both load-bearing. The registry can name
					     a session that is already gone — up to ~30s for a dropped
					     CLIENT, and on a Redis-backed deployment up to ~90s for a
					     dead SERVER instance, whose entries clear on the shared
					     registry's TTL (BUG-2698, codex round 23). So even "was
					     listening" is past tense. And nothing acknowledges a push,
					     so delivery is never confirmable.

					     The copy says "recently" rather than naming either number:
					     which one applies depends on a deployment shape the user
					     cannot see, and a specific figure that is wrong in the other
					     shape is worse than an honest vague one. The precise bounds
					     live in LiveSession's doc comment for the people who can act
					     on them. -->
					— as of the last check. Pad can’t confirm delivery, and a session
					that stopped listening recently can still be listed here.
				</p>
				<ul class="session-list">
					{#each acceptingSessions as session (session.id)}
						<li>
							<span class="session-name">{sessionName(session)}</span>
							<span class="muted">connected {relativeTime(session.connected_at)}</span>
						</li>
					{/each}
				</ul>
			{/if}
		</section>

		<!-- ── Target ────────────────────────────────────────────────────────
		     Only rendered once there is something to pick between (PLAN-2558
		     S5, TASK-2588) — with zero sessions Send is already disabled by
		     `noListeners`, so a picker here would offer a choice that can't be
		     acted on. Broadcast is the default on every fresh open (Fresh-on-
		     open reset above), never a remembered previous target. -->
		{#if acceptingCount > 0}
			<section class="section">
				<label class="field-label" for={targetId}>Send to</label>
				<!-- Only ARMED sessions are targetable (PLAN-2613 S4): a push to
				     an unarmed session is dropped server-side, so offering one as
				     a target would be offering a guaranteed miss. Broadcast, too,
				     reaches only accepting sessions (the server filters), so its
				     count is the accepting count, not the connected one. -->
				<select id={targetId} class="target-picker" bind:value={selectedSessionId} disabled={sending}>
					<option value=""
						>All accepting sessions ({acceptingCount})</option
					>
					{#each acceptingSessions as session (session.id)}
						<option value={session.id}>{sessionName(session)}</option>
					{/each}
				</select>
			</section>
		{/if}

		<!-- ── Message ───────────────────────────────────────────────────── -->
		<section class="section">
			<label class="field-label" for={textareaId}>Message</label>
			<textarea
				id={textareaId}
				class="composer"
				rows="4"
				bind:value={message}
				disabled={sending}
				aria-invalid={tooLong ? 'true' : undefined}
				aria-describedby="{counterId} {noteId}"
				placeholder="What should the agent do with this item?"
			></textarea>

			<div class="composer-meta">
				<span id={counterId} class="counter" class:over={tooLong}>
					{messageLength} / {PUSH_MESSAGE_MAX_LEN}
				</span>
				<!--
					One stable node for BOTH the over-length error and the
					collapse note, always present and always referenced by the
					textarea's aria-describedby.

					Not two conditionally-rendered spans: an `aria-describedby`
					pointing at an id that isn't in the document resolves to
					nothing, so a note that only exists when relevant is
					announced by nobody.

					And a FIXED `role="status"`, not one that escalates to
					`alert` when the message is too long — swapping the role on
					a live region while also changing its text is not reliably
					honoured across screen readers, so the escalation would be a
					promise the markup can't keep. The blocking condition is
					carried where it is unambiguous instead: `aria-invalid` on
					the textarea plus a disabled Push.
				-->
				<span id={noteId} class:notice-inline={tooLong} class:muted={!tooLong} role="status">
					{#if tooLong}
						Too long — trim {messageLength - PUSH_MESSAGE_MAX_LEN} character{messageLength -
							PUSH_MESSAGE_MAX_LEN ===
						1
							? ''
							: 's'} before sending.
					{:else if willCollapse}
						Line breaks and repeated spaces are collapsed — this arrives as one line.
					{/if}
				</span>
			</div>
		</section>

		{#if sendError}
			<p class="notice notice-error" role="alert">{sendError}</p>
		{/if}
	</div>

	<div class="modal-footer">
		{#if sending}
			<span class="muted footer-status" role="status">Sending…</span>
		{/if}
		<Button variant="secondary" disabled={sending} onclick={handleDismiss}>Cancel</Button>
		{#if noAccepting}
			<Button variant="secondary" disabled={empty || tooLong} onclick={handleCopyInstead}>
				Copy instead
			</Button>
		{/if}
		<Button variant="primary" disabled={!canSend} onclick={handleSend}>
			{sending ? 'Sending…' : 'Push'}
		</Button>
	</div>
</Modal>

<style>
	.modal-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
		padding: var(--space-4);
		border-bottom: 1px solid var(--border);
		flex-shrink: 0;
	}
	.modal-header h2 {
		margin: 0;
		font-size: 1rem;
		font-weight: 600;
	}
	.close-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		cursor: pointer;
		font-size: 1rem;
		line-height: 1;
		padding: var(--space-1);
	}
	.close-btn:disabled {
		opacity: 0.4;
		cursor: not-allowed;
	}

	.modal-body {
		padding: var(--space-4);
		overflow-y: auto;
		flex: 1 1 auto;
		min-height: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}

	.modal-footer {
		display: flex;
		align-items: center;
		justify-content: flex-end;
		gap: var(--space-2);
		padding: var(--space-3) var(--space-4);
		border-top: 1px solid var(--border);
		flex-shrink: 0;
	}
	.footer-status {
		margin-right: auto;
	}

	.section,
	.presence {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.field-label {
		font-size: 0.8125rem;
		font-weight: 600;
		color: var(--text-muted);
	}

	/* Border / background / padding come from app.css's global
	   `input, textarea, select` rule — only the box behaviour is local. */
	/* Border / background / padding come from app.css's global
	   `input, textarea, select` rule, same as .composer above. */
	.target-picker {
		width: 100%;
		box-sizing: border-box;
	}
	.composer {
		width: 100%;
		box-sizing: border-box;
		resize: vertical;
	}
	.composer:disabled {
		opacity: 0.6;
	}

	.composer-meta {
		display: flex;
		align-items: baseline;
		justify-content: space-between;
		gap: var(--space-3);
		font-size: 0.8125rem;
		flex-wrap: wrap;
	}
	.counter {
		color: var(--text-muted);
		font-variant-numeric: tabular-nums;
		flex-shrink: 0;
	}
	.counter.over {
		color: var(--accent-red);
		font-weight: 600;
	}
	.notice-inline {
		color: var(--accent-red);
	}

	.presence-ok {
		margin: 0;
		font-size: 0.875rem;
	}

	.session-list {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
		font-size: 0.8125rem;
	}
	.session-list li {
		display: flex;
		gap: var(--space-2);
		align-items: baseline;
		flex-wrap: wrap;
	}
	.session-name {
		font-weight: 600;
	}

	/* Same notice treatment as CopyItemDialog, so the two dialogs in this
	   pane's ⋯ menu read as one surface. */
	.notice {
		margin: 0;
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius);
		font-size: 0.85rem;
		line-height: 1.5;
		border: 1px solid var(--border);
		background: var(--bg-secondary);
	}
	.notice-warn {
		border-left: 3px solid var(--accent-orange);
	}
	.notice-error {
		border-left: 3px solid var(--accent-red);
	}
	.notice code {
		font-size: 0.8125em;
	}

	.muted {
		color: var(--text-muted);
		font-size: 0.8125rem;
		margin: 0;
	}
</style>
