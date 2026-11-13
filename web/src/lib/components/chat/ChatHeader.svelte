<script lang="ts">
	import type { Snippet } from 'svelte';
	import IconButton from '$lib/components/common/IconButton.svelte';
	import { chatSurface } from '$lib/stores/chatSurface.svelte';

	// The one header row of every chat view (TASK-4515): back to the inbox, the title with
	// what is under it (presence on a thread), and Close in the host. `title` is the page's
	// h1; a view whose own state carries the h1 (not found, failed) passes none.
	let {
		title = '',
		back = false,
		onClose,
		sub,
		lead,
		actions
	}: {
		title?: string;
		/** A back chevron to the inbox, through Chat's own navigation. */
		back?: boolean;
		/** Present in the host (dock, drawer, sheet) only. */
		onClose?: () => void;
		sub?: Snippet;
		/** In place of the back chevron (the /messages route inbox puts its Back link here). */
		lead?: Snippet;
		/** Controls before Close (the thread's Jump to unread). */
		actions?: Snippet;
	} = $props();
</script>

<header class="chat-header">
	{#if lead}
		<div class="lead">{@render lead()}</div>
	{:else if back}
		<!-- A plain anchor, not IconButton's href, so the chat link census sees its interception. -->
		<a
			class="back"
			href="/messages"
			aria-label="All conversations"
			onclick={(e) => chatSurface.handleLinkClick(e, '/messages')}
		>
			<span class="glyph" aria-hidden="true"
				><svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"
					><path d="M12.5 4.5 7 10l5.5 5.5" /></svg
				></span
			>
			<span class="sr-only">All conversations</span>
		</a>
	{/if}
	<div class="titles">
		{#if title}<h1 {title}>{title}</h1>{/if}
		{#if sub}<div class="sub">{@render sub()}</div>{/if}
	</div>
	{#if actions}
		<div class="actions">{@render actions()}</div>
	{/if}
	{#if onClose}
		<IconButton label="Close Chat" class="close-chat" onclick={() => onClose()}>
			<svg viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"
				><path d="m5 5 10 10M15 5 5 15" /></svg
			>
		</IconButton>
	{/if}
</header>

<style>
	.chat-header {
		display: flex;
		align-items: center;
		gap: var(--space-1);
		min-height: 48px;
		flex-shrink: 0;
		box-sizing: border-box;
		padding: 0 var(--space-2);
		border-bottom: 1px solid var(--border);
	}
	/* Drawn as IconButton md: 32px, 44px on a phone or a coarse pointer. */
	.back {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		flex-shrink: 0;
		width: 32px;
		height: 32px;
		border-radius: var(--radius);
		color: var(--text-secondary);
		text-decoration: none;
	}
	.back:hover {
		background: var(--bg-hover);
		color: var(--text-primary);
		text-decoration: none;
	}
	.back:focus-visible {
		outline: 2px solid color-mix(in srgb, var(--accent-primary) 65%, transparent);
		outline-offset: 2px;
	}
	.back .glyph {
		display: inline-flex;
	}
	.back svg {
		width: 18px;
		height: 18px;
	}
	@media (max-width: 768px), (pointer: coarse) {
		.back {
			width: 44px;
			height: 44px;
		}
		.back svg {
			width: 20px;
			height: 20px;
		}
	}
	.lead {
		flex-shrink: 0;
		padding-left: var(--space-2);
		font-size: 13px;
	}
	.actions {
		display: flex;
		align-items: center;
		flex-shrink: 0;
		gap: var(--space-1);
	}
	.titles {
		flex: 1;
		min-width: 0;
		padding: 0 var(--space-2);
		display: flex;
		flex-direction: column;
		justify-content: center;
	}
	h1 {
		margin: 0;
		font-size: 15px;
		font-weight: 600;
		line-height: 1.3;
		color: var(--text-primary);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.sub {
		display: flex;
		align-items: center;
		min-width: 0;
		font-size: 12px;
		line-height: 1.3;
		color: var(--text-secondary);
	}
	.sub :global(.presence-label) {
		font-size: 12px;
		overflow: hidden;
		text-overflow: ellipsis;
	}
	@media (max-width: 768px) {
		.chat-header {
			min-height: 52px;
			padding: 0 var(--space-1);
		}
	}
</style>
