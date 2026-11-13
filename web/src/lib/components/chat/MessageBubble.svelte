<script lang="ts">
	import { longPress } from '$lib/chat/longPress';
	import MessageMenu from '$lib/components/chat/MessageMenu.svelte';
	import MessageBody from '$lib/components/chat/MessageBody.svelte';
	import type { ChatMessage, ChatRef } from '$lib/types';
	import { tick } from 'svelte';
	import Modal from '$lib/components/common/Modal.svelte';
	import Button from '$lib/components/common/Button.svelte';
	import IconButton from '$lib/components/common/IconButton.svelte';
	import ChatAvatar from '$lib/components/chat/ChatAvatar.svelte';
	import { copyToClipboard } from '$lib/utils/clipboard';
	import { toastStore } from '$lib/stores/toast.svelte';

	// One confirmed row. Only a ref the server called `visible` becomes a link;
	// `restricted` is a placeholder with no href and no title; anything else
	// stays literal, `[[...]]` kept. A tombstone has no body or actions.
	// `head` (TASK-4515): the first row of a group shows the meta line; a continuation
	// keeps its author, kind and time as visually hidden text.
	let {
		message,
		head = true,
		refs,
		onJumpToQuote,
		onReply,
		onDelete,
		canDelete = false
	}: {
		message: ChatMessage;
		head?: boolean;
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
	let kind = $derived(message.author_is_human ? 'Human' : 'Agent');
	let time = $derived(
		new Date(message.created_at).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })
	);
	let stamp = $derived(new Date(message.created_at).toLocaleString());
	let quote = $derived(message.reply_to);
</script>

<!-- svelte-ignore a11y_no_noninteractive_element_interactions, a11y_no_noninteractive_tabindex -->
<article
	class="bubble"
	class:head
	class:mine={message.mine}
	class:human={message.author_is_human}
	class:deleted={message.deleted}
	class:open={menuOpen}
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
	{#if head}
		<header class="meta">
			{#if !message.mine}<ChatAvatar key={author} />{/if}
			<span class="author">{author}</span>
			<!-- Your own rows say "You"; "Human" stays for assistive tech. -->
			<span class={message.mine ? 'sr-only' : 'kind'}>{kind}</span>
			<time datetime={message.created_at} title={stamp}>{time}</time>
			{#if message.context_workspace}
				<span class="from">sent from {message.context_workspace.name}</span>
			{/if}
		</header>
	{:else}
		<!-- A continuation: the group head above carries the chrome, but each row still says who and when. -->
		<span class="sr-only"
			><span>{author}</span> <span>{kind}</span> <time datetime={message.created_at} title={stamp}>{time}</time></span
		>
	{/if}
	{#if !message.deleted}
		<!-- Fine pointer: a toolbar on hover or focus. Touch: More only, visible on a group head;
		     a continuation keeps it focusable but clipped, and the long press opens the same menu. -->
		<div class="actions">
			<IconButton size="sm" touch={false} class="quick" label="Reply" onclick={reply}>
				<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"
					><path d="M6.5 4 3 7.5 6.5 11M3.5 7.5H10a3 3 0 0 1 3 3V12" /></svg
				>
			</IconButton>
			<IconButton size="sm" touch={false} class="quick" label="Copy" onclick={copy}>
				<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round"
					><rect x="5.5" y="5.5" width="7.5" height="7.5" rx="1.5" /><path d="M10.5 3.5V3A1 1 0 0 0 9.5 2H4a1 1 0 0 0-1 1v5.5a1 1 0 0 0 1 1h.5" /></svg
				>
			</IconButton>
			<IconButton
				size="sm"
				touch={false}
				class="more"
				bind:element={more}
				label={`More actions for ${author}, message ${message.seq}`}
				aria-haspopup="menu"
				aria-expanded={menuOpen}
				onclick={() => { more?.focus({ preventScroll: true }); menuOpen = !menuOpen; }}
			>
				<svg viewBox="0 0 16 16" fill="currentColor"
					><circle cx="3.5" cy="8" r="1.25" /><circle cx="8" cy="8" r="1.25" /><circle cx="12.5" cy="8" r="1.25" /></svg
				>
			</IconButton>
		</div>
	{/if}
	{#if message.deleted}
		<p class="body tombstone">Message deleted</p>
	{:else}
		<div class="payload">
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
		</div>
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
		<Button touch class="copy-close" onclick={() => (copyText = null)}>Close</Button>
	</div>
</Modal>

<style>
	/* Flat full-width rows; your own text sits in a right-aligned tinted bubble (TASK-4515).
	   The row bleeds into the scroller's padding so the hover fill reaches the edge. */
	.bubble {
		position: relative;
		box-sizing: border-box;
		min-width: 0;
		align-self: stretch;
		margin: 2px calc(-1 * var(--space-2)) 0;
		padding: 2px var(--space-2);
		border-radius: var(--radius);
		background: transparent;
		/* A touch long press opens the action menu: keep iOS from starting its own callout under the same hold. */
		-webkit-touch-callout: none;
	}
	/* The long press does not arm on a link, so a link keeps iOS's own long-press actions. */
	.bubble :global(a) { -webkit-touch-callout: default; }
	.bubble.head { margin-top: var(--space-3); }
	.bubble.mine {
		display: flex;
		flex-direction: column;
		align-items: flex-end;
	}
	.meta {
		display: flex;
		align-items: center;
		gap: 6px;
		min-width: 0;
		min-height: 24px;
		margin-bottom: 2px;
		font-size: 12px;
		color: var(--text-muted);
		white-space: nowrap;
	}
	.mine .meta { justify-content: flex-end; max-width: 100%; }
	.author {
		font-size: 13px;
		font-weight: 600;
		color: var(--text-primary);
	}
	.kind {
		padding: 0 5px;
		border: 1px solid var(--border);
		border-radius: 4px;
		font-size: 10px;
		font-weight: 600;
		letter-spacing: 0.04em;
		line-height: 16px;
		text-transform: uppercase;
		color: var(--text-secondary);
	}
	.from {
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
	}
	.from::before { content: '·'; margin-right: 6px; }
	.payload {
		min-width: 0;
		max-width: 100%;
		font-size: 14px;
		line-height: 1.5;
		overflow-wrap: anywhere;
	}
	.payload :global(.md) { margin-top: 0; }
	.mine .payload {
		box-sizing: border-box;
		max-width: 85%;
		padding: 7px 12px;
		border: 1px solid color-mix(in srgb, var(--accent-primary) 22%, transparent);
		border-radius: 14px 14px 4px 14px;
		background: color-mix(in srgb, var(--accent-primary) 16%, transparent);
		color: var(--text-primary);
	}
	.mine:not(.head) .payload { border-top-right-radius: 4px; }
	.body {
		margin: 0;
		overflow-wrap: anywhere;
	}
	.tombstone {
		font-size: 13px;
		font-style: italic;
		color: var(--text-muted);
	}
	.quote {
		display: flex;
		align-items: center;
		gap: 6px;
		box-sizing: border-box;
		width: 100%;
		min-height: 28px;
		margin: 0 0 4px;
		padding: 4px 8px;
		border: 0;
		border-left: 3px solid var(--border-strong);
		border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
		background: var(--bg-hover);
		font: inherit;
		font-size: 12px;
		line-height: 1.4;
		color: var(--text-secondary);
		text-align: left;
		cursor: pointer;
	}
	.quote:focus-visible {
		outline: 2px solid color-mix(in srgb, var(--accent-primary) 65%, transparent);
		outline-offset: 2px;
	}
	.quote-author {
		flex-shrink: 0;
		font-weight: 600;
		color: var(--text-primary);
	}
	.quote-text {
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	@media (max-width: 768px), (pointer: coarse) {
		.quote { min-height: 44px; }
	}

	/* Actions take no row. Opacity, not visibility, so More stays tabbable. */
	.actions {
		position: absolute;
		z-index: 1;
		top: -14px;
		right: var(--space-2);
		display: flex;
		padding: 2px;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-raised);
		box-shadow: var(--shadow-sm);
		opacity: 0;
	}
	.mine .actions { right: auto; left: var(--space-2); }
	@media (hover: hover) {
		.bubble:hover .actions,
		.bubble:focus-within .actions,
		.bubble.open .actions { opacity: 1; }
		.bubble:hover:not(.mine),
		.bubble.open:not(.mine) { background: var(--bg-hover); }
	}
	@media (hover: none) {
		/* One quiet More on the group head's meta line, a 44px target centred on it. */
		.actions {
			top: -8px;
			right: 0;
			padding: 0;
			border: 0;
			background: none;
			box-shadow: none;
			opacity: 1;
		}
		.mine .actions { left: 0; }
		.actions :global(.quick) { display: none; }
		.actions :global(.more) {
			width: 44px;
			height: 44px;
			color: var(--text-muted);
		}
		.bubble.head:not(.mine) .meta { padding-right: 40px; }
		.bubble.head.mine .meta { padding-left: 40px; }
		/* A continuation keeps More for the keyboard and screen readers; the long press opens it. */
		.bubble:not(.head) .actions {
			width: 1px;
			height: 1px;
			overflow: hidden;
			clip-path: inset(50%);
		}
	}

	.copy-surface { display: grid; gap: var(--space-3); padding: var(--space-4); }
	.copy-surface textarea { box-sizing: border-box; width: 100%; resize: vertical; color: var(--text-primary); background: var(--bg-primary); }
	.copy-surface textarea:focus-visible { outline: 2px solid var(--text-primary); outline-offset: -2px; }
	.copy-surface :global(.copy-close) { justify-self: end; }
</style>
