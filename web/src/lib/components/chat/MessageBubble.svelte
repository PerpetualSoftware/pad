<script lang="ts">
	import { longPress } from '$lib/chat/longPress';
	import MessageMenu from '$lib/components/chat/MessageMenu.svelte';
	import type { ChatMessage } from '$lib/types';

	// One confirmed message (TASK-4044 PR 3a). The body is Svelte text
	// interpolation, never {@html}: bodies are plain text (IDEA-4083 tracks
	// markdown). A tombstone shows "Message deleted" and nothing of the body.
	//
	// PR 3b: with `onReply` and `onDelete` a live message has an action menu. It
	// opens on hover or keyboard focus (:focus-visible; a tap focuses the bubble
	// too, and must open nothing) and on a 500 ms long-press (touch). A tombstone
	// never has one.
	let {
		message,
		onJumpToQuote,
		onReply,
		onDelete
	}: {
		message: ChatMessage;
		onJumpToQuote?: (seq: number) => void;
		onReply?: (m: ChatMessage) => void;
		onDelete?: (m: ChatMessage) => void;
	} = $props();

	let el = $state<HTMLElement | null>(null);
	let hovered = $state(false);
	let focused = $state(false);
	let pinned = $state(false);
	let hasMenu = $derived(!message.deleted && !!onReply && !!onDelete);
	// Set while focus is handed back to the bubble after an action: that focus is
	// programmatic and must not reopen the menu it just closed.
	let returning = false;
	let menuOpen = $derived(hasMenu && (hovered || focused || pinned));

	// A pinned (long-pressed) menu closes on a press anywhere but the menu itself.
	$effect(() => {
		if (!pinned) return;
		const away = (e: PointerEvent) => {
			if (!(e.target as Element).closest?.('.menu')) pinned = false;
		};
		window.addEventListener('pointerdown', away, true);
		return () => window.removeEventListener('pointerdown', away, true);
	});

	let author = $derived(message.mine ? 'You' : message.author_agent_key || 'Unknown');
	let time = $derived(
		new Date(message.created_at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
	);
	let quote = $derived(message.reply_to);
</script>

<!-- The article is the hover/focus/long-press target for the menu; the menu's own buttons are the interactive controls. -->
<!-- svelte-ignore a11y_no_noninteractive_element_interactions, a11y_no_noninteractive_tabindex -->
<article
	class="bubble"
	class:mine={message.mine}
	class:deleted={message.deleted}
	id="msg-{message.seq}"
	data-seq={message.seq}
	tabindex={hasMenu ? 0 : message.deleted ? -1 : undefined}
	bind:this={el}
	use:longPress={() => {
		if (hasMenu) pinned = true;
	}}
	onpointerenter={(e) => (hovered = e.pointerType !== 'touch')}
	onpointerleave={() => (hovered = false)}
	onfocusin={(e) => {
		if (!returning && (e.target as Element).matches(':focus-visible')) focused = true;
	}}
	onfocusout={(e) => {
		if (!el?.contains(e.relatedTarget as Node | null)) focused = false;
	}}
	onkeydown={(e) => {
		if (e.key === 'Escape') pinned = focused = hovered = false;
	}}
	oncontextmenu={(e) => {
		if (pinned) e.preventDefault();
	}}
>
	<header class="meta">
		<span class="author">{author}</span>
		<time datetime={message.created_at}>{time}</time>
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
		<p class="body">{message.body}</p>
	{/if}
	{#if menuOpen && onReply && onDelete}
		<MessageMenu
			{message}
			{onReply}
			{onDelete}
			onDone={() => {
				pinned = focused = hovered = false;
				// The menu is about to unmount: keep focus on the bubble, not the body (r5 H3).
				// A deleted row keeps tabindex -1, so it still holds focus as the tombstone.
				returning = true;
				el?.focus();
				returning = false;
			}}
		/>
	{/if}
</article>

<style>
	.bubble {
		max-width: min(36rem, 85%);
		align-self: flex-start;
		padding: var(--space-2) var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		background: var(--bg-secondary);
		-webkit-touch-callout: none;
	}
	.bubble.mine {
		align-self: flex-end;
		background: var(--bg-hover);
	}
	.meta {
		display: flex;
		gap: var(--space-2);
		align-items: baseline;
		font-size: 0.75rem;
		color: var(--text-secondary);
	}
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
		white-space: pre-wrap;
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
