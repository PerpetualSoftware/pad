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

// The collection page's `filteredItems` treats `parent` and the legacy
// `phase` key identically — both resolve against `item.parent_link_id`
// (`phase` is kept for backward compat with pre-rename saved views). The
// mutex helpers below must recognize both aliases, or a legacy view/URL
// carrying `phase` would survive "Unparented only" turning on and produce a
// contradictory (always-empty) filter combination (Codex review round 1).
const PARENT_FILTER_KEYS = ['parent', 'phase'] as const;

/**
 * True when an incoming `activeFilters` patch (as FilterBar's
 * `onFilterChange` hands it to the page) sets a specific-parent filter
 * (`parent` or its legacy `phase` alias). The unparented chip is mutually
 * exclusive with the parent filter (PLAN-2095 DR-3) — the page clears
 * `unparentedFilter` when this is true.
 */
export function filtersSetParent(filters: Record<string, string>): boolean {
	return PARENT_FILTER_KEYS.some((key) => typeof filters[key] === 'string' && filters[key] !== '');
}

/**
 * Drop the specific-parent filter (`parent` and/or the legacy `phase`
 * alias) from an `activeFilters` map. Used when the unparented chip is
 * switched on — mutual exclusivity in the other direction (PLAN-2095 DR-3).
 * Returns the same reference when there was nothing to clear, so callers
 * can skip a state write on a no-op.
 */
export function clearParentFilter(filters: Record<string, string>): Record<string, string> {
	if (!PARENT_FILTER_KEYS.some((key) => key in filters)) return filters;
	const next = { ...filters };
	for (const key of PARENT_FILTER_KEYS) delete next[key];
	return next;
}

/**
 * Resolve the parent/unparented mutex over a full filter state at once —
 * used when a URL or saved view is applied wholesale (as opposed to the
 * incremental FilterBar handlers, which enforce the mutex interactively).
 * A hand-crafted URL or a legacy saved view could carry both a
 * specific-parent filter AND the unparented pseudo-filter simultaneously;
 * unparented wins (same precedence as `handleUnparentedChange`), dropping
 * `parent`/`phase` rather than leaving a contradictory, always-empty
 * combination in place (PLAN-2095 DR-3, Codex review round 1).
 */
export function resolveParentUnparentedMutex(
	filters: Record<string, string>,
	unparented: boolean
): { filters: Record<string, string>; unparented: boolean } {
	if (!unparented) return { filters, unparented };
	return { filters: clearParentFilter(filters), unparented };
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

// The URL round-trip reuses the reserved `$unparented` name (not a plain
// `unparented` param) for the SAME reason the saved-view discriminator does
// (PLAN-2095 DR-5): a grandfathered collection can legally have a real
// schema field literally keyed `unparented` (no `$`) that the collection
// page's generic field-filter loop reads/writes through `activeFilters`
// via the SAME URL searchParams object. A plain `unparented` param name
// would collide with that field's filter value and either clobber it or
// swallow it into the "known params" skip-set on read (Codex review round
// 1). `$` is not a legal field-key character server-side (see
// `internal/store/store.go::isValidFieldKey`), so this name is collision-free.

/** Read the reserved `$unparented` URL search param as a boolean intent. */
export function readUnparentedParam(searchParams: URLSearchParams): boolean {
	return searchParams.get(UNPARENTED_FILTER_FIELD) === 'true';
}

/**
 * Write (or remove) the reserved `$unparented` URL search param. Callers
 * should pass the EFFECTIVE state (`unparentedEffective(...)`), not raw
 * intent, so a restricted caller's URL never even transiently carries the
 * param.
 */
export function writeUnparentedParam(params: URLSearchParams, active: boolean): void {
	if (active) {
		params.set(UNPARENTED_FILTER_FIELD, 'true');
	} else {
		params.delete(UNPARENTED_FILTER_FIELD);
	}
}
