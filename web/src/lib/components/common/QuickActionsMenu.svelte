<script lang="ts">
	import type { QuickAction, Item, Collection } from '$lib/types';
	import { parseFields, formatItemRef } from '$lib/types';
	import { toastStore } from '$lib/stores/toast.svelte';

	interface Props {
		actions: QuickAction[];
		item?: Item | null;
		collection: Collection;
		scope: 'item' | 'collection';
	}

	let { actions, item = null, collection, scope }: Props = $props();

	let open = $state(false);

	let filtered = $derived(actions.filter((a) => a.scope === scope));

	function resolvePrompt(action: QuickAction): string {
		let prompt = action.prompt;
		const fields = item ? parseFields(item) : {};

		const vars: Record<string, string> = {
			ref: item ? formatItemRef(item) ?? '' : '',
			title: item?.title ?? '',
			status: item ? String(fields['status'] ?? '') : '',
			priority: item ? String(fields['priority'] ?? '') : '',
			collection: collection.name,
			content: item?.content ? item.content.slice(0, 200) : '',
			fields: Object.entries(fields)
				.map(([k, v]) => `${k}: ${v}`)
				.join(', '),
			plan: item ? String(fields['plan'] ?? '') : '',
			phase: item ? String(fields['phase'] ?? fields['plan'] ?? '') : ''
		};

		for (const [key, value] of Object.entries(vars)) {
			prompt = prompt.replaceAll(`{${key}}`, value);
		}

		return prompt;
	}

	function copyToClipboard(text: string): boolean {
		// Try navigator.clipboard first
		if (navigator.clipboard?.writeText) {
			navigator.clipboard.writeText(text).catch(() => {});
			return true;
		}
		// Fallback: temporary textarea + execCommand
		const textarea = document.createElement('textarea');
		textarea.value = text;
		textarea.style.position = 'fixed';
		textarea.style.opacity = '0';
		document.body.appendChild(textarea);
		textarea.select();
		try {
			document.execCommand('copy');
			return true;
		} catch {
			return false;
		} finally {
			document.body.removeChild(textarea);
		}
	}

	function handleAction(action: QuickAction) {
		const resolved = resolvePrompt(action);
		if (copyToClipboard(resolved)) {
			toastStore.show('Copied to clipboard', 'success');
		} else {
			toastStore.show('Failed to copy to clipboard', 'error');
		}
		open = false;
	}

	function handleWindowClick(e: MouseEvent) {
		const target = e.target as HTMLElement;
		if (!target.closest('.quick-actions-menu')) {
			open = false;
		}
	}
</script>

<svelte:window onclick={handleWindowClick} />

{#if filtered.length > 0}
	<div class="quick-actions-menu">
		<button
			class="trigger-btn"
			onclick={(e) => { e.stopPropagation(); open = !open; }}
			title="Quick actions"
		>
			&#9889;
		</button>

		{#if open}
			<div class="dropdown">
				{#each filtered as action (action.label)}
					<button class="action-item" onclick={() => handleAction(action)}>
						{#if action.icon}
							<span class="action-icon">{action.icon}</span>
						{/if}
						<span class="action-label">{action.label}</span>
					</button>
				{/each}
				<div class="dropdown-tagline">Copy a prompt to your agent</div>
			</div>
		{/if}
	</div>
{/if}

<style>
	.quick-actions-menu {
		position: relative;
		display: inline-block;
	}

	.trigger-btn {
		padding: 2px var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.85em;
		cursor: pointer;
		transition: all 0.1s;
	}

	.trigger-btn:hover {
		background: var(--bg-tertiary);
		color: var(--text-primary);
	}

	.dropdown {
		position: absolute;
		top: 100%;
		right: 0;
		margin-top: var(--space-1);
		min-width: 200px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		box-shadow: 0 4px 12px rgba(0, 0, 0, 0.15);
		z-index: 50;
		padding: var(--space-1) 0;
	}

	.action-item {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		width: 100%;
		padding: var(--space-2) var(--space-3);
		background: none;
		border: none;
		color: var(--text-primary);
		font-size: 0.85em;
		cursor: pointer;
		text-align: left;
		transition: background 0.1s;
	}

	.action-item:hover {
		background: var(--bg-tertiary);
	}

	.action-icon {
		flex-shrink: 0;
	}

	.action-label {
		flex: 1;
		min-width: 0;
	}

	.dropdown-tagline {
		padding: var(--space-2) var(--space-3);
		font-size: 0.72em;
		color: var(--text-muted);
		border-top: 1px solid var(--border);
		text-align: center;
	}
</style>
