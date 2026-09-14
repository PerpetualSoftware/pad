/**
 * ONE ANSWER TO "may this surface print this field as a categorical value?"
 * (BUG-3067), for the surfaces that render `status` / `priority` BY NAME.
 *
 * The defect this closes is a class, not a site: a `status` or `priority` field
 * retyped to a `relation` stores a STRING — an item id — so any surface that
 * reads the value by name and prints it shows a raw uuid where a word belongs.
 * BUG-3016 fixed every site that already had a `FieldDef` in hand; what was left
 * were surfaces that read by name with no schema, and they were enumerated three
 * times before the population held still (see the BUG-3067 trail).
 *
 * It lives in ONE module because that is the lesson of the last enumeration
 * round rather than a style preference: five call sites asking the same question
 * in five spellings is how the question comes to have five answers, which is the
 * arrangement `laneKeyCallers.test.ts` exists to prevent for lane keys.
 *
 * The core is PURE — it takes the collections rather than reading a store — so
 * it is testable without mocking a store, and so a caller that already holds a
 * collection can skip the lookup entirely.
 */
import type { Collection, FieldDef, Item } from '$lib/types';
import { parseSchema } from '$lib/types';
import { categoricalChipValue } from '$lib/components/share/shareView';

/**
 * The declared field, or undefined when the collection or field is unknown.
 *
 * A SLUG IS ONLY UNIQUE WITHIN A WORKSPACE, and `collectionStore.collections` is
 * a single global slot that deliberately retains the PREVIOUS workspace's array
 * while the next load is in flight (the store says so itself, from BUG-1461).
 * Two workspaces can hold a `tasks` whose `priority` is a select in one and a
 * relation in the other — so a slug-only lookup during that window can answer
 * with the wrong workspace's type, in either direction: printing an id, or
 * withholding a perfectly good value.
 *
 * `notStale` asks the NARROWER question, and the difference is a regression I
 * shipped and the review round caught. The first version took the store's
 * `collectionsAreFreshFor(wsSlug)`, which is false in TWO different situations:
 * the collections belong to another workspace, and the store has not been
 * stamped for any workspace yet. Only the first is a reason to withhold.
 *
 * The second is routine — `playbooks` and the dashboard both fetch collections
 * into PAGE-LOCAL state and never stamp the shared store, so the stamp is set by
 * the workspace layout and not by them. Treating unstamped as stale therefore
 * blanked every status pill on those pages until the layout's load landed, and
 * permanently if it failed while their own fetch succeeded. Withholding a value
 * for a load nobody is waiting on is not fail-closed, it is just wrong.
 *
 * So: withhold only when the store says it holds ANOTHER workspace's
 * collections. Unstamped falls through to the lookup, which answers undefined
 * for an empty array anyway — the unloaded case was already covered by the data,
 * never by this flag.
 */
export function fieldDefFor(
	collections: Collection[],
	collectionSlug: string | undefined,
	key: string,
	notStale = true,
): FieldDef | undefined {
	if (!notStale) return undefined;
	if (!collectionSlug) return undefined;
	const coll = collections.find((c) => c.slug === collectionSlug);
	if (!coll) return undefined;
	return parseSchema(coll).fields.find((f) => f.key === key);
}

/**
 * The categorical value for a field named on a COLLECTION SLUG rather than on an
 * item — for a surface holding a server projection (the graph node) instead of a
 * full `Item`. Same question, same module: `graph/DetailCard` reached for
 * `categoricalChipValue` directly before this existed, which made the claim
 * "every surface routes through one helper" false by one call.
 */
export function categoricalValueForSlug(
	collections: Collection[],
	collectionSlug: string | undefined,
	key: string,
	raw: unknown,
	notStale = true,
): string {
	return categoricalChipValue(fieldDefFor(collections, collectionSlug, key, notStale), raw);
}

/**
 * The value to PRINT for a categorical field, or `''` when there is none.
 *
 * Empty covers four different situations on purpose, because every one of them
 * has the same honest rendering — nothing:
 *
 *  - the field is a relation type, so the stored string is an id (the defect);
 *  - the stored value is not a string (a list-typed field, a number);
 *  - the field is not declared;
 *  - the collections belong to a DIFFERENT workspace (see `fieldDefFor`);
 *  - THE COLLECTION IS NOT LOADED YET, which is the one that is not an error.
 *
 * That last case makes this FAIL CLOSED while `collectionStore` is still
 * filling: a row renders without its chip for a moment rather than printing a
 * value it cannot vouch for. Withholding a chip is recoverable and showing a raw
 * id is the bug, so the asymmetry is deliberate — but it does mean a caller must
 * not read `''` as "this item has no status".
 */
export function categoricalValueFor(
	collections: Collection[],
	item: Pick<Item, 'collection_slug'> & { fields?: unknown },
	key: string,
	raw: unknown,
	notStale = true,
): string {
	return categoricalValueForSlug(collections, item.collection_slug, key, raw, notStale);
}

/**
 * "The loaded collections are NOT known to belong to a different workspace."
 *
 * The one spelling of the staleness question, so the six call sites cannot
 * drift into asking two different ones — which is exactly what happened between
 * this and `collectionsAreFreshFor`, whose extra `null` case blanked two pages.
 *
 * `stampedWorkspace` is `collectionStore.collectionsWorkspace`: the slug the
 * current array was loaded for, or null when no load has completed.
 */
export function collectionsNotStaleFor(
	stampedWorkspace: string | null,
	wsSlug: string | undefined,
): boolean {
	if (stampedWorkspace === null) return true;
	if (!wsSlug) return true;
	return stampedWorkspace === wsSlug;
}
