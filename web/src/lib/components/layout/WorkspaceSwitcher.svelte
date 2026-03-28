<script lang="ts">
	import { goto } from '$app/navigation';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { uiStore } from '$lib/stores/ui.svelte';

	let open = $state(false);

	function select(slug: string) {
		open = false;
		goto(`/${slug}`);
	}

	function openCreateModal() {
		open = false;
		uiStore.openCreateWorkspace();
	}
</script>

<div class="switcher">
	<button class="current" onclick={() => open = !open}>
		<span class="name">{workspaceStore.current?.name ?? 'Select workspace'}</span>
		<span class="chevron">{open ? '▲' : '▼'}</span>
	</button>

	{#if open}
		<!-- svelte-ignore a11y_click_events_have_key_events -->
		<!-- svelte-ignore a11y_no_static_element_interactions -->
		<div class="backdrop" onclick={() => open = false}></div>
		<div class="dropdown">
			{#each workspaceStore.workspaces as ws}
				<button
					class="item"
					class:active={ws.slug === workspaceStore.current?.slug}
					onclick={() => select(ws.slug)}
				>
					{ws.name}
				</button>
			{/each}
			<button class="item create-trigger" onclick={openCreateModal}>
				+ New Workspace
			</button>
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
	}
	.item:hover { background: var(--bg-hover); }
	.item.active { background: var(--bg-active); color: var(--accent-blue); }
	.create-trigger { color: var(--text-muted); border-top: 1px solid var(--border); }
</style>
