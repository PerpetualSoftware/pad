// What an activity change pill needs to render a relation side as the target
// rather than the item ID it stores (BUG-2872): which keys are relations, a
// resolver with the relation family's narrowings, and whether the workspace
// index has loaded (a miss means "unresolved" only once it has).
//
// Built by the page that owns the item/collection context (ItemDetail, the
// workspace activity page) and handed DOWN to the timeline components, which
// import no stores themselves: the collection store registers an identity
// listener at module scope, so importing it into the leaves pulls a real store
// into every timeline test that mocks auth.
import type { FieldDef, ItemIndexRow } from '$lib/types';
import { narrowRelationRow } from '$lib/collections/relationGroups';
import { localIndex } from '$lib/stores/localIndex.svelte';
import { collectionStore } from '$lib/stores/collections.svelte';

export interface ChangeContext {
	/** The changed key's field definition, when the collection declares it. */
	fieldFor(key: string): FieldDef | undefined;
	/** Resolve a relation value: id-only, scoped to the declared target. */
	resolveRow(id: string, declaredCollection: string | undefined): ItemIndexRow | null;
	/** True once the workspace index has loaded. */
	indexReady(): boolean;
}

export function createChangeContext(
	wsSlug: () => string,
	fieldFor: (key: string) => FieldDef | undefined,
): ChangeContext {
	return {
		fieldFor,
		resolveRow(id, declaredCollection) {
			const ws = wsSlug();
			if (!ws) return null;
			const known = new Set(collectionStore.collections.map((c) => c.slug));
			return narrowRelationRow(localIndex.findByIdOrSlug(ws, id), id, declaredCollection, known);
		},
		indexReady() {
			const ws = wsSlug();
			return !!ws && localIndex.bootstrapStateFor(ws) === 'ready';
		},
	};
}
