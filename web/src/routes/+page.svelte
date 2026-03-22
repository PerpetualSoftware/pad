<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { workspaceStore } from '$lib/stores/workspace.svelte';

	onMount(async () => {
		await workspaceStore.loadAll();
		const ws = workspaceStore.workspaces;
		if (ws.length > 0) {
			// Pick the most recently updated workspace
			const sorted = [...ws].sort((a, b) =>
				new Date(b.updated_at).getTime() - new Date(a.updated_at).getTime()
			);
			goto(`/${sorted[0].slug}`, { replaceState: true });
		}
	});
</script>

<div class="welcome">
	{#if workspaceStore.loading}
		<p>Loading...</p>
	{:else if workspaceStore.workspaces.length === 0}
		<h1>Welcome to Pad</h1>
		<p>Create a workspace to get started.</p>
		<p class="hint">Run <code>pad init</code> in your project directory, or use the workspace switcher in the sidebar.</p>
	{:else}
		<p>Redirecting...</p>
	{/if}
</div>

<style>
	.welcome {
		display: flex;
		flex-direction: column;
		align-items: center;
		justify-content: center;
		height: 100%;
		text-align: center;
		color: var(--text-secondary);
		gap: var(--space-3);
	}
	h1 { font-size: 1.8em; color: var(--text-primary); }
	code {
		background: var(--bg-tertiary);
		padding: 2px 8px;
		border-radius: var(--radius-sm);
		font-family: var(--font-mono);
	}
	.hint { font-size: 0.9em; color: var(--text-muted); }
</style>
