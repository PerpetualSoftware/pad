<script lang="ts">
	import { goto } from '$app/navigation';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import BottomSheet from '$lib/components/common/BottomSheet.svelte';

	interface Props {
		/**
		 * Force the mobile (BottomSheet) branch regardless of the internal
		 * viewport detection. Use this when an ancestor component already
		 * owns the mobile/desktop decision (e.g. TopBar branches on
		 * `uiStore.isMobile` at ≤768px but this component's own breakpoint
		 * is ≤639.98px — without the override, 640–768px would show the
		 * desktop dropdown inside a mobile layout).
		 */
		mobile?: boolean;
	}

	let { mobile }: Props = $props();

	let open = $state(false);

	// ── Viewport detection ───────────────────────────────────────────────
	// Track mobile viewport so we can swap the absolute-positioned dropdown
	// for a full-width BottomSheet that reads better when workspace names
	// are long or the list is deep. Skipped when the caller passes an
	// explicit `mobile` prop.
	let detectedMobile = $state(false);
	$effect(() => {
		if (mobile !== undefined) return;
		if (typeof window === 'undefined') return;
		const mq = window.matchMedia('(max-width: 639.98px)');
		detectedMobile = mq.matches;
		const onChange = (e: MediaQueryListEvent) => {
			detectedMobile = e.matches;
			// Close the sheet if the viewport crosses above mobile while it's
			// open (e.g. rotation) so returning to mobile doesn't reopen it.
			if (!e.matches) {
				open = false;
			}
		};
		mq.addEventListener('change', onChange);
		return () => mq.removeEventListener('change', onChange);
	});

	// Also close the sheet if an ancestor-driven `mobile` prop flips off
	// while the sheet is open — same rotation-reopen guard as the internal
	// detection path.
	$effect(() => {
		if (mobile === false) {
			open = false;
		}
	});

	let isMobile = $derived(mobile ?? detectedMobile);

	function select(ws: { slug: string; owner_username?: string }) {
		open = false;
		// Close the mobile sidebar if open — the TopBar's previous inline
		// workspace links did this on click, so preserve the behavior now
		// that the switcher is the mobile nav entry point.
		uiStore.onNavigate();

		// Restore the last-visited route in this workspace if we have one
		// in localStorage (written by the workspace +layout on every nav).
		// Falls back to the dashboard on miss, parse error, storage error,
		// or any saved path that doesn't belong to this workspace (guards
		// against username changes / corrupt entries / cross-workspace
		// bleed). Implements IDEA-753 / TASK-754.
		const fallback = `/${ws.owner_username}/${ws.slug}`;
		let target = fallback;
		try {
			const saved = localStorage.getItem(`pad-last-route-${ws.slug}`);
			if (saved && (saved === fallback || saved.startsWith(fallback + '/'))) {
				target = saved;
			}
		} catch {
			// localStorage unavailable; fall through to dashboard.
		}
		goto(target);
	}

	function openCreateModal() {
		open = false;
		uiStore.onNavigate();
		uiStore.openCreateWorkspace();
	}
</script>

{#snippet workspaceList()}
	{#each workspaceStore.workspaces as ws (ws.slug)}
		<button
			class="item"
			class:active={ws.slug === workspaceStore.current?.slug}
			onclick={() => select(ws)}
		>
			{ws.name}
		</button>
	{/each}
	<button class="item create-trigger" onclick={openCreateModal}>
		+ New Workspace
	</button>
{/snippet}

<div class="switcher">
	<button class="current" onclick={() => open = !open}>
		<span class="name">{workspaceStore.current?.name ?? 'Select workspace'}</span>
		<span class="chevron">{open ? '▲' : '▼'}</span>
	</button>

	{#if isMobile && open}
		<!--
			Mobile: render the workspace list inside a BottomSheet so long
			workspace names don't clip and the hit targets are full-width.
			Gate on `open` (gate-on-open pattern) so the sheet (and its
			global keydown listener) isn't mounted when the switcher is idle.
		-->
		<BottomSheet
			{open}
			onclose={() => (open = false)}
			title="Switch workspace"
		>
			<div class="sheet-body">
				{@render workspaceList()}
			</div>
		</BottomSheet>
	{:else if open}
		<!-- svelte-ignore a11y_click_events_have_key_events -->
		<!-- svelte-ignore a11y_no_static_element_interactions -->
		<div class="backdrop" onclick={() => open = false}></div>
		<div class="dropdown">
			{@render workspaceList()}
		</div>
	{/if}
</div>

<style>
	.switcher { position: relative; }
	.current {
		width: 100%;
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-2) var(--space-3);
		background: var(--bg-secondary);
		border-radius: var(--radius);
		font-weight: 600;
		font-size: 0.9em;
	}
	.name {
		flex: 1;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.current:hover { background: var(--bg-tertiary); }
	.chevron { font-size: 0.7em; color: var(--text-muted); flex-shrink: 0; }
	.backdrop { position: fixed; inset: 0; z-index: 10; }
	.dropdown {
		position: absolute;
		top: 100%;
		left: 0;
		min-width: 240px;
		margin-top: 4px;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		box-shadow: 0 8px 24px rgba(0, 0, 0, 0.3);
		z-index: 11;
		overflow: hidden;
	}
	.item {
		display: block;
		width: 100%;
		text-align: left;
		padding: var(--space-2) var(--space-4);
		background: none;
		border: none;
		color: var(--text-primary);
		cursor: pointer;
		font-size: 0.95em;
	}
	.item:hover { background: var(--bg-hover); }
	.item.active { background: var(--bg-active); color: var(--accent-blue); }
	.create-trigger { color: var(--text-muted); border-top: 1px solid var(--border); }

	/* Inside the mobile sheet, give the rows a bit more vertical padding
	   to be thumb-reachable. */
	.sheet-body {
		display: flex;
		flex-direction: column;
		padding: 0 var(--space-2) var(--space-3);
	}
	.sheet-body .item {
		padding: var(--space-3);
		font-size: 1em;
		border-radius: var(--radius-sm);
	}
</style>
