<script lang="ts">
	import { goto } from '$app/navigation';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import BottomSheet from '$lib/components/common/BottomSheet.svelte';
	import { workspaceRestoreTarget } from '$lib/utils/workspace-route';

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
		// IDEA-760: preserve mobile sidebar visibility across workspace
		// switches. Previously this called uiStore.onNavigate() to mirror
		// the TopBar's old inline-link behavior, but per the idea the
		// switcher must work as a navbar control whether the sidebar is
		// open or hidden, and the user's sidebar state should carry over
		// to the new workspace.
		//
		// Click on the *current* workspace overrides the last-route
		// restore — gives the user a path back to the workspace
		// dashboard from any deep route. Mirrors TopBar.handleWsClick.
		// Use workspaceStore.current (rather than ws.owner_username,
		// which is typed optional) for the dashboard URL — when isCurrent
		// is true we know `current` is non-null and shares this slug, so
		// its `owner_username` is guaranteed present. Avoids producing
		// `//slug` (scheme-relative URL) if a caller passes a workspace
		// shape without owner_username.
		const current = workspaceStore.current;
		const isCurrent = !!current && ws.slug === current.slug;
		const target = isCurrent
			? `/${current.owner_username}/${current.slug}`
			: workspaceRestoreTarget(ws);
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
	<button
		class="current"
		onclick={() => open = !open}
		aria-haspopup="menu"
		aria-expanded={open}
	>
		<span class="name">{workspaceStore.current?.name ?? 'Select workspace'}</span>
		<span class="chevron" aria-hidden="true">{open ? '▲' : '▼'}</span>
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
