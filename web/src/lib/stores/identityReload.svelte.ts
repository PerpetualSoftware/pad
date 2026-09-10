import { browser } from '$app/environment';
import { clearAttachmentMetadataCache } from '$lib/components/editor/attachment-metadata';
import { CURSOR_STORAGE_PREFIX } from '$lib/collab/wsProvider.svelte';

/**
 * What a real identity change does to this tab (BUG-3005, lead ruling after
 * codex round 3): clear the persistent state a reload would NOT drop, then
 * reload.
 *
 * WHY A RELOAD RATHER THAN MORE FENCES. Three review rounds each found another
 * layer of surfaces holding the previous user's data — three stores, then
 * fourteen route pages plus a connection and two caches, then the console
 * subtree, share pages, the command palette and an open dialog. The population
 * is "everything in the tab that ever held a fetch", and a per-surface fix
 * cannot terminate: it is a list somebody has to keep complete forever. A
 * reload drops every store, component, cache and in-flight request at once and
 * nothing new has to be remembered.
 *
 * WHAT THE RELOAD DOES NOT DROP is exactly this function's other half.
 * `localStorage`, `sessionStorage` and IndexedDB survive it, so those stay an
 * enumeration — but a BOUNDED one, over storage keys rather than over every
 * module in the app, and one that a grep for the storage APIs can check.
 *
 * The store-level fences stay too, and are not redundant with this: they cover
 * the window between the identity changing and the page actually going away,
 * during which in-flight responses are still settling into live stores.
 */
/** Must equal `RECENT_SEARCHES_KEY` in CommandPalette.svelte. Pinned by a test. */
export const RECENT_SEARCHES_KEY = 'pad-recent-searches';
/** Built by `workspace-route.ts`; the prefix is duplicated, pinned by a test. */
export const LAST_ROUTE_PREFIX = 'pad-last-route-';
/** Built inline by every page that uses `createScrollRestoration`. */
export const LAST_SCROLL_PREFIX = 'pad-last-scroll-';

export function clearPersistentIdentityState(): void {
	if (!browser) return;

	// The editor's attachment-metadata memo. In RAM, so the reload would get it
	// anyway — cleared here so that this function is the one place the answer
	// to "what does an identity change drop" lives.
	clearAttachmentMetadataCache();

	// The command palette's recent searches. Search terms carry private item
	// names and project context, and the key has no user in it. The literal is
	// duplicated from CommandPalette.svelte, which cannot export it — a guard
	// test pins the two spellings together.
	try {
		localStorage.removeItem(RECENT_SEARCHES_KEY);
	} catch {
		// A browser with storage disabled throws on access. Nothing to clear.
	}

	// Route and scroll memory. `pad-last-route-<ws>` holds the last URL visited
	// in a workspace — which can be a private item slug — and
	// `pad-last-scroll-<ws>-<pathname>` embeds the path in its own KEY. Both are
	// keyed by workspace with no user in them, so two accounts with access to
	// the same workspace slug share them, and the second one is legible from
	// the key list alone without reading a single value (codex round 4).
	try {
		for (const key of Object.keys(localStorage)) {
			if (key.startsWith(LAST_ROUTE_PREFIX) || key.startsWith(LAST_SCROLL_PREFIX)) {
				localStorage.removeItem(key);
			}
		}
	} catch {
		// As above.
	}

	// Collaborative-editing cursors, keyed by item id ALONE. B opening an item
	// A edited in this tab would otherwise inherit A's op-log cursor and
	// reconcile against a document history that is not theirs.
	try {
		for (const key of Object.keys(sessionStorage)) {
			if (key.startsWith(CURSOR_STORAGE_PREFIX)) sessionStorage.removeItem(key);
		}
	} catch {
		// As above.
	}
}

/**
 * Clear what survives a reload, then reload.
 *
 * Separated from `clearPersistentIdentityState` so a test can drive the clears
 * without navigating, and so the reload itself is one mockable call rather
 * than a bare `location.reload()` buried in a layout.
 */
export function reloadForIdentityChange(): void {
	clearPersistentIdentityState();
	if (!browser) return;
	location.reload();
}
