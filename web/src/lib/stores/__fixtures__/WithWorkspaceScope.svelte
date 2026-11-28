<script lang="ts">
	// Mounts `component` under a provided workspace scope, as the home item
	// pane does (TASK-4902 tests).
	import type { Component } from 'svelte';
	import { setWorkspaceScope, type WorkspaceScope } from '../workspaceScope.svelte';

	let { scope, component, props = {} }: { scope: WorkspaceScope; component: Component<any>; props?: Record<string, unknown> } = $props();
	// svelte-ignore state_referenced_locally
	setWorkspaceScope(scope);
	const Inner = $derived(component);
</script>

<Inner {...props} />
