<script lang="ts">
	import { onMount, untrack } from 'svelte';
	import {
		MAX_BODY_BYTES,
		NOTICE,
		PLACEHOLDER_OP_KEY,
		utf8Bytes,
		validateDraft
	} from '$lib/chat/compose';
	import { registerComposer, type LateOutcome } from '$lib/chat/composerRegistry';
	import { getDraft, setDraft, clearDraft } from '$lib/chat/draftKeeper';
	import { inputMode } from '$lib/chat/inputMode.svelte';
	import type { AgentPresence } from '$lib/chat/presence';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { registerSessionExpiryKeeper } from '$lib/stores/sessionExpiryKeep';
	import { signOutDiscarding } from '$lib/stores/signOutGuard.svelte';
	import type { SendOutcome } from '$lib/stores/chatThread.svelte';
	import type { ChatMessage } from '$lib/types';
	import Button from '$lib/components/common/Button.svelte';
	import IconButton from '$lib/components/common/IconButton.svelte';

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
	// An IME commit can reach keydown after compositionend (Safari), so the flag outlives it by one task.
	let composing = false;

	let box = $state<HTMLTextAreaElement | null>(null);
	let sendButton = $state<HTMLButtonElement | undefined>(undefined);
	/** Reply puts the reader straight into the box (r5 H3). */
	export function focus() {
		box?.focus();
	}

	/** A refused send's text comes back, ahead of anything typed since, with its notice. The page uses it for a refused Retry too (r7 M1). */
	export function restore(body: string, outcome: LateOutcome | 'not_started' | 'send_error', replyToSeq: number | null = null) {
		text = text ? `${body}\n${text}` : body;
		if (replyToSeq !== null && replySeq === undefined) restoredReplySeq = replyToSeq;
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
		// A refusal that outlives its sender (dock closed, conversation left and re-entered) lands here.
		const unregister = registerComposer(owner, conversation, (body, outcome, reply) => restore(body, outcome, reply));
		return () => {
			saveDraft(text, replySeq);
			destroyed = true;
			unregister();
		};
	});

	// A soft keyboard coming up means touch: Enter stops sending. The surface holding the
	// composer fits the visual viewport (viewportOwner); this box does not lift itself.
	// Edge-triggered with a plain `let`, so it neither reads nor writes reactive state.
	let keyboardWas = false;
	$effect(() => {
		const up = uiStore.keyboardVisible;
		if (up && !keyboardWas) inputMode.softKeyboardUp();
		keyboardWas = up;
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

	// The caption line's priority (TASK-4515): status > presence note > draft note. Read in the
	// template only; the soft keyboard hides the two notes but never a status.
	let keyboardUp = $derived(uiStore.keyboardVisible);
	let statusShown = $derived(live || block === 'too_long');
	let presenceShown = $derived(!!presenceNote && !statusShown && !keyboardUp);
	let draftShown = $derived(!!text && !!draftNotice && !statusShown && !presenceShown && !keyboardUp);

	// The box grows with its text to the CSS max-height, then scrolls. Reads `text` and writes
	// only the element's style: no $state write, so it cannot invalidate itself.
	$effect(() => {
		void text;
		const el = box;
		if (!el) return;
		el.style.height = 'auto';
		if (el.scrollHeight > 0) el.style.height = `${el.scrollHeight}px`;
		else el.style.height = '';
	});

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
	onsubmit={(e) => {
		e.preventDefault();
		// Keep the box focused (and a touch keyboard up) through the send. Send itself never takes
		// focus on a press; a keyboard activation of Send returns to the box, because the button
		// disables once the box empties. Focus that was elsewhere (keyboard closed) is left alone.
		const active = document.activeElement;
		if (active === box || active === sendButton) box?.focus();
		void send();
	}}
>
	<!-- The header, the placeholder and Send's name say where this goes on screen; the full
	     line stays for assistive technology (TASK-4515 ruling 1). -->
	{#if agentKey}
		<p class="target sr-only">
			<strong>To {agentKey}</strong> · C{n}{#if title}<span class="target-title"> · {title}</span>{/if}
		</p>
	{/if}
	<div class="box">
		{#if replySeq !== undefined}
			<div class="reply">
				<span class="reply-text">
					{#if replyPreview}
						<strong>Replying to {replyPreview.mine ? 'yourself' : replyPreview.author_agent_key}:</strong> {replyPreview.body}
					{:else}
						Replying to message #{replySeq}
					{/if}
				</span>
				<IconButton label="Cancel reply" size="sm" touch onclick={clearReply}>
					<svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"
						><path d="m5 5 10 10M15 5 5 15" /></svg
					>
				</IconButton>
			</div>
		{/if}
		<div class="row">
			<textarea
				bind:this={box}
				bind:value={text}
				rows="1"
				placeholder={agentKey ? `Message ${agentKey}` : 'Message'}
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
			<Button
				bind:element={sendButton}
				type="submit"
				variant="primary"
				touch
				class="send"
				onpointerdown={(e) => e.preventDefault()}
				onmousedown={(e) => e.preventDefault()}
				disabled={block !== null}
				aria-label={agentKey ? `Send to ${agentKey}, conversation ${n}` : undefined}>Send</Button
			>
		</div>
	</div>
	<!-- One caption line, first of: status > presence note > draft note; the key hint only when
	     nothing else shows. The rest stay for assistive technology. With the soft keyboard up
	     only a status shows (ruling 5). -->
	<div class="caption">
		<!-- Mounted from the start, only its text changes: a region inserted with its text is not announced by some screen readers (r5 H5). -->
		<p class="status" class:sr-only={!statusShown} class:over={block === 'too_long' || block === 'too_large'} role="status">
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
		<p class="presence-note" class:sr-only={!presenceShown}>{presenceNote}</p>
		{#if text && draftNotice}<p class="draft-note" class:sr-only={!draftShown}>{draftNotice}</p>{/if}
		<p class="key-hint" class:sr-only={statusShown || presenceShown || draftShown}>
			{inputMode.hardware ? 'Enter sends · Shift+Enter adds a line' : 'Enter adds a line · use Send to send'}
		</p>
	</div>
</form>

<style>
	.composer {
		flex-shrink: 0;
		padding: var(--space-2) var(--space-3) var(--space-3);
	}
	.box {
		display: flex;
		flex-direction: column;
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		background: var(--bg-tertiary);
		transition: border-color 0.12s, box-shadow 0.12s;
	}
	.box:focus-within {
		border-color: var(--accent-primary);
		box-shadow: 0 0 0 2px color-mix(in srgb, var(--accent-primary) 30%, transparent);
	}
	.row {
		display: flex;
		gap: var(--space-2);
		align-items: flex-end;
		padding: 6px;
	}
	/* The box carries focus; the field draws nothing of its own (outranks app.css's ring). */
	.box textarea {
		flex: 1;
		min-width: 0;
		box-sizing: border-box;
		resize: none;
		margin: 0;
		border: 0;
		outline: none;
		background: transparent;
		color: var(--text-primary);
		font: inherit;
		font-size: 14px;
		line-height: 1.45;
		padding: 5px 6px;
		min-height: 32px;
		max-height: 160px;
		overflow-y: auto;
	}
	/* A text field always matches :focus-visible when focused; the box's :focus-within ring is
	   the indicator, so the field's own ring would be a second one inside it. */
	.box textarea:focus-visible {
		border: 0;
		outline: none;
	}
	.box :global(.send) {
		height: 32px;
		flex-shrink: 0;
	}
	.reply {
		display: flex;
		gap: var(--space-2);
		align-items: center;
		min-width: 0;
		margin: 6px 6px 0;
		padding: 2px 2px 2px 8px;
		border-left: 3px solid var(--accent-primary);
		border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
		background: var(--bg-hover);
		font-size: 12px;
		color: var(--text-secondary);
	}
	.reply-text {
		flex: 1;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.reply-text strong {
		color: var(--text-primary);
	}
	.caption {
		display: flex;
		gap: var(--space-3);
		margin: 6px 4px 0;
		font-size: 12px;
		line-height: 1.4;
		color: var(--text-muted);
	}
	.caption p {
		margin: 0;
	}
	/* The status region is never removed from the page: idle, it is sr-only (r5 H5). */
	.caption p:not([role="status"]):empty {
		display: none;
	}
	.caption .key-hint {
		margin-left: auto;
		white-space: nowrap;
	}
	.status.over {
		color: var(--accent-red);
	}
	.count {
		margin-left: var(--space-2);
	}
	@media (max-width: 768px), (pointer: coarse) {
		.composer {
			padding: var(--space-2) var(--space-3);
		}
		.row {
			padding: 4px;
		}
		.box textarea {
			font-size: 16px;
			padding: 10px 8px;
			min-height: 44px;
			max-height: 132px;
		}
		.box :global(.send) {
			height: auto;
		}
		/* Visually hidden on the size axis, still read (the same recipe as app.css .sr-only). */
		.caption .key-hint {
			position: absolute;
			width: 1px;
			height: 1px;
			margin: -1px;
			overflow: hidden;
			clip: rect(0, 0, 0, 0);
		}
	}
</style>
