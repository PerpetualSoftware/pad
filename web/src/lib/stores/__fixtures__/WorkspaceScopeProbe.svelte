<script lang="ts">
	// Renders what `workspaceScope()` answers for the component tree it sits
	// in (workspaceScope.svelte.test.ts).
	import { workspaceScope } from '../workspaceScope.svelte';
	const scope = workspaceScope();
</script>

<output
	data-testid="scope"
	data-slug={scope.slug}
	data-username={scope.username}
	data-owner={String(scope.isOwner)}
	data-role={scope.currentMembership?.role ?? ''}
	data-edit-item={String(scope.canEditItem({ id: 'i1', collection_id: 'c1' }))}
	data-edit-collection={String(scope.canEditCollection('c1'))}
	data-collections={scope.collections.map((c) => c.slug).join(',')}
	data-fresh={String(scope.collectionsAreFreshFor('pad'))}
></output>
