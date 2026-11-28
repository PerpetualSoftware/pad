<script lang="ts">
	// Stands in for ItemDetail in HomePaneHost.svelte.test.ts: renders the
	// props the pane hands it and the workspace scope it sits under.
	import { workspaceScope } from '$lib/stores/workspaceScope.svelte';
	let { ref, username, wsSlug, collSlug }: { ref: string; username: string; wsSlug: string; collSlug: string } = $props();
	const scope = workspaceScope();
	const mounts = ((globalThis as { __detailMounts?: number }).__detailMounts ?? 0) + 1;
	(globalThis as { __detailMounts?: number }).__detailMounts = mounts;
</script>

<p
	data-testid="item-detail-stub"
	data-ref={ref}
	data-username={username}
	data-ws={wsSlug}
	data-coll={collSlug}
	data-scope-slug={scope.slug}
	data-scope-owner={String(scope.isOwner)}
	data-mount={mounts}
>{ref}</p>
