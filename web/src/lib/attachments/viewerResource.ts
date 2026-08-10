/**
 * The viewer's resource-identity rule (PLAN-2392 phase 3a, TASK-2428).
 *
 * `AttachmentSurfaceHost` closes an open surface on a RESOURCE SWITCH and on
 * nothing else. Half of that is the host's own `itemId`; the other half is a
 * generation counter, because the id alone cannot tell a same-item resource
 * change from a same-item RELOAD — and `ItemDetail` reloads constantly (a
 * collection schema edit refetches the item it is already showing).
 *
 * The two counters `ItemDetail` already has are both wrong for this:
 * `loadGeneration` is a non-reactive fence bumped by EVERY `loadData()`, and
 * `itemGen` is bumped by every optimistic item write. Keying a viewer on either
 * tears it down on a refresh that changed nothing the user can see.
 *
 * This module is the rule itself, extracted from the component for one reason:
 * `ItemDetail` is 7,000 lines with no unit harness, so a rule left inline is
 * only ever tested through the host's props — which cannot see whether the
 * generation was computed correctly in the first place.
 */

/** Identity of a loaded item resource, or `''` for "nothing loaded". */
export function viewerResourceKey(
	workspaceSlug: string | null | undefined,
	itemId: string | null | undefined,
	loaded: boolean
): string {
	if (!loaded || !itemId) return '';
	// The workspace is part of the identity, not decoration: a reused pane can
	// navigate ws1→ws2 carrying the same `?item=<ref>` where both workspaces
	// own that ref (IDEA-2135). A plain `::` separator, deliberately: the first
	// draft used a NUL, which makes the whole source file binary to grep.
	return `${workspaceSlug ?? ''}::${itemId}`;
}

/**
 * The next generation for an observed key. Returns the CURRENT generation
 * unchanged when nothing advanced, so the caller's write is a no-op.
 *
 * An EMPTY key never advances it. Empty is "no loaded resource right now" —
 * what the switch boundary reads mid-load — so A→''→B is ONE transition and
 * counts once, at B. (Going empty still closes an open viewer: that is the
 * host's `itemId` arm, which fires immediately rather than waiting for a
 * replacement that may never arrive.)
 */
export function nextViewerResourceGen(key: string, lastKey: string, gen: number): number {
	if (!key || key === lastKey) return gen;
	return gen + 1;
}
