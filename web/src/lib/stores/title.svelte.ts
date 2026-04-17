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

const SEP = ' \u00B7 '; // " · " — U+00B7 middle dot, with surrounding spaces
const APP_NAME = 'Pad';

let workspace = $state<string | undefined>(undefined);
let section = $state<string | undefined>(undefined);
let item = $state<string | undefined>(undefined);

const title = $derived.by(() => {
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
	/** Current section label (for debugging/inspection). */
	get section() { return section; },
	/** Current item ref (for debugging/inspection). */
	get item() { return item; },

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
			section = parts.section ?? undefined;
		}
		if ('item' in parts) {
			item = parts.item ?? undefined;
		}
	},

	/** Reset all parts; the resulting title is bare `Pad`. */
	clearPageTitle() {
		workspace = undefined;
		section = undefined;
		item = undefined;
	},
};
