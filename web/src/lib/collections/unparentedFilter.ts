// The "Unparented only" collection-page filter (TASK-2099 / PLAN-2095
// Phase 2). Phase 1 (TASK-2096) delivered the backend/CLI/MCP contract —
// index/delta rows carry an optional `is_unparented` bit, gated to
// unrestricted callers via `includes_unparented_metadata` (see
// `localIndex.svelte.ts::includesUnparentedMetadataFor`). This module holds
// the PURE decision logic the collection page (+page.svelte) and the public
// share route (`/s/[token]`) wire up around that bit, so the filter/mutex/
// round-trip/skip rules are unit-testable without mounting the full page.
//
// Kept deliberately framework-agnostic (no `$state`/`$derived`) — every
// export here is a plain function over plain data.

/**
 * Reserved saved-view filter field for the "unparented only" pseudo-filter
 * (PLAN-2095 DR-5). Never a real schema field key — server-reserved (schema
 * validation rejects new fields keyed `$unparented`) so a grandfathered
 * schema that happens to carry a literal `unparented` field (no `$`) can
 * never collide with it. Public-share evaluation must always skip
 * conditions on this exact field — see `filterEvaluable` /
 * `matchesFilter` in `$lib/components/share/shareView`.
 */
export const UNPARENTED_FILTER_FIELD = '$unparented';

/**
 * True when an incoming `activeFilters` patch (as FilterBar's
 * `onFilterChange` hands it to the page) sets a specific-parent filter.
 * The unparented chip is mutually exclusive with the parent filter
 * (PLAN-2095 DR-3) — the page clears `unparentedFilter` when this is true.
 */
export function filtersSetParent(filters: Record<string, string>): boolean {
	return typeof filters.parent === 'string' && filters.parent !== '';
}

/**
 * Drop the specific-parent filter from an `activeFilters` map. Used when
 * the unparented chip is switched on — mutual exclusivity in the other
 * direction (PLAN-2095 DR-3). Returns the same reference when there was
 * nothing to clear, so callers can skip a state write on a no-op.
 */
export function clearParentFilter(filters: Record<string, string>): Record<string, string> {
	if (!('parent' in filters)) return filters;
	const next = { ...filters };
	delete next.parent;
	return next;
}

/**
 * Whether the unparented chip's filter should actually apply to the item
 * list. Requires BOTH the user's toggle intent AND confirmed unrestricted
 * metadata availability (PLAN-2095 DR-2) — a restricted caller's intent
 * (carried over from a URL or saved view) never takes effect, closing the
 * side channel DR-2 calls out. `metadataAvailable` should come from
 * `localIndex.includesUnparentedMetadataFor(ws) === true` (strict `true`,
 * not just truthy — `null`/`false` both mean "don't apply").
 */
export function unparentedEffective(intent: boolean, metadataAvailable: boolean): boolean {
	return intent && metadataAvailable;
}

/** One saved-view filter condition, matching `ViewConfig['filters'][number]`. */
export interface ViewFilterCondition {
	field: string;
	op: string;
	value: unknown;
}

/**
 * Build the `$unparented` saved-view condition for `buildViewConfig`, or
 * `null` when it shouldn't be persisted — the chip isn't active, or
 * metadata isn't available to this caller (a restricted caller can't have
 * toggled it, but this guards a stale `true` from ever round-tripping into
 * a saved view). PLAN-2095 DR-5.
 */
export function buildUnparentedViewFilter(
	active: boolean,
	metadataAvailable: boolean
): ViewFilterCondition | null {
	if (!unparentedEffective(active, metadataAvailable)) return null;
	return { field: UNPARENTED_FILTER_FIELD, op: 'eq', value: true };
}

/**
 * True when a saved view's parsed `filters` array carries the reserved
 * `$unparented` discriminator (PLAN-2095 DR-5). Used by `applyViewConfig`
 * to recover the chip's intent when a saved view is selected — the actual
 * on-screen effect still runs through `unparentedEffective`, so a
 * restricted caller applying a view that carries this never filters
 * anything (DR-2).
 */
export function viewHasUnparentedFilter(filters: ViewFilterCondition[] | undefined): boolean {
	if (!filters) return false;
	return filters.some(
		(f) => f.field === UNPARENTED_FILTER_FIELD && f.op === 'eq' && f.value === true
	);
}

/** Read the `unparented` URL search param as a boolean intent. */
export function readUnparentedParam(searchParams: URLSearchParams): boolean {
	return searchParams.get('unparented') === 'true';
}

/**
 * Write (or remove) the `unparented` URL search param. Callers should pass
 * the EFFECTIVE state (`unparentedEffective(...)`), not raw intent, so a
 * restricted caller's URL never even transiently carries the param.
 */
export function writeUnparentedParam(params: URLSearchParams, active: boolean): void {
	if (active) {
		params.set('unparented', 'true');
	} else {
		params.delete('unparented');
	}
}
