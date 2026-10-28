<script lang="ts">
	import { onMount } from 'svelte';
	import {
		MAX_BODY_BYTES,
		NOTICE,
		PLACEHOLDER_OP_KEY,
		utf8Bytes,
		validateDraft
	} from '$lib/chat/compose';
	import { keepDraft, takeDraft } from '$lib/chat/draftKeeper';
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
		replyTo = null,
		onClearReply,
		onSend
	}: {
		n: number;
		replyTo?: ChatMessage | null;
		onClearReply: () => void;
		onSend: (body: string, replyToSeq?: number) => Promise<SendOutcome>;
	} = $props();

	let text = $state('');
	// Bodies sent and not yet answered. send() empties `text` before the POST, so on a 401 they
	// would be in neither the box nor the stash; the keeper adds them (r6 B2-L1).
	const inFlight: { body: string }[] = [];
	let notice = $state('');
	let inset = $state(0);

	let box = $state<HTMLTextAreaElement | null>(null);
	/** Reply puts the reader straight into the box (r5 H3). */
	export function focus() {
		box?.focus();
	}

	/** A refused send's text comes back, ahead of anything typed since, with its notice. The page uses it for a refused Retry too (r7 M1). */
	export function restore(body: string, outcome: Exclude<SendOutcome, 'sent' | 'failed' | 'stale'>) {
		text = text ? `${body}\n${text}` : body;
		if (outcome !== 'empty') notice = NOTICE[outcome];
	}

	let bytes = $derived(utf8Bytes(text));
	let block = $derived(validateDraft(text, replyTo?.seq, PLACEHOLDER_OP_KEY));
	let showCount = $derived(bytes > MAX_BODY_BYTES * 0.75);
	let live = $derived(!!notice || showCount || block === 'too_large');

	onMount(() => {
		// A draft kept across a session-expiry redirect comes back for the same
		// user on the same thread only (N8); the page keys this component by `n`.
		text = takeDraft(authStore.userId, n);
		return watchKeyboardInset((px) => (inset = px));
	});

	// The redirect to /login keeps the identity, so a typed message would vanish.
	// A confirmed sign-out is the user choosing to discard (signOutDiscarding).
	$effect(() =>
		registerSessionExpiryKeeper(() => {
			if (signOutDiscarding()) return;
			keepDraft(authStore.userId, n, [...inFlight.map((f) => f.body), text].filter((p) => p !== '').join('\n'));
		})
	);

	async function send() {
		if (block) return;
		const body = text;
		const target = replyTo;
		text = '';
		notice = '';
		const pending = { body };
		inFlight.push(pending);
		let outcome: SendOutcome;
		try {
			outcome = await onSend(body, target?.seq);
		} finally {
			inFlight.splice(inFlight.indexOf(pending), 1);
		}
		if (outcome === 'stale') return;
		if (outcome === 'sent' || outcome === 'failed' || outcome === 'reply_deleted' || outcome === 'reply_missing') {
			if (replyTo?.seq === target?.seq) onClearReply();
		}
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
	{#if replyTo}
		<div class="reply">
			<span class="reply-text">
				Replying to {replyTo.mine ? 'yourself' : replyTo.author_agent_key}: {replyTo.body}
			</span>
			<button type="button" aria-label="Cancel reply" onclick={onClearReply}>×</button>
		</div>
	{/if}
	<div class="row">
		<textarea
			bind:this={box}
			bind:value={text}
			rows="2"
			placeholder="Message"
			aria-label="Message"
			oninput={() => (notice = '')}
		></textarea>
		<button type="submit" class="send" disabled={block !== null}>Send</button>
	</div>
	<!-- Mounted from the start, only its text changes: a region inserted with its text is not announced by some screen readers (r5 H5). -->
	<p class="status" class:idle={!live} class:over={block === 'too_long' || block === 'too_large'} role="status">
		{#if block === 'too_long'}
			{NOTICE.too_long}
		{:else if block === 'too_large'}
			{NOTICE.too_large}
		{:else if notice}
			{notice}
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
