<!--
	WorkspaceSheet — the mobile "Workspace" slot surface (PLAN-1694, TASK-1701).

	A docked sheet (DockedSheet, anchored above the nav) for moving around the
	current workspace: a Navigate tile grid (Dashboard/Insights/Roles/Starred/
	Tags/Settings) and the Collections list.

	The workspace *switcher* used to live here as a card; IDEA-1835 moved it to
	the persistent top bar (MobileContextBar), so this sheet is now navigation
	only. The bottom-nav slot keeps its "Workspace" label and avatar.
-->
<script lang="ts">
	import { page } from '$app/state';
	import { goto, afterNavigate } from '$app/navigation';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';
	import { getPrimaryDestinations, getActiveKey } from '$lib/nav/destinations';
	import DockedSheet from '$lib/components/layout/DockedSheet.svelte';

	let { open, onclose }: { open: boolean; onclose: () => void } = $props();

	let wsSlug = $derived(workspaceStore.current?.slug);
	let wsUsername = $derived(workspaceStore.current?.owner_username ?? '');
	let wsPrefix = $derived(wsUsername && wsSlug ? `/${wsUsername}/${wsSlug}` : '');
	let isGuest = $derived(workspaceStore.current?.is_guest ?? false);
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

	// Close on navigation (the tiles/collections navigate on select).
	afterNavigate((nav) => {
		if (nav.type !== 'enter' && open) onclose();
	});

	function navigate(href: string) {
		onclose();
		uiStore.onNavigate();
		goto(href);
	}
</script>

<DockedSheet {open} {onclose} label="Workspace">
	<div class="ws">
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
