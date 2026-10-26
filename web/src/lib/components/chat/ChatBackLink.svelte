<script lang="ts">
	import { lastWorkspace } from '$lib/stores/lastWorkspace.svelte';
	import { workspaceStore } from '$lib/stores/workspace.svelte';
	import { workspaceRestoreTarget } from '$lib/utils/workspace-route';

	let last = $derived(lastWorkspace.current);
	let href = $derived(last ? workspaceRestoreTarget(last) : '/console');
	let name = $derived(
		last ? (workspaceStore.workspaces.find((w) => w.slug === last.slug)?.name ?? last.slug) : ''
	);
</script>

<a class="back-link" {href}>{last ? `Back to ${name}` : 'Back'}</a>

<style>
	.back-link {
		color: var(--text-secondary);
		font-size: 0.85rem;
		text-decoration: none;
	}
	.back-link:hover {
		color: var(--text-primary);
	}
</style>
