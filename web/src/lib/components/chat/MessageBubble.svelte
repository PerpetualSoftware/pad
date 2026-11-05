<script lang="ts">
	import { longPress } from '$lib/chat/longPress';
	import MessageMenu from '$lib/components/chat/MessageMenu.svelte';
	import MessageBody from '$lib/components/chat/MessageBody.svelte';
	import type { ChatMessage, ChatRef } from '$lib/types';
	import { tick } from 'svelte';
	import Modal from '$lib/components/common/Modal.svelte';
	import { copyToClipboard } from '$lib/utils/clipboard';
	import { toastStore } from '$lib/stores/toast.svelte';

	// One confirmed row. Only a ref the server called `visible` becomes a link;
	// `restricted` is a placeholder with no href and no title; anything else
	// stays literal, `[[...]]` kept. A tombstone has no body or actions.
	let {
		message,
		refs,
		onJumpToQuote,
		onReply,
		onDelete,
		canDelete = false
	}: {
		message: ChatMessage;
		canDelete?: boolean;
		/** The thread's ref verdicts; absent means every ref stays literal. */
		refs?: ReadonlyMap<string, ChatRef>;
		onJumpToQuote?: (seq: number) => void;
		onReply?: (m: ChatMessage) => void;
		onDelete?: (m: ChatMessage) => void;
	} = $props();

	let el = $state<HTMLElement | null>(null);
	let more = $state<HTMLButtonElement>();
	let menuOpen = $state(false);
	let copyText = $state<string | null>(null);
	let copyBox = $state<HTMLTextAreaElement>();

	// Keep focus on the row before its focused controls disappear on success.
	$effect.pre(() => {
		if (message.deleted && el?.contains(document.activeElement)) el.focus();
	});
	$effect(() => {
		if (copyText !== null && copyBox) {
			copyBox.focus();
			copyBox.select();
		}
	});

	function closeMenu() {
		menuOpen = false;
		more?.focus({ preventScroll: true });
	}
	async function reply() {
		const target = message;
		closeMenu();
		await tick(); // let a mobile sheet return focus before the composer takes it
		onReply?.(target);
	}
	async function remove() {
		const target = message;
		closeMenu();
		await tick();
		onDelete?.(target);
	}
	async function copy() {
		const source = message.body;
		closeMenu();
		await tick();
		const copied = await copyToClipboard(source, { legacyFallback: false });
		more?.focus({ preventScroll: true });
		if (copied) toastStore.show('Copied to clipboard', 'success');
		else copyText = source;
	}
	let author = $derived(message.mine ? 'You' : message.author_agent_key || 'Unknown');
	let time = $derived(
		new Date(message.created_at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
	);
	let quote = $derived(message.reply_to);
</script>

<!-- svelte-ignore a11y_no_noninteractive_element_interactions, a11y_no_noninteractive_tabindex -->
<article
	class="bubble"
	class:mine={message.mine}
	class:human={message.author_is_human}
	class:deleted={message.deleted}
	id="msg-{message.seq}"
	data-seq={message.seq}
	tabindex={-1}
	bind:this={el}
	use:longPress={() => {
		if (!message.deleted) {
			more?.focus({ preventScroll: true });
			menuOpen = true;
		}
	}}
	oncontextmenu={(e) => { if (menuOpen) e.preventDefault(); }}
>
	<header class="meta">
		<div class="identity">
			<span class="author">{author}</span>
			<span>{message.author_is_human ? 'Human' : 'Agent'}</span>
			<time datetime={message.created_at} title={new Date(message.created_at).toLocaleString()}>{time}</time>
		</div>
		{#if !message.deleted}
			<div class="actions">
				<button class="more" type="button" bind:this={more} aria-label={`More actions for ${author}, message ${message.seq}`}
					aria-haspopup="menu" aria-expanded={menuOpen} onclick={() => { more?.focus({ preventScroll: true }); menuOpen = !menuOpen; }}>More</button>
				<div class="quick-actions">
					<button type="button" onclick={reply}>Reply</button>
					<button type="button" onclick={copy}>Copy</button>
				</div>
			</div>
		{/if}
	</header>
	{#if message.context_workspace}
		<p class="from">sent from {message.context_workspace.name}</p>
	{/if}
	{#if message.deleted}
		<p class="body tombstone">Message deleted</p>
	{:else}
		{#if quote}
			<button
				type="button"
				class="quote"
				onclick={() => onJumpToQuote?.(quote.seq)}
				aria-label="Go to the quoted message"
			>
				<span class="quote-author">{quote.author_is_human ? 'You' : quote.author_agent_key}</span>
				<span class="quote-text">{quote.deleted ? 'Deleted message' : quote.excerpt}</span>
			</button>
		{/if}
		<MessageBody body={message.body} {refs} ctxWorkspace={message.context_workspace?.slug} />
	{/if}
</article>

{#if !message.deleted}
	<MessageMenu open={menuOpen} trigger={more} {author} seq={message.seq} {canDelete}
		onclose={() => (menuOpen = false)} onReply={reply} onCopy={copy} onDelete={remove} />
{/if}
<Modal open={copyText !== null} onclose={() => (copyText = null)} ariaLabel="Select and copy this message." maxWidth="440px">
	<div class="copy-surface">
		<label for="copy-message-{message.id}">Select and copy this message.</label>
		<textarea id="copy-message-{message.id}" bind:this={copyBox} readonly value={copyText ?? ''} rows="6"></textarea>
		<button type="button" onclick={() => (copyText = null)}>Close</button>
	</div>
</Modal>

<style>
	.bubble {
		box-sizing: border-box;
		width: 100%;
		min-width: 0;
		align-self: stretch;
		padding: var(--space-2) var(--space-3);
		border-bottom: 1px solid var(--border);
		background: transparent;
		/* A touch long press opens the action menu: keep iOS from starting its own callout under the same hold. */
		-webkit-touch-callout: none;
	}
	.bubble.human { background: var(--bg-hover); }
	.meta {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		align-items: center;
		font-size: 0.75rem;
		color: var(--text-secondary);
	}
	.identity {
		display: flex;
		flex: 1 1 10rem;
		flex-wrap: wrap;
		gap: var(--space-2);
		min-width: 0;
		overflow-wrap: anywhere;
	}
	.actions { position: relative; flex: 0 1 10.5rem; height: 44px; margin-left: auto; }
	.more { position: absolute; right: 0; top: 0; }
	.quick-actions { position: absolute; right: 3.5rem; top: 0; display: flex; visibility: hidden; }
	.bubble:hover .quick-actions, .bubble:focus-within .quick-actions { visibility: visible; }
	button {
		min-height: 44px;
		min-width: 44px;
		padding: 0 var(--space-2);
		border: 0;
		border-radius: var(--radius-sm);
		background: transparent;
		color: var(--text-primary);
		font: inherit;
		cursor: pointer;
	}
	button:hover { background: var(--bg-secondary); }
	button:focus-visible, textarea:focus-visible { outline: 2px solid var(--text-primary); outline-offset: -2px; }
	.copy-surface { display: grid; gap: var(--space-3); padding: var(--space-4); }
	.copy-surface textarea { box-sizing: border-box; width: 100%; resize: vertical; color: var(--text-primary); background: var(--bg-primary); }
	@media (pointer: coarse) { .quick-actions { display: none; } .actions { flex-basis: 3.5rem; } }
	.author {
		font-weight: 600;
		color: var(--text-primary);
	}
	.from {
		margin: 0;
		font-size: 0.75rem;
		color: var(--text-secondary);
	}
	.body {
		margin: var(--space-1) 0 0;
		overflow-wrap: anywhere;
	}
	.tombstone {
		font-style: italic;
		color: var(--text-secondary);
	}
	.quote {
		display: block;
		width: 100%;
		margin-top: var(--space-1);
		padding: var(--space-1) var(--space-2);
		border: 0;
		border-left: 3px solid var(--border);
		background: transparent;
		text-align: left;
		cursor: pointer;
		font: inherit;
		font-size: 0.8rem;
		color: var(--text-secondary);
	}
	.quote-author {
		font-weight: 600;
		margin-right: var(--space-1);
	}
</style>
