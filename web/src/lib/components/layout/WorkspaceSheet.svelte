<!--
	WorkspaceSheet — the mobile "Workspace" slot surface (PLAN-1694, TASK-1701).

	A docked sheet (DockedSheet, anchored above the nav) for moving around the
	current workspace: a purpose-built workspace switcher card (tap to expand an
	inline list — no reused WorkspaceSwitcher), a Navigate tile grid
	(Dashboard/Insights/Roles/Starred/Tags/Settings), and the Collections list.
-->
<script lang="ts">
	import { page } from '$app/state';
	import { goto, afterNavigate } from '$app/navigation';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { workspaceRestoreTarget } from '$lib/utils/workspace-route';
	import { avatarColor, avatarInitial } from '$lib/utils/avatar';
	import { getPrimaryDestinations, getActiveKey } from '$lib/nav/destinations';
	import DockedSheet from '$lib/components/layout/DockedSheet.svelte';

	let { open, onclose }: { open: boolean; onclose: () => void } = $props();

	let wsSlug = $derived(workspaceStore.current?.slug);
	let wsUsername = $derived(workspaceStore.current?.owner_username ?? '');
	let wsPrefix = $derived(wsUsername && wsSlug ? `/${wsUsername}/${wsSlug}` : '');
	let wsName = $derived(workspaceStore.current?.name ?? 'Workspace');
	let isGuest = $derived(workspaceStore.current?.is_guest ?? false);
	let role = $derived(workspaceStore.currentRole);
	let activeKey = $derived(getActiveKey(page.url.pathname, wsPrefix));

	// Navigate tiles: every static destination except Activity (its own nav
	// slot); Settings dropped for guests.
	let navDestinations = $derived(
		getPrimaryDestinations(wsPrefix)
			.filter((d) => d.key !== 'activity')
			.filter((d) => !(d.guestHidden && isGuest))
	);

	const agentSlugs = ['conventions', 'playbooks'];
	let regularCollections = $derived(
		collectionStore.collections.filter((c) => !agentSlugs.includes(c.slug))
	);
	let agentCollections = $derived(
		collectionStore.collections.filter((c) => agentSlugs.includes(c.slug))
	);

	let switching = $state(false);

	// Close on navigation (the switcher card navigates on select).
	afterNavigate((nav) => {
		if (nav.type !== 'enter' && open) onclose();
	});

	function navigate(href: string) {
		onclose();
		uiStore.onNavigate();
		goto(href);
	}

	function selectWorkspace(ws: { slug: string; owner_username?: string }) {
		const current = workspaceStore.current;
		const isCurrent = !!current && ws.slug === current.slug;
		const target = isCurrent
			? `/${current.owner_username}/${current.slug}`
			: workspaceRestoreTarget(ws);
		onclose();
		goto(target);
	}

	function newWorkspace() {
		onclose();
		uiStore.openCreateWorkspace();
	}
</script>

<DockedSheet {open} {onclose} label="Workspace">
	<div class="ws">
		<!-- Workspace switcher card -->
		<button class="ws-card" type="button" onclick={() => (switching = !switching)}>
			<span class="ws-avatar" style:background={avatarColor(wsName)}>{avatarInitial(wsName)}</span>
			<span class="ws-meta">
				<span class="ws-name">{wsName}</span>
				{#if role}<span class="ws-role">{role}</span>{/if}
			</span>
			<span class="ws-switch">Switch <span class="ws-chev" class:up={switching}>⌄</span></span>
		</button>

		{#if switching}
			<div class="ws-list">
				{#each workspaceStore.workspaces as ws (ws.slug)}
					<button
						class="ws-row"
						class:active={ws.slug === wsSlug}
						type="button"
						onclick={() => selectWorkspace(ws)}
					>
						<span class="ws-row-avatar" style:background={avatarColor(ws.name)}>
							{avatarInitial(ws.name)}
						</span>
						<span class="ws-row-name">{ws.name}</span>
						{#if ws.slug === wsSlug}<span class="ws-row-check" aria-hidden="true">✓</span>{/if}
					</button>
				{/each}
				<button class="ws-row ws-new" type="button" onclick={newWorkspace}>
					<span class="ws-row-avatar plus" aria-hidden="true">＋</span>
					<span class="ws-row-name">New workspace</span>
				</button>
			</div>
		{/if}

		<!-- Navigate -->
		<div class="ws-label">Navigate</div>
		<div class="ws-grid">
			{#each navDestinations as dest (dest.key)}
				<button
					class="tile"
					class:active={activeKey === dest.key}
					type="button"
					onclick={() => navigate(dest.href)}
				>
					<span class="tile-icon" aria-hidden="true">{dest.icon}</span>
					<span class="tile-label">{dest.label}</span>
				</button>
			{/each}
		</div>

		<!-- Collections -->
		{#if regularCollections.length || agentCollections.length}
			<div class="ws-label">Collections</div>
			<div class="ws-collections">
				{#each regularCollections as coll (coll.id)}
					<button
						class="coll"
						class:active={activeKey === `collection:${coll.slug}`}
						type="button"
						onclick={() => navigate(`${wsPrefix}/${coll.slug}`)}
					>
						<span class="coll-icon" aria-hidden="true">{coll.icon}</span>
						<span class="coll-name">{coll.name}</span>
					</button>
				{/each}
				{#each agentCollections as coll (coll.id)}
					<button
						class="coll"
						class:active={activeKey === `collection:${coll.slug}`}
						type="button"
						onclick={() => navigate(`${wsPrefix}/${coll.slug}`)}
					>
						<span class="coll-icon" aria-hidden="true">{coll.icon}</span>
						<span class="coll-name">{coll.name}</span>
					</button>
				{/each}
			</div>
		{/if}
	</div>
</DockedSheet>

<style>
	.ws {
		display: flex;
		flex-direction: column;
		padding: 0 var(--space-4);
	}

	/* Workspace switcher card */
	.ws-card {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		width: 100%;
		padding: var(--space-3);
		background: var(--bg-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		cursor: pointer;
		text-align: left;
	}
	.ws-avatar {
		flex-shrink: 0;
		width: 40px;
		height: 40px;
		border-radius: var(--radius-md, 6px);
		display: flex;
		align-items: center;
		justify-content: center;
		color: #fff;
		font-weight: 700;
		font-size: 1.1em;
	}
	.ws-meta {
		display: flex;
		flex-direction: column;
		gap: 1px;
		min-width: 0;
		flex: 1;
	}
	.ws-name {
		font-weight: 600;
		color: var(--text-primary);
		font-size: 1em;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.ws-role {
		font-size: 0.75em;
		color: var(--text-muted);
		text-transform: capitalize;
	}
	.ws-switch {
		flex-shrink: 0;
		display: flex;
		align-items: center;
		gap: 2px;
		font-size: 0.8em;
		color: var(--text-secondary);
	}
	.ws-chev {
		display: inline-block;
		transition: transform 0.15s ease;
	}
	.ws-chev.up {
		transform: rotate(180deg);
	}

	.ws-list {
		display: flex;
		flex-direction: column;
		margin-top: var(--space-2);
		border: 1px solid var(--border);
		border-radius: var(--radius-md, 6px);
		overflow: hidden;
	}
	.ws-row {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-2) var(--space-3);
		background: none;
		border: none;
		cursor: pointer;
		text-align: left;
		color: var(--text-secondary);
	}
	.ws-row:hover {
		background: var(--bg-hover);
	}
	.ws-row.active {
		color: var(--text-primary);
	}
	.ws-row-avatar {
		flex-shrink: 0;
		width: 26px;
		height: 26px;
		border-radius: var(--radius-sm);
		display: flex;
		align-items: center;
		justify-content: center;
		color: #fff;
		font-weight: 700;
		font-size: 0.8em;
	}
	.ws-row-avatar.plus {
		background: transparent;
		border: 1px dashed var(--border);
		color: var(--text-muted);
	}
	.ws-row-name {
		flex: 1;
		min-width: 0;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		font-size: 0.95em;
	}
	.ws-row-check {
		color: var(--accent-blue);
		font-size: 0.9em;
	}
	.ws-new .ws-row-name {
		color: var(--text-muted);
	}

	.ws-label {
		padding: var(--space-4) var(--space-1) var(--space-2);
		font-size: 0.7em;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
	}

	/* Navigate tile grid */
	.ws-grid {
		display: grid;
		grid-template-columns: repeat(3, 1fr);
		gap: var(--space-2);
	}
	.tile {
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		gap: var(--space-1);
		padding: var(--space-3) var(--space-1);
		background: var(--bg-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius-md, 6px);
		cursor: pointer;
		color: var(--text-secondary);
	}
	.tile:hover {
		background: var(--bg-hover);
	}
	.tile.active {
		border-color: var(--accent-blue);
		color: var(--text-primary);
		background: color-mix(in srgb, var(--accent-blue) 12%, var(--bg-primary));
	}
	.tile-icon {
		font-size: 1.3em;
		line-height: 1;
	}
	.tile-label {
		font-size: 0.78em;
		font-weight: 500;
	}

	/* Collections */
	.ws-collections {
		display: flex;
		flex-direction: column;
		gap: 2px;
	}
	.coll {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		width: 100%;
		padding: var(--space-2) var(--space-3);
		background: none;
		border: none;
		border-radius: var(--radius-sm);
		cursor: pointer;
		text-align: left;
		color: var(--text-secondary);
		font-size: 0.95em;
	}
	.coll:hover {
		background: var(--bg-hover);
	}
	.coll.active {
		background: var(--bg-hover);
		color: var(--text-primary);
		font-weight: 600;
	}
	.coll-icon {
		font-size: 1.05em;
		width: 1.4em;
		text-align: center;
		flex-shrink: 0;
	}
</style>
