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

/** The declared field, or undefined when the collection or field is unknown. */
export function fieldDefFor(
	collections: Collection[],
	collectionSlug: string | undefined,
	key: string,
): FieldDef | undefined {
	if (!collectionSlug) return undefined;
	const coll = collections.find((c) => c.slug === collectionSlug);
	if (!coll) return undefined;
	return parseSchema(coll).fields.find((f) => f.key === key);
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
): string {
	return categoricalChipValue(fieldDefFor(collections, item.collection_slug, key), raw);
}
