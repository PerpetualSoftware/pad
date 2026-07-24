<script lang="ts">
	import type { Snippet } from 'svelte';
	import type { Collection } from '$lib/types';
	import Button from '$lib/components/common/Button.svelte';

	interface Props {
		/** Collection mode (legacy): collection-flavored copy + create CTA +
		 *  agent-prompt hint. Generic mode: omit collection and pass
		 *  icon/title/message/actions instead. */
		collection?: Collection;
		wsSlug?: string;
		oncreate?: () => void;
		/** Generic mode: leading icon/emoji. */
		icon?: string;
		/** Generic mode: bold headline above the message. */
		title?: string;
		/** Generic mode: muted body text. */
		message?: string;
		/** Generic mode: actions row (Button primitives). */
		actions?: Snippet;
	}

	let {
		collection,
		wsSlug,
		oncreate,
		icon,
		title,
		message: genericMessage,
		actions
	}: Props = $props();

	const messages: Record<string, string> = {
		tasks: 'No tasks yet. Create your first task to start tracking work.',
		ideas: 'No ideas captured yet. Jot down your first idea.',
		plans: 'No plans defined. Create a plan to organize your milestones.',
		docs: 'No documents yet. Start writing to build your knowledge base.',
		bugs: "No bugs reported. That's either great news or you haven't looked yet.",
		conventions: 'No conventions set. Define rules for how agents should work.'
	};

	let message = $derived(
		collection
			? (messages[collection.slug] ?? `No ${collection.name.toLowerCase()} yet.`)
			: (genericMessage ?? '')
	);

	let singularName = $derived.by(() => {
		if (!collection) return '';
		const name = collection.name;
		if (name.endsWith('s') && name.length > 1) {
			return name.slice(0, -1).toLowerCase();
		}
		return name.toLowerCase();
	});

	let agentPrompt = $derived(`/pad create a ${singularName} for...`);

	let copied = $state(false);

	async function copyPrompt() {
		try {
			await navigator.clipboard.writeText(agentPrompt);
			copied = true;
			setTimeout(() => (copied = false), 1500);
		} catch {
			// Clipboard API not available
		}
	}
</script>

<div class="empty-state">
	{#if collection}
		<div class="empty-icon">{collection.icon || ''}</div>
		<p class="empty-message">{message}</p>
		{#if oncreate}
			<div class="create-btn-wrap">
				<Button variant="primary" onclick={oncreate}>
					+ Create {singularName[0].toUpperCase() + singularName.slice(1)}
				</Button>
			</div>
		{/if}
		<div class="agent-hint">
			<span class="hint-text">Or try: <code>{agentPrompt}</code></span>
			<button class="copy-btn" onclick={copyPrompt}>
				{copied ? 'Copied' : 'Copy'}
			</button>
		</div>
	{:else}
		{#if icon}<div class="empty-icon">{icon}</div>{/if}
		{#if title}<p class="empty-title">{title}</p>{/if}
		{#if message}<p class="empty-message">{message}</p>{/if}
		{#if actions}
			<div class="empty-actions">{@render actions()}</div>
		{/if}
	{/if}
</div>

<style>
	.empty-state {
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		padding: var(--space-10) var(--space-6);
		text-align: center;
	}

	.empty-icon {
		font-size: 3em;
		color: var(--text-muted);
		opacity: 0.4;
		margin-bottom: var(--space-4);
		line-height: 1;
	}

	.empty-title {
		color: var(--text-primary);
		font-size: 0.95em;
		font-weight: 600;
		margin: 0 0 var(--space-2) 0;
	}

	.empty-message {
		color: var(--text-secondary);
		font-size: 0.95em;
		max-width: 300px;
		margin: 0 0 var(--space-5) 0;
		line-height: 1.5;
	}

	.empty-actions {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	/* Layout-only wrapper — the button itself is the shared <Button> primitive. */
	.create-btn-wrap {
		margin-bottom: var(--space-5);
	}

	.agent-hint {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	.hint-text {
		color: var(--text-muted);
		font-size: 0.8em;
	}

	.hint-text code {
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: 3px;
		padding: 1px 5px;
		font-size: 0.95em;
	}

	.copy-btn {
		background: none;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-muted);
		font-size: 0.72em;
		padding: 2px 8px;
		cursor: pointer;
		white-space: nowrap;
		transition: color 0.15s, border-color 0.15s;
	}

	.copy-btn:hover {
		color: var(--text-primary);
		border-color: var(--text-muted);
	}
</style>
