// ItemDetail, loaded on demand (TASK-2226).
//
// ItemDetail pulls in the whole rich-text stack (Tiptap, ProseMirror, Yjs):
// about 1 MB of the collection route's JavaScript, though a list needs it only
// once a pane opens. PaneHost imports it through here instead of statically,
// so the route's first paint no longer downloads and parses the editor, and
// the pages that host a pane warm it in idle time so an open rarely waits.

import type { Component } from 'svelte';

type ItemDetailModule = typeof import('./ItemDetail.svelte');

let pending: Promise<ItemDetailModule> | null = null;

/**
 * The ItemDetail module, imported once. A failed import (offline, a chunk
 * replaced by a deploy) is not cached, so the next call tries again.
 */
export function loadItemDetail(): Promise<ItemDetailModule> {
	pending ??= import('./ItemDetail.svelte').catch((err: unknown) => {
		pending = null;
		throw err;
	});
	return pending;
}

/** The component itself, for callers that render it. */
export async function loadItemDetailComponent(): Promise<Component<Record<string, unknown>>> {
	return (await loadItemDetail()).default as unknown as Component<Record<string, unknown>>;
}

/**
 * Warm the import when the browser is idle, so the first pane open finds it
 * loaded. Failures are left for the open itself to report.
 */
export function prefetchItemDetail(): void {
	if (typeof window === 'undefined') return;
	const go = () => void loadItemDetail().catch(() => {});
	const idle = (window as Window & { requestIdleCallback?: (cb: () => void, opts?: { timeout: number }) => number })
		.requestIdleCallback;
	if (idle) idle(go, { timeout: 3000 });
	else setTimeout(go, 1500);
}
