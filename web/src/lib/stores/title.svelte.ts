/**
 * Title store — central source of truth for the browser-tab title.
 *
 * The root layout's `<svelte:head><title>` reads `titleStore.title`
 * reactively, and route-level pages call `titleStore.setPageTitle(...)`
 * (wired up in TASK-603) to contribute their context.
 *
 * Format (most-specific-first, so tab truncation keeps the useful bits):
 *   - item set:      `{item} · {workspace} · Pad`
 *   - section set:   `{section} · {workspace} · Pad`
 *   - workspace set: `{workspace} · Pad`
 *   - otherwise:     `Pad`
 * Workspace is skipped from the composed string if it is unset.
 *
 * Tracked by IDEA-592 / PLAN-601 / TASK-602.
 */

import { page } from '$app/state';

const SEP = ' \u00B7 '; // " · " — U+00B7 middle dot, with surrounding spaces
const APP_NAME = 'Pad';

let workspace = $state<string | undefined>(undefined);
let sectionValue = $state<string | undefined>(undefined);
let itemValue = $state<string | undefined>(undefined);

// The pathname each part was set FOR (TASK-2245). `section` and `item` belong
// to a route; `workspace` does not, and is deliberately unstamped.
//
// WHY A STAMP RATHER THAN A CLEAR. The workspace layout used to clear these on
// route change, and that clear was a race it could lose three different ways:
//   - it ran on SEARCH-only changes (opening/closing the item pane, switching
//     view), because reading `page.url.pathname` tracks the whole reactive
//     `page.url` — so it wiped a section the leaf had just set;
//   - as an `$effect` it could run AFTER the leaf's effect, since the
//     parent-before-child guarantee is about mount order, not re-runs;
//   - moved to `beforeNavigate` to fix that, it then fired for navigations that
//     were subsequently CANCELLED — the collection page cancels to prompt about
//     an unsaved draft — leaving the title cleared on a page the user never
//     left (codex round 1).
//
// Stamping removes the clear, and with it all three. A part simply stops
// counting when the route it was set for is no longer the route being shown, so
// nothing has to run at the right moment.
let sectionPath = $state<string | undefined>(undefined);
let itemPath = $state<string | undefined>(undefined);

/** The route the title is being composed FOR. */
function currentPath(): string | undefined {
	// `page` is safe to read during SSR and in tests; guard anyway so a
	// non-router consumer of this store still composes a sensible title.
	try {
		return page.url?.pathname;
	} catch {
		return undefined;
	}
}

const title = $derived.by(() => {
	// A part counts only while the route it was set for is the route on screen.
	const here = currentPath();
	const item = itemPath === here ? itemValue : undefined;
	const section = sectionPath === here ? sectionValue : undefined;
	if (item) {
		return workspace
			? `${item}${SEP}${workspace}${SEP}${APP_NAME}`
			: `${item}${SEP}${APP_NAME}`;
	}
	if (section) {
		return workspace
			? `${section}${SEP}${workspace}${SEP}${APP_NAME}`
			: `${section}${SEP}${APP_NAME}`;
	}
	if (workspace) {
		return `${workspace}${SEP}${APP_NAME}`;
	}
	return APP_NAME;
});

export interface PageTitleParts {
	workspace?: string | null;
	section?: string | null;
	item?: string | null;
}

export const titleStore = {
	/** Composed browser-tab title, reactive. */
	get title() { return title; },

	/** Current workspace display name (for debugging/inspection). */
	get workspace() { return workspace; },
	/** Current section label (for debugging/inspection), route-scoped. */
	get section() { return sectionPath === currentPath() ? sectionValue : undefined; },
	/** Current item ref (for debugging/inspection), route-scoped. */
	get item() { return itemPath === currentPath() ? itemValue : undefined; },

	/**
	 * Merge the provided keys into title state.
	 *
	 * Semantics per key:
	 *  - omitted (key not present on `parts`): leave existing value unchanged
	 *  - `null`: explicitly clear that key back to `undefined`
	 *  - a string: set that key
	 *
	 * This lets a route set only what it knows (e.g. `{ section: 'Ideas' }`)
	 * without clobbering the workspace that the layout set earlier.
	 */
	setPageTitle(parts: PageTitleParts) {
		if ('workspace' in parts) {
			workspace = parts.workspace ?? undefined;
		}
		if ('section' in parts) {
			sectionValue = parts.section ?? undefined;
			sectionPath = currentPath();
		}
		if ('item' in parts) {
			itemValue = parts.item ?? undefined;
			itemPath = currentPath();
		}
	},

	/** Reset all parts; the resulting title is bare `Pad`. */
	clearPageTitle() {
		workspace = undefined;
		sectionValue = undefined;
		itemValue = undefined;
		sectionPath = undefined;
		itemPath = undefined;
	},
};
