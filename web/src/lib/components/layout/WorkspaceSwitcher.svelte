<script lang="ts">
	import { goto } from '$app/navigation';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { tabsStore } from '$lib/stores/tabs.svelte';
	import { authStore } from '$lib/stores/auth.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import BottomSheet from '$lib/components/common/BottomSheet.svelte';
	import { viewport } from '$lib/stores/breakpoint.svelte';
	import { workspaceRestoreTarget } from '$lib/utils/workspace-route';
	import RecentlyDeletedWorkspaces from '$lib/components/layout/RecentlyDeletedWorkspaces.svelte';
	import type { Workspace } from '$lib/types';

	interface Props {
		/**
		 * Force the mobile (BottomSheet) branch regardless of the shared
		 * viewport flag. Callers that live only inside mobile chrome (TopBar's
		 * mobile slot, MobileContextBar) pass `mobile={true}` so the switcher
		 * always renders as a sheet there. Left unset elsewhere, the switcher
		 * follows the shared breakpoint (now unified with the app chrome at
		 * ≤768px — TASK-2028).
		 */
		mobile?: boolean;
	}

	let { mobile }: Props = $props();

	let open = $state(false);

	// On mobile the absolute-positioned dropdown is swapped for a full-width
	// BottomSheet that reads better when workspace names are long or the list is
	// deep. Follows the shared breakpoint store unless the caller forces the
	// branch via the `mobile` prop.
	let isMobile = $derived(mobile ?? viewport.isMobile);
	let otherWorkspaces = $derived.by(() => {
		const openSlugs = new Set(tabsStore.tabs.map((tab) => tab.slug));
		return workspaceStore.workspaces.filter((ws) => !openSlugs.has(ws.slug));
	});

	// Close the sheet if the viewport (or an ancestor-driven `mobile` prop)
	// crosses above mobile while it's open (e.g. rotation) so returning to
	// mobile doesn't reopen it. Reads `isMobile`; writes only `open`.
	$effect(() => {
		if (!isMobile) open = false;
	});

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

	// Choosing a workspace outside the open set keeps its new tab, as in "+" search.
	async function selectOther(ws: Workspace) {
		open = false;
		const isSameIdentity = authStore.identityFence();
		await tabsStore.open(ws.slug, false).catch(() => {});
		if (!isSameIdentity()) return;
		goto(workspaceRestoreTarget(ws));
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

{#snippet mobileWorkspaceList()}
	{#each tabsStore.tabs as tab (tab.slug)}
		<button
			class="item"
			class:active={tab.slug === workspaceStore.current?.slug}
			data-ws-slug={tab.slug}
			style:font-style={tab.ephemeral ? 'italic' : undefined}
			onclick={() => select(tab)}
		>
			{tab.name}
		</button>
	{/each}
	{#if tabsStore.tabs.length > 0 && otherWorkspaces.length > 0}
		<hr class="workspace-divider" />
	{/if}
	{#each otherWorkspaces as ws (ws.slug)}
		<button
			class="item"
			class:active={ws.slug === workspaceStore.current?.slug}
			data-ws-slug={ws.slug}
			onclick={() => selectOther(ws)}
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
		aria-haspopup={isMobile ? 'dialog' : undefined}
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
				{@render mobileWorkspaceList()}
				<RecentlyDeletedWorkspaces active={open} roomy />
			</div>
		</BottomSheet>
	{:else if open}
		<!-- svelte-ignore a11y_click_events_have_key_events -->
		<!-- svelte-ignore a11y_no_static_element_interactions -->
		<div class="backdrop" onclick={() => open = false}></div>
		<div class="dropdown">
			{@render workspaceList()}
			<RecentlyDeletedWorkspaces active={open} />
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
	.workspace-divider { width: 100%; margin: var(--space-2) 0; border: 0; border-top: 1px solid var(--border); }

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
