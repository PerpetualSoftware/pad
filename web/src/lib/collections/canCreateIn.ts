import { collectionStore } from '$lib/stores/collections.svelte';
import { workspaceStore } from '$lib/stores/workspace.svelte';

/**
 * Whether the caller may create items in the collection a write addresses by
 * `slug` (BUG-3264). The library and the Conventions and Playbooks pages
 * POST to the literal `conventions` / `playbooks` slugs, so this resolves the
 * same collection the write does, then asks the grant-aware
 * `canEditCollection` (the predicate BUG-3258 applied to every item-create
 * door). A slug naming no loaded collection answers false.
 *
 * Reads two reactive stores, so a `$derived` that calls it re-runs when
 * either changes.
 */
export function canCreateIn(slug: string): boolean {
	const coll = collectionStore.collections.find((c) => c.slug === slug);
	return !!coll && workspaceStore.canEditCollection(coll.id);
}
