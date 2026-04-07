<script lang="ts">
	import type { Collection } from '$lib/types';

	interface Props {
		collection: Collection;
		wsSlug: string;
		oncreate?: () => void;
	}

	let { collection, wsSlug, oncreate }: Props = $props();

	const messages: Record<string, string> = {
		tasks: 'No tasks yet. Create your first task to start tracking work.',
		ideas: 'No ideas captured yet. Jot down your first idea.',
		plans: 'No plans defined. Create a plan to organize your milestones.',
		docs: 'No documents yet. Start writing to build your knowledge base.',
		bugs: "No bugs reported. That's either great news or you haven't looked yet.",
		conventions: 'No conventions set. Define rules for how agents should work.'
	};

	let message = $derived(
		messages[collection.slug] ?? `No ${collection.name.toLowerCase()} yet.`
	);

	let singularName = $derived.by(() => {
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
	<div class="empty-icon">{collection.icon || ''}</div>
	<p class="empty-message">{message}</p>
	{#if oncreate}
		<button class="create-btn" onclick={oncreate}>
			+ Create {singularName[0].toUpperCase() + singularName.slice(1)}
		</button>
	{/if}
	<div class="agent-hint">
		<span class="hint-text">Or try: <code>{agentPrompt}</code></span>
		<button class="copy-btn" onclick={copyPrompt}>
			{copied ? 'Copied' : 'Copy'}
		</button>
	</div>
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

	.empty-message {
		color: var(--text-secondary);
		font-size: 0.95em;
		max-width: 300px;
		margin: 0 0 var(--space-5) 0;
		line-height: 1.5;
	}

	.create-btn {
		background: var(--accent-blue);
		color: #fff;
		border: none;
		padding: var(--space-2) var(--space-5);
		border-radius: var(--radius);
		font-size: 0.9em;
		font-weight: 600;
		cursor: pointer;
		transition: opacity 0.1s;
		margin-bottom: var(--space-5);
	}

	.create-btn:hover {
		opacity: 0.85;
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
