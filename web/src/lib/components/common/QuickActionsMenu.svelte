<script lang="ts">
	import type { QuickAction, Item, Collection } from '$lib/types';
	import { parseFields, formatItemRef } from '$lib/types';
	import { toastStore } from '$lib/stores/toast.svelte';
	import BottomSheet from '$lib/components/common/BottomSheet.svelte';

	interface Props {
		actions: QuickAction[];
		item?: Item | null;
		collection: Collection;
		scope: 'item' | 'collection';
	}

	let { actions, item = null, collection, scope }: Props = $props();

	let open = $state(false);
	let alignLeft = $state(false);
	let triggerEl = $state<HTMLButtonElement | null>(null);

	let filtered = $derived(actions.filter((a) => a.scope === scope));

	// ── Viewport detection ────────────────────────────────────────────────
	// Track mobile viewport so we can swap the absolute-positioned popover
	// for a BottomSheet that never clips off-screen.
	let isMobile = $state(false);
	$effect(() => {
		if (typeof window === 'undefined') return;
		const mq = window.matchMedia('(max-width: 639.98px)');
		isMobile = mq.matches;
		const onChange = (e: MediaQueryListEvent) => {
			isMobile = e.matches;
		};
		mq.addEventListener('change', onChange);
		return () => mq.removeEventListener('change', onChange);
	});

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

	function handleTriggerClick(e: MouseEvent) {
		e.stopPropagation();
		const nextOpen = !open;
		// Only compute alignment when opening on desktop; the mobile branch
		// renders a BottomSheet which doesn't need trigger-relative positioning.
		if (nextOpen && !isMobile && triggerEl) {
			const rect = triggerEl.getBoundingClientRect();
			// If the trigger is too close to the left edge, the default
			// right-anchored 200px dropdown would clip — switch to left-anchored.
			alignLeft = rect.left < 220;
		}
		open = nextOpen;
	}

	function handleWindowClick(e: MouseEvent) {
		// On mobile the BottomSheet owns dismissal (backdrop tap, Escape,
		// swipe-down) — skip the outside-click handler so it doesn't race.
		if (isMobile) return;
		const target = e.target as HTMLElement;
		if (!target.closest('.quick-actions-menu')) {
			open = false;
		}
	}
</script>

<svelte:window onclick={handleWindowClick} />

{#snippet actionList()}
	{#each filtered as action (action.label)}
		<button class="action-item" onclick={() => handleAction(action)}>
			{#if action.icon}
				<span class="action-icon">{action.icon}</span>
			{/if}
			<span class="action-label">{action.label}</span>
		</button>
	{/each}
	<div class="dropdown-tagline">Copy a prompt to your agent</div>
{/snippet}

{#if filtered.length > 0}
	<div class="quick-actions-menu">
		<button
			bind:this={triggerEl}
			class="trigger-btn"
			onclick={handleTriggerClick}
			title="Quick actions"
		>
			&#9889;
		</button>

		{#if isMobile}
			<BottomSheet open={open} onclose={() => (open = false)} title="Quick actions">
				{@render actionList()}
			</BottomSheet>
		{:else if open}
			<div class="dropdown" class:align-left={alignLeft}>
				{@render actionList()}
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
		/* Defensive cap so the popover can never overflow the viewport
		   horizontally, even if trigger placement or zoom produces an
		   edge case we didn't anticipate. */
		max-width: calc(100vw - var(--space-4));
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		box-shadow: 0 4px 12px rgba(0, 0, 0, 0.15);
		z-index: 50;
		padding: var(--space-1) 0;
	}

	/* When the trigger sits near the viewport's left edge, flip to
	   left-anchored so the dropdown opens rightward and stays on-screen. */
	.dropdown.align-left {
		right: auto;
		left: 0;
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
