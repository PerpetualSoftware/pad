// The `?item=` query param drives the collection page's split-pane detail
// view (PLAN-2105 Phase 2 / TASK-2111). This module holds the two PURE
// decision points the pane's URL machinery depends on, factored out of
// `[collection]/+page.svelte`'s `updateUrlFilters` / `loadUrlFilters` so
// they're unit-testable without mounting the full route (TASK-2116) — the
// same pattern `unparentedFilter.ts` already uses for the `$unparented`
// pseudo-filter.
//
// Kept framework-agnostic (no `$state`/`page`) — callers pass plain
// `URL`/`URLSearchParams` values.

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
