import { collectionStore } from '$lib/stores/collections.svelte';
import { parseTraits, type Collection } from '$lib/types';

// BUG-3481: the web half of BUG-2702. The Library and Playbooks pages addressed
// the conventions and playbooks collections by their DEFAULT slugs, so a
// workspace that renamed either (a documented onboarding step, TASK-1510)
// showed nothing Active and hid Activate / Create / Import. The server has
// resolved these collections by their `artifact_kind` trait since TASK-2657;
// this does the same in the browser.

export type ArtifactKind = 'convention' | 'playbook';

const DEFAULT_SLUG: Record<ArtifactKind, string> = { convention: 'conventions', playbook: 'playbooks' };

/** The slug of the collection declaring `kind`, or null when none does. */
export function artifactCollectionSlug(collections: readonly Collection[], kind: ArtifactKind): string | null {
	for (const c of collections) {
		if (parseTraits(c).artifact_kind?.kind === kind) return c.slug;
	}
	return null;
}

/**
 * The slug to address `kind`'s collection by in workspace `ws`: the one that
 * declares it, once the collection store holds this workspace; until then,
 * and when nothing declares it, the default slug. A page that keys its load
 * on this re-fetches once when a renamed collection resolves, and not at all
 * when the slug is the default.
 */
export function artifactSlugFor(ws: string, kind: ArtifactKind): string {
	if (collectionStore.collectionsWorkspace === ws) {
		return artifactCollectionSlug(collectionStore.collections, kind) ?? DEFAULT_SLUG[kind];
	}
	return DEFAULT_SLUG[kind];
}
