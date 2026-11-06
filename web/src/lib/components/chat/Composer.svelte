<script lang="ts">
	import { onMount, untrack } from 'svelte';
	import {
		MAX_BODY_BYTES,
		NOTICE,
		PLACEHOLDER_OP_KEY,
		utf8Bytes,
		validateDraft
	} from '$lib/chat/compose';
	import { getDraft, setDraft, clearDraft } from '$lib/chat/draftKeeper';
	import { inputMode } from '$lib/chat/inputMode.svelte';
	import type { AgentPresence } from '$lib/chat/presence';
	import { watchKeyboardInset } from '$lib/chat/viewport';
	import { authStore } from '$lib/stores/auth.svelte';
	import { registerSessionExpiryKeeper } from '$lib/stores/sessionExpiryKeep';
	import { signOutDiscarding } from '$lib/stores/signOutGuard.svelte';
	import type { SendOutcome } from '$lib/stores/chatThread.svelte';
	import type { ChatMessage } from '$lib/types';

	// The message composer (TASK-4044 PR 3b). It knows nothing of the store: the
	// page hands it `onSend`, whose outcome says whether the text is gone (a
	// bubble now carries it) or comes back (a refusal). Counts UTF-8 bytes and
	// uses Go's TrimSpace for "empty", as the server does (N1, N2).
	let {
		n,
		agentKey = '',
		title = '',
		presence = 'unknown',
		replyTo = null,
		messages = [],
		onClearReply,
		onSend
	}: {
		n: number;
		/** Where the message goes, named above the box (DOC-4316 section 5). */
		agentKey?: string;
		title?: string;
		/** The latest presence read: it shapes the words around a send, never whether one is allowed. */
		presence?: AgentPresence;
		replyTo?: ChatMessage | null;
		/** Authorized rows used to refresh a restored reply preview. */
		messages?: ChatMessage[];
		onClearReply: () => void;
		onSend: (body: string, replyToSeq?: number) => Promise<SendOutcome>;
	} = $props();

	let text = $state('');
	let restoredReplySeq = $state<number | null>(null);
	let replySeq = $derived(replyTo?.seq ?? restoredReplySeq ?? undefined);
	let replyPreview = $derived(replyTo ?? messages.find((m) => m.seq === restoredReplySeq && !m.deleted));
	let draftNotice = $state('');
	// The view keys this composer by conversation; capture its owner before any teardown.
	let owner = '';
	let conversation = 0;
	let expiryKept = false;
	let destroyed = false;
	function clearReply() {
		restoredReplySeq = null;
		onClearReply();
	}
	function saveDraft(body: string, reply: number | undefined) {
		if (expiryKept || destroyed) return;
		if (body === '') {
			clearDraft(owner, conversation);
			draftNotice = '';
		} else {
			const persisted = setDraft(owner, conversation, body, reply);
			draftNotice = persisted ? 'Draft kept in this tab' : 'Draft kept until this page reloads';
		}
	}
	let notice = $state('');
	// "Saved to conversation" after a stored send (section 7): a successful POST proves storage and nothing more.
	let saved = $state('');
	let inset = $state(0);
	// An IME commit can reach keydown after compositionend (Safari), so the flag outlives it by one task.
	let composing = false;

	let box = $state<HTMLTextAreaElement | null>(null);
	/** Reply puts the reader straight into the box (r5 H3). */
	export function focus() {
		box?.focus();
	}

	/** A refused send's text comes back, ahead of anything typed since, with its notice. The page uses it for a refused Retry too (r7 M1). */
	export function restore(body: string, outcome: Exclude<SendOutcome, 'sent' | 'failed' | 'stale'> | 'send_error') {
		text = text ? `${body}\n${text}` : body;
		if (outcome !== 'empty') notice = NOTICE[outcome];
	}

	let bytes = $derived(utf8Bytes(text));
	let block = $derived(validateDraft(text, replySeq, PLACEHOLDER_OP_KEY));
	let showCount = $derived(bytes > MAX_BODY_BYTES * 0.75);
	let live = $derived(!!notice || !!saved || showCount || block === 'too_large');

	onMount(() => {
		owner = authStore.userId;
		conversation = n;
		const draft = getDraft(owner, conversation);
		if (draft && draft.n === n) {
			text = draft.text;
			restoredReplySeq = draft.replySeq;
		}
		const stopViewport = watchKeyboardInset((px) => {
			inset = px;
			if (px > 0) inputMode.enterByTouch();
		});
		return () => {
			saveDraft(text, replySeq);
			destroyed = true;
			stopViewport();
		};
	});

	$effect(() => {
		const body = text;
		const reply = replySeq;
		// Empty drafts disappear immediately; edits coalesce, and destroy flushes them.
		if (body === '') untrack(() => saveDraft(body, reply));
		else {
			const timer = setTimeout(() => untrack(() => saveDraft(body, reply)), 250);
			return () => clearTimeout(timer);
		}
	});

	// Session expiry retains identity. Keep what was typed since; an unanswered body is already
	// a pending-attempt record, and stashing it here too would offer it twice after re-login.
	// The snapshot is final: the subsequent destroy must not overwrite it.
	$effect(() =>
		registerSessionExpiryKeeper(() => {
			if (signOutDiscarding()) return;
			if (text !== '') setDraft(owner, conversation, text, replySeq);
			expiryKept = true;
		})
	);

	// Enter sends only in hardware mode, never while an IME is composing, and never on a
	// key repeat or with a modifier. Everything else is left to the textarea.
	function onKeydown(e: KeyboardEvent) {
		if (e.key !== 'Enter' || e.shiftKey || e.ctrlKey || e.altKey || e.metaKey) return;
		if (e.isComposing || e.keyCode === 229 || composing) return;
		if (!inputMode.hardware) return;
		e.preventDefault();
		if (e.repeat) return;
		void send();
	}

	function onCompositionEnd() {
		setTimeout(() => (composing = false), 0);
	}

	// Presence only words the send; it never blocks it (the API stores the message).
	let agentLive = $derived(presence === 'online' || presence === 'busy_uncertain');
	let presenceNote = $derived(
		presence === 'unknown'
			? 'Session status unavailable.'
			: agentLive
				? ''
				: 'No live armed session. Saving a message does not confirm receipt.'
	);

	async function send() {
		if (block) return;
		const body = text;
		const targetSeq = replySeq;
		// The words follow the state the person saw when they sent, not one that changed mid-flight.
		const wasLive = agentLive;
		text = '';
		clearDraft(owner, conversation);
		notice = '';
		saved = '';
		let outcome: SendOutcome | 'send_error';
		try {
			outcome = await onSend(body, targetSeq);
		} catch {
			// The box is already empty: a send that threw must not take the text with it (BUG-4312).
			outcome = 'send_error';
		}
		if (destroyed || outcome === 'stale') return;
		if (outcome === 'sent' || outcome === 'failed' || outcome === 'reply_deleted' || outcome === 'reply_missing') {
			if (replySeq === targetSeq) clearReply();
		}
		if (outcome === 'sent') saved = wasLive ? 'Saved to conversation' : 'Saved to conversation · receipt unconfirmed';
		if (outcome === 'sent' || outcome === 'failed') return;
		// A definite refusal: the text comes back, ahead of anything typed since.
		restore(body, outcome);
	}
</script>

<form
	class="composer"
	style:margin-bottom={inset > 0 ? `${inset}px` : undefined}
	onsubmit={(e) => {
		e.preventDefault();
		void send();
	}}
>
	{#if agentKey}
		<p class="target">
			<strong>To {agentKey}</strong> · C{n}{#if title}<span class="target-title"> · {title}</span>{/if}
		</p>
	{/if}
	{#if replySeq !== undefined}
		<div class="reply">
			<span class="reply-text">
				{#if replyPreview}
					Replying to {replyPreview.mine ? 'yourself' : replyPreview.author_agent_key}: {replyPreview.body}
				{:else}
					Replying to message #{replySeq}
				{/if}
			</span>
			<button type="button" aria-label="Cancel reply" onclick={clearReply}>×</button>
		</div>
	{/if}
	<div class="row">
		<textarea
			bind:this={box}
			bind:value={text}
			rows="2"
			placeholder="Message"
			aria-label="Message"
			enterkeyhint={inputMode.hardware ? 'send' : 'enter'}
			oninput={() => {
				notice = '';
				saved = '';
			}}
			onkeydown={onKeydown}
			oncompositionstart={() => (composing = true)}
			oncompositionend={onCompositionEnd}
			onpointerdown={(e) => {
				if (e.pointerType === 'touch') inputMode.enterByTouch();
			}}
		></textarea>
		<button
			type="submit"
			class="send"
			disabled={block !== null}
			aria-label={agentKey ? `Send to ${agentKey}, conversation ${n}` : undefined}>Send</button
		>
	</div>
	{#if text && draftNotice}<p class="draft-note">{draftNotice}</p>{/if}
	<p class="presence-note">{presenceNote}</p>
	<p class="key-hint">
		{inputMode.hardware ? 'Enter sends · Shift+Enter adds a line' : 'Enter adds a line · use Send to send'}
	</p>
	{#if inputMode.offered}
		<label class="hardware">
			<input
				type="checkbox"
				checked={inputMode.hardware}
				onchange={(e) => inputMode.setHardware(e.currentTarget.checked)}
			/>
			Hardware keyboard: Enter sends
		</label>
	{/if}
	<!-- Mounted from the start, only its text changes: a region inserted with its text is not announced by some screen readers (r5 H5). -->
	<p class="status" class:idle={!live} class:over={block === 'too_long' || block === 'too_large'} role="status">
		{#if block === 'too_long'}
			{NOTICE.too_long}
		{:else if block === 'too_large'}
			{NOTICE.too_large}
		{:else if notice}
			{notice}
		{:else if saved}
			{saved}
		{/if}
		{#if showCount}<span class="count">{bytes.toLocaleString()} / {MAX_BODY_BYTES.toLocaleString()} bytes</span>{/if}
	</p>
</form>

<style>
	.composer {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
		padding-top: var(--space-2);
		border-top: 1px solid var(--border);
		background: var(--bg-primary);
	}
	.row {
		display: flex;
		gap: var(--space-2);
		align-items: flex-end;
	}
	textarea {
		flex: 1;
		resize: none;
		font: inherit;
		padding: var(--space-2);
		border: 1px solid var(--border);
		border-radius: var(--radius-md, 6px);
		background: var(--bg-secondary);
		color: var(--text-primary);
	}
	.target {
		margin: 0;
		font-size: 0.85rem;
		overflow-wrap: anywhere;
	}
	.target-title {
		color: var(--text-secondary);
	}
	.draft-note,
	.presence-note,
	.key-hint,
	.hardware {
		margin: 0;
		font-size: 0.75rem;
		color: var(--text-secondary);
	}
	.presence-note:empty {
		display: none;
	}
	.reply {
		display: flex;
		gap: var(--space-2);
		align-items: center;
		padding: var(--space-1) var(--space-2);
		border-left: 3px solid var(--border);
		font-size: 0.8rem;
		color: var(--text-secondary);
	}
	.reply-text {
		flex: 1;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.status {
		margin: 0;
		font-size: 0.75rem;
		color: var(--text-secondary);
	}
	/* Empty, but kept in the page: cancel the form's flex gap above it. */
	.status.idle {
		margin-top: calc(-1 * var(--space-1));
	}
	.status.over {
		color: var(--accent-red, var(--text-primary));
	}
	.count {
		margin-left: var(--space-2);
	}
</style>
