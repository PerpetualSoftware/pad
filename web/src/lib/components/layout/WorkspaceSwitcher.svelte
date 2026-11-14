<script lang="ts">
	import { onMount, tick, untrack } from 'svelte';
	import { avatarColor } from '$lib/utils/avatar';
	import { coveredPage } from '$lib/stores/coveredPage.svelte';
	import { portal } from '$lib/utils/portalAction';
	import { clickOutside } from '$lib/utils/clickOutside';
	import { pushEscapeHandler, runTopEscape, ESCAPE_PRIORITY } from '$lib/stores/escapeStack';
	import { pendingInvitations } from '$lib/stores/pendingInvitations.svelte';
	import { homeOverview } from '$lib/stores/homeOverview.svelte';
	import PendingInvitations from './PendingInvitations.svelte';
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
	current?: Workspace | null;
	}

	let { mobile, current = workspaceStore.current }: Props = $props();
	onMount(() => { if (!isMobile) void pendingInvitations.refresh(); });

	let open = $state(false);
	let trigger: HTMLButtonElement | undefined = $state();
	let panel: HTMLDivElement | undefined = $state();
	let filter = $state('');
	let invitationsOpen = $state(false);
	const matches = (ws: { name: string; slug: string }) => `${ws.name} ${ws.slug}`.toLocaleLowerCase().includes(filter.toLocaleLowerCase());
	const openSlugs = $derived(new Set(tabsStore.tabs.map(t => t.slug)));
	const sections = $derived([
		{ title: 'OPEN', rows: tabsStore.tabs.filter(matches) },
		{ title: 'YOUR OTHER WORKSPACES', rows: (workspaceStore.workspaces ?? []).filter(w => !openSlugs.has(w.slug) && !w.is_guest && matches(w)) },
		{ title: 'SHARED WITH YOU', rows: (workspaceStore.workspaces ?? []).filter(w => !openSlugs.has(w.slug) && w.is_guest && matches(w)) }
	]);
	function closeDesktop() { open = false; invitationsOpen = false; trigger?.focus(); }
	function place(node: HTMLElement) {
		const rect = trigger!.getBoundingClientRect();
		node.style.left = `${Math.max(8, Math.min(rect.left, window.innerWidth - 308))}px`;
		node.style.top = `${rect.bottom + 6}px`;
		node.style.maxHeight = `${Math.max(160, window.innerHeight - rect.bottom - 18)}px`;
		void tick().then(() => node.querySelector('input')?.focus());
	}
	function navigateList(event: KeyboardEvent) {
		if (event.key === 'Escape') { if (runTopEscape(event)) { event.preventDefault(); event.stopPropagation(); } return; }
		if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp') return;
		event.preventDefault();
		const rows = Array.from(panel?.querySelectorAll<HTMLElement>('button, a') ?? []);
		const index = rows.indexOf(document.activeElement as HTMLElement);
		rows[(index + (event.key === 'ArrowDown' ? 1 : -1) + rows.length) % rows.length]?.focus();
	}
	$effect(() => {
		if (!open || isMobile) return;
		return pushEscapeHandler(() => { closeDesktop(); return true; }, ESCAPE_PRIORITY.menu);
	});

	// On mobile the absolute-positioned dropdown is swapped for a full-width
	// BottomSheet that reads better when workspace names are long or the list is
	// deep. Follows the shared breakpoint store unless the caller forces the
	// branch via the `mobile` prop.
	let isMobile = $derived(mobile ?? viewport.isMobile);
	$effect(() => {
		if (!isMobile && coveredPage.active) untrack(() => { open = false; invitationsOpen = false; });
	});
	let otherWorkspaces = $derived.by(() => {
		const openSlugs = new Set(tabsStore.tabs.map((tab) => tab.slug));
		return (workspaceStore.workspaces ?? []).filter((ws) => !openSlugs.has(ws.slug));
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
		closeDesktop();
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

<div class="switcher" class:desktop={!isMobile}>
	<button
		class="current"
	bind:this={trigger}
		onclick={() => { filter = ''; open = !open; }}
		aria-haspopup="dialog"
		aria-expanded={open}
	>
		{#if !isMobile && current}<span class="workspace-avatar" style:background={avatarColor(current.name)}>{(current.name || current.slug).charAt(0).toUpperCase()}</span>{/if}
		<span class="name">{current?.name ?? 'Select workspace'}</span>
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
				<!-- Until the open set loads, every workspace would look "not open"
				     and a tap would pin it; show the plain list instead. -->
				{#if tabsStore.loaded}
					{@render mobileWorkspaceList()}
				{:else}
					{@render workspaceList()}
				{/if}
				<RecentlyDeletedWorkspaces active={open} roomy />
			</div>
		</BottomSheet>
	{:else if open}
		<div class="desktop-dropdown" role="dialog" aria-label="Switch workspace" tabindex="-1" bind:this={panel} use:portal use:place use:clickOutside={{ onOutside: closeDesktop, extra: () => [trigger] }} onkeydown={navigateList}>
			<input placeholder="Find a workspace…" aria-label="Find a workspace" bind:value={filter} />
			{#each sections as section}
				{#if section.rows.length}
					<h3>{section.title}</h3>
					{#each section.rows as ws (ws.slug)}
						<button class="workspace-option" onclick={() => { closeDesktop(); if (openSlugs.has(ws.slug)) select(ws); else void selectOther(ws as Workspace); }}>
							<span class="workspace-avatar" style:background={avatarColor(ws.name)}>{ws.name.charAt(0).toUpperCase()}</span><span class="option-name">{ws.name}</span>
							{#if ws.is_guest}<small>guest</small>{/if}
							{#if ws.slug === current?.slug}<span class="check" aria-label="Current workspace">✓</span>
							{:else if homeOverview.needs?.workspaces.find(w => w.slug === ws.slug)?.count}<span class="needs-count">{homeOverview.needs.workspaces.find(w => w.slug === ws.slug)?.count}</span>{/if}
						</button>
					{/each}
				{/if}
			{/each}
			<div class="switcher-actions">
				<button onclick={openCreateModal}>+ New workspace</button>
				<button onclick={() => invitationsOpen = !invitationsOpen} aria-expanded={invitationsOpen}>Invitations <span>{pendingInvitations.count}</span></button>
				{#if invitationsOpen}<PendingInvitations active={false} onaccepted={closeDesktop} />{#if !pendingInvitations.count}<p>No pending invitations.</p>{/if}{/if}
				<a href="/settings/deleted-workspaces" onclick={closeDesktop}>Deleted workspaces <small>Settings</small></a>
			</div>
		</div>

	{/if}
</div>

<style>
	.desktop .current { background: var(--bg-tertiary); border: 1px solid var(--border); padding: var(--space-2); gap: var(--space-2); font-size: .85rem; text-align: left; }
	.desktop .name { text-align: left; }
	.desktop-dropdown { position: fixed; width: 300px; z-index: 60; overflow-y: auto; padding: var(--space-2); background: var(--bg-secondary); border: 1px solid var(--border); border-radius: var(--radius-lg); box-shadow: 0 8px 24px rgba(0,0,0,.3); }
	.desktop-dropdown input { width: 100%; background: var(--bg-tertiary); border: 1px solid var(--border); border-radius: var(--radius); padding: var(--space-2) var(--space-3); color: var(--text-primary); }
	h3 { font-size: .65rem; color: var(--text-muted); margin: var(--space-4) var(--space-2) var(--space-1); }
	.workspace-option, .switcher-actions button, .switcher-actions a { width: 100%; display: flex; align-items: center; gap: var(--space-2); padding: var(--space-2); border: 0; border-radius: var(--radius-sm); background: none; color: var(--text-secondary); font-size: .8rem; text-align: left; text-decoration: none; cursor: pointer; }
	.workspace-option:hover, .switcher-actions button:hover, .switcher-actions a:hover { background: var(--bg-hover); }
	.workspace-avatar { display: inline-flex; align-items: center; justify-content: center; width: 22px; height: 22px; border-radius: var(--radius-sm); background: var(--accent-purple); color: var(--bg-primary); font-size: .75rem; font-weight: 700; flex-shrink: 0; }
	.option-name { flex: 1; } small { color: var(--text-muted); font-size: .7rem; } .check { color: var(--accent-blue); }
	.needs-count { color: var(--accent-amber); } .switcher-actions { border-top: 1px solid var(--border); margin-top: var(--space-2); padding-top: var(--space-1); } .switcher-actions span, .switcher-actions small { margin-left: auto; }

	.switcher { position: relative; flex-shrink: 0; }
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
