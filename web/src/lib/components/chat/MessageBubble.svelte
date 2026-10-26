<script lang="ts">
	import type { ChatMessage } from '$lib/types';

	// One confirmed message (TASK-4044 PR 3a). The body is Svelte text
	// interpolation, never {@html}: bodies are plain text (IDEA-4083 tracks
	// markdown). A tombstone shows "Message deleted" and nothing of the body.
	let {
		message,
		onJumpToQuote
	}: { message: ChatMessage; onJumpToQuote?: (seq: number) => void } = $props();

	let author = $derived(message.mine ? 'You' : message.author_agent_key || 'Unknown');
	let time = $derived(
		new Date(message.created_at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
	);
	let quote = $derived(message.reply_to);
</script>

<article
	class="bubble"
	class:mine={message.mine}
	class:deleted={message.deleted}
	id="msg-{message.seq}"
	data-seq={message.seq}
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
</article>

<style>
	.bubble {
		max-width: min(36rem, 85%);
		align-self: flex-start;
		padding: var(--space-2) var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		background: var(--bg-secondary);
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
