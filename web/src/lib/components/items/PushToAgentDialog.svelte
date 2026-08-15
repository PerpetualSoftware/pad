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
		pushMessageLength
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
	 * Fence for async presence writes. Plain `let`, never $state: it is read and
	 * written only inside the loader and the open effect, never in reactive
	 * position. Incremented on every open so a response in flight when the user
	 * closes and reopens (or the parent remounts on an item switch) cannot write
	 * a stale count into the new opening — the no-`{#key}` late-continuation
	 * class this codebase keeps hitting.
	 */
	let presenceGen = 0;

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
	const willCollapse = $derived(message.trim() !== '' && collapsed !== message.trim());

	/** Nothing is listening, and we are sure of it. The only state that blocks
	 *  send on presence grounds — 'unknown' deliberately does not. */
	const noListeners = $derived(presenceState === 'known' && sessionCount === 0);

	const canSend = $derived(
		!sending && !empty && !tooLong && !noListeners && presenceState !== 'checking'
	);

	async function refreshPresence(gen: number): Promise<void> {
		try {
			const resp = await api.sessions.list();
			if (gen !== presenceGen) return;
			sessions = resp.sessions ?? [];
			presenceState = 'known';
			presenceReason = '';
		} catch (err) {
			if (gen !== presenceGen) return;
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
			message = defaultPushMessage(itemRef, itemTitle);
			sending = false;
			sendError = '';
			sessions = [];
			presenceState = 'checking';
			presenceReason = '';
			return presenceGen;
		});

		void refreshPresence(gen);
		const timer = setInterval(() => void refreshPresence(gen), PRESENCE_POLL_MS);
		return () => clearInterval(timer);
	});

	function handleDismiss() {
		if (sending) return;
		// Invalidate any in-flight presence read so it cannot write after close.
		presenceGen += 1;
		onclose();
	}

	async function handleSend() {
		if (!canSend) return;
		sending = true;
		sendError = '';
		try {
			// NEVER retried, at this call site or any other: the endpoint carries
			// no idempotency key, so a retry on an ambiguous failure can deliver
			// the same instruction twice.
			await api.items.push(wsSlug, itemSlug, collapsed);
			// Honest past tense: the notification was published. Whether an agent
			// read it is not something the server can tell us, so the toast does
			// not claim it.
			toastStore.show(`Pushed to ${itemRef} — delivery isn’t confirmed`, 'success');
			sending = false;
			handleDismiss();
		} catch (err) {
			sending = false;
			sendError =
				err instanceof PadApiError || err instanceof Error
					? err.message
					: 'Failed to push the message.';
		}
	}

	/** Clipboard fallback for the no-listeners state — the same escape hatch
	 *  PLAN-2558 S4 rules for quick actions, so the surface is never a dead end. */
	async function handleCopyInstead() {
		try {
			await navigator.clipboard.writeText(collapsed);
			toastStore.show('Copied to clipboard', 'success');
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
					— Pad can’t confirm delivery, only that something is listening.
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
				aria-describedby={counterId}
				placeholder="What should the agent do with this item?"
			></textarea>

			<div class="composer-meta">
				<span id={counterId} class="counter" class:over={tooLong}>
					{messageLength} / {PUSH_MESSAGE_MAX_LEN}
				</span>
				{#if tooLong}
					<span class="notice-inline" role="alert">
						Too long — trim {messageLength - PUSH_MESSAGE_MAX_LEN} character{messageLength -
							PUSH_MESSAGE_MAX_LEN ===
						1
							? ''
							: 's'} before sending.
					</span>
				{:else if willCollapse}
					<span class="muted">
						Line breaks and repeated spaces are collapsed — this arrives as one line.
					</span>
				{/if}
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
