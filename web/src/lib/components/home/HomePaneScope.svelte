<script lang="ts">
	// Provides one item workspace's scope to the home item pane below it
	// (TASK-4902). The host keys this component on the minted owner/workspace,
	// so the props never change for an instance: a workspace change remounts it
	// with a fresh scope, and the old one is disposed with it.
	import { onDestroy, type Snippet } from 'svelte';
	import { createItemWorkspaceScope, setWorkspaceScope } from '$lib/stores/workspaceScope.svelte';

	let { owner, workspace, children }: { owner: string; workspace: string; children: Snippet } = $props();

	// svelte-ignore state_referenced_locally
	const scope = createItemWorkspaceScope(owner, workspace);
	setWorkspaceScope(scope);
	onDestroy(() => scope.dispose());
</script>

{@render children()}
