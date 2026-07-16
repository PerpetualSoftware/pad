// The `?item=` query param drives the collection page's split-pane detail
// view (PLAN-2105 Phase 2 / TASK-2111). This module holds the PURE decision
// points the pane's URL machinery depends on, factored out of
// `[collection]/+page.svelte`'s `updateUrlFilters` / `loadUrlFilters` so
// they're unit-testable without mounting the full route (TASK-2116) — the
// same pattern `unparentedFilter.ts` already uses for the `$unparented`
// pseudo-filter.
//
// `buildCollectionUrlParams` is the COMPLETE `updateUrlFilters` param
// builder, not just the pane-preservation sliver — `+page.svelte` calls it
// directly rather than duplicating any of this logic inline, so a spec
// exercising this function is exercising the real production code path: a
// future edit that drops the `?item=` re-emit (or any other param) here
// breaks both the app and these specs together, not just a parallel copy.
//
// Kept framework-agnostic (no `$state`/`page`) — callers pass plain
// `URL`/`URLSearchParams` values and plain data.

import { writeUnparentedParam } from './unparentedFilter';

/** The query-string key the split pane's `openItemRef` derives from. */
export const PANE_ITEM_PARAM = 'item';

/**
 * Params `loadUrlFilters` already understands by name (view mode, search
 * query, tags, and the pane ref) rather than absorbing into
 * `activeFilters` as a schema-field filter. `+page.svelte` folds in the
 * collection-specific `UNPARENTED_FILTER_FIELD` on top of this base list —
 * kept here as the base so this module doesn't need to import that
 * vocabulary.
 */
export const KNOWN_COLLECTION_URL_PARAMS: readonly string[] = ['view', 'q', 'tags', PANE_ITEM_PARAM];

/**
 * Re-emit the currently-open pane ref (if any) onto `params` — the
 * `updateUrlFilters` half of the round-trip. `updateUrlFilters` rebuilds
 * its query from scratch (`new URLSearchParams()`) on every
 * filter/sort/view/tag/search change, so without this the pane would
 * silently close on the next unrelated URL sync. `currentUrl` is the live
 * `page.url` that `openItemRef` itself derives from, so this always
 * reflects the pane that's actually open.
 */
export function preservePaneItemParam(params: URLSearchParams, currentUrl: URL): void {
	const openItem = currentUrl.searchParams.get(PANE_ITEM_PARAM);
	if (openItem) params.set(PANE_ITEM_PARAM, openItem);
}

/** The filter/sort/view/search state `updateUrlFilters` serializes. */
export interface CollectionUrlFilterState {
	/** The page's `ViewMode` ('list' | 'board' | 'table'); 'list' is the
	 *  default and omitted from the query string. */
	viewMode: string;
	activeFilters: Record<string, string>;
	selectedTags: string[];
	/** The EFFECTIVE unparented-filter state (`unparentedEffective(...)`),
	 *  not raw intent — see `writeUnparentedParam`. */
	unparentedApplied: boolean;
	searchQuery: string;
}

/**
 * Build the full query-string params for `updateUrlFilters` — view mode,
 * active field filters, tags, the unparented pseudo-filter, search, and the
 * preserved pane ref, in the same order the route wires them. `currentUrl`
 * is only consulted for the pane ref (via `preservePaneItemParam`); every
 * other value comes from `state`.
 */
export function buildCollectionUrlParams(state: CollectionUrlFilterState, currentUrl: URL): URLSearchParams {
	const params = new URLSearchParams();
	if (state.viewMode !== 'list') params.set('view', state.viewMode);
	for (const [k, v] of Object.entries(state.activeFilters)) {
		params.set(k, v);
	}
	// Tags are comma-joined under a single `tags` param. The tag editor
	// uses comma as its add-delimiter, so tag values never contain
	// commas — round-tripping through a comma-joined string is safe.
	if (state.selectedTags.length > 0) params.set('tags', state.selectedTags.join(','));
	// Write the EFFECTIVE state, not raw intent — a restricted caller's URL
	// never even transiently carries `unparented=true` (PLAN-2095 DR-2).
	writeUnparentedParam(params, state.unparentedApplied);
	if (state.searchQuery) params.set('q', state.searchQuery);
	// Preserve an open split pane across this rebuild (PLAN-2105) — see
	// `preservePaneItemParam` above for why this is necessary.
	preservePaneItemParam(params, currentUrl);
	return params;
}
