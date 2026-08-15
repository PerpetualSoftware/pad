<!--
@component
PushToAgentDialog — compose an instruction about an item and push it to the
user's own connected agent sessions (PLAN-2558 S3, IDEA-2544 Phase 3).

Built on the shared `Modal` primitive, same as CopyItemDialog: the composer
needs more room than the pane menu's drill-down affords, and the native
`<dialog>` supplies focus trap + Escape + top-layer. Consumer obligations from
Modal's docs are honored — it is NOT wrapped in `{#if open}`, and `labelledby`
points at the heading.

THE POINT OF THIS DIALOG IS THE PRESENCE LINE, NOT THE TEXTAREA. `pad push` is
fire-and-forget: no durable inbox, no ack, no "nobody was listening" warning
(handlers_push.go — Dave's product call, and defensible for a CLI verb typed by
someone who knows their own session is running). A button in a web UI has no
such user. So this dialog answers "is anything listening?" BEFORE the click, and
words the answer at exactly the confidence the server can support:

  - N > 0        → "N session connected". NOT "this will be delivered". The
                   registry can name a session that died up to ~30s ago (an
                   ungraceful disconnect is invisible until the next keepalive
                   write fails), and even a live session gets no delivery
                   receipt. Send is enabled; the caveat is stated, not implied.
  - N == 0       → send is DISABLED. With nothing listening, a push is not
                   "probably lost" — it is definitively lost, because there is
                   no inbox to land in. Offering the button anyway would make
                   this surface worse than the clipboard ferry it replaces, so
                   the empty state offers the clipboard instead (the same
                   fallback PLAN-2558 S4 rules for quick actions).
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

DEPLOYMENT CONSTRAINT, inherited not created. Both the presence registry and
the event bus are per-PROCESS (`internal/server/session_presence.go`'s
SINGLE-PROCESS LIMITATION note). Behind more than one padd process a load
balancer can route the presence GET and the push POST to different instances,
and then every claim on this surface — including "No agent session is
connected" — can be wrong. That file already states the rule ("Do not put the
web-UI push surface in front of a multi-process deployment until both exist");
this dialog is the surface it means. The copy below is written for the
single-process case ON PURPOSE: hedging every sentence for a deployment the
server tells you not to run would cost honesty in the case that actually ships.
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

	/**
	 * Presence re-read cadence while the dialog is open. A session can connect
	 * or drop mid-compose, and the whole value of this surface is that the
	 * count is true at the moment of the click rather than at mount.
	 *
	 * 10s is affordable: GET /api/v1/sessions falls through to the general API
	 * limiter (600 req/min per user, burst 60) and is in no strict per-path
	 * bucket, so a poll open for an hour costs 360 of a 36,000-request budget.
	 * It is also the right ORDER of magnitude for the underlying signal — the
	 * registry's own worst-case staleness is ~30s (keepalive interval), so
	 * polling much faster would buy precision the data doesn't have.
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
	 *  - 'known':    a 200 answered; `count` is authoritative (modulo the ~30s
	 *                staleness every consumer of this data carries).
	 *  - 'unknown':  the server could not answer (503 / 401) or the request
	 *                failed. NOT zero — see this component's header.
	 */
	type PresenceState = 'checking' | 'known' | 'unknown';
	let presenceState = $state<PresenceState>('checking');
	let sessions = $state<LiveSession[]>([]);
	let presenceReason = $state('');

	let message = $state('');
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

	const sessionCount = $derived(sessions.length);
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

	/** Nothing is listening, and we are sure of it. The only state that blocks
	 *  send on presence grounds — 'unknown' deliberately does not. */
	const noListeners = $derived(presenceState === 'known' && sessionCount === 0);

	const canSend = $derived(
		!sending &&
			!outcomeUnknown &&
			!empty &&
			!tooLong &&
			!noListeners &&
			presenceState !== 'checking'
	);

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
			presenceState = 'known';
			presenceReason = '';
		} catch (err) {
			if (!stillCurrent()) return;
			presenceAppliedSeq = seq;
			// Every failure lands here as 'unknown', never as zero. The message
			// distinguishes the one case a self-hosted user can act on (the
			// server has no presence registry) from a transient read failure.
			sessions = [];
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
				presenceState = 'unknown';
				presenceReason = 'The last check was a while ago and hasn’t refreshed.';
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

	/**
	 * Error codes handlePushToItem (and the middleware in front of it) can
	 * return WITHOUT having published — the request provably never reached the
	 * bus, so a corrected resend cannot duplicate anything.
	 *
	 * A whitelist, not `err instanceof PadApiError`, and the difference is
	 * load-bearing (codex round 2): the API client turns ANY JSON error
	 * envelope into a PadApiError, including one a proxy or gateway invented
	 * AFTER the handler had already published. Treating "structured" as
	 * "definitely not published" would re-arm Push on exactly that case. So the
	 * rule is inverted — enumerate what we know is safe, and treat every
	 * unrecognised failure as ambiguous. The cost of the wrong answer is
	 * asymmetric: an unnecessary "we can't tell" makes the user check, while a
	 * wrong re-arm delivers the instruction twice.
	 */
	const PRE_PUBLISH_ERROR_CODES = new Set([
		'bad_request', // empty / whitespace-only / over-length / undecodable body
		'unauthorized', // no resolved user
		'not_found', // item or workspace doesn't resolve
		'forbidden',
		'permission_denied', // workspace-access middleware
		'unavailable', // the bus isn't wired — nothing to publish TO
		'rate_limited', // the client's own 429 shape; the handler never ran
		'plan_limit_exceeded'
	]);

	async function handleSend() {
		if (!canSend) return;
		// Fence the continuation two ways. `gen` catches a close-and-reopen
		// within THIS instance; `destroyed` catches the parent remounting the
		// dialog for a different item, where a new instance has its own `gen`
		// and only liveness distinguishes A's stale continuation from B's.
		const gen = presenceGen;
		const stillMine = () => !destroyed && gen === presenceGen;
		sending = true;
		sendError = '';
		try {
			// NEVER retried automatically, at this call site or any other: the
			// endpoint carries no idempotency key.
			await api.items.push(wsSlug, itemSlug, collapsed);
			sending = false;
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
			// See PRE_PUBLISH_ERROR_CODES: a recognised pre-publish refusal is
			// safe to correct and resend. Everything else — a rejected fetch, a
			// non-JSON 502, a gateway envelope we don't recognise — means the
			// request went out and we never learned its fate. The handler
			// publishes BEFORE it writes the response, so the message may well
			// have been delivered; re-arming Push would offer a duplicate.
			const code = err instanceof PadApiError ? err.code : '';
			if (code && PRE_PUBLISH_ERROR_CODES.has(code)) {
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
			{:else if sessionCount === 0}
				<p class="notice notice-warn">
					<strong>No agent session is connected.</strong>
					A push isn’t stored anywhere — with nothing listening it would be lost, not
					queued. Start a session (<code>pad watch --stream</code>) and it will appear
					here, or copy the message and paste it yourself.
				</p>
			{:else}
				<p class="presence-ok">
					<strong
						>{sessionCount}
						{sessionCount === 1 ? 'session' : 'sessions'} connected</strong
					>
					<!-- Two separate hedges, both load-bearing. The registry can name a
					     session that dropped ungracefully up to ~30s ago, so even
					     "was listening" is past tense; and nothing acknowledges a
					     push, so delivery is never confirmable. -->
					— as of the last check. Pad can’t confirm delivery, and a session
					that dropped in the last ~30 seconds can still be listed here.
				</p>
				<ul class="session-list">
					{#each sessions as session (session.id)}
						<li>
							<span class="session-name">{sessionName(session)}</span>
							<span class="muted">connected {relativeTime(session.connected_at)}</span>
						</li>
					{/each}
				</ul>
			{/if}
		</section>

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
		{#if noListeners}
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
