// Board-column bucketing (IDEA-2275).
//
// The kanban board groups items into lanes by a select field's value. Its
// original bucketing seeded a lane for every known schema option and pushed
// each item into the lane matching its value — but SILENTLY DROPPED any item
// whose value was empty, missing, or no longer a valid option. Those items
// then had no home on the board and could only be found in other views.
//
// This helper collects every such item into a single UNCATEGORIZED ('') lane
// instead, so nothing is invisible. The '' key matches the convention used by
// ListView and the public share board, and is exactly what a drop into the
// lane writes back to the field (clearing it).
import type { Item } from '$lib/types';
import { parseFields } from '$lib/types';
import { safeText } from '$lib/fields/fieldShape';

/** Lane key for items with no (or an unrecognised) group value. */
export const UNCATEGORIZED = '';

/**
 * Normalise a raw field value to the string lane key it groups under.
 *
 * ABSENT is `undefined`, `null` and `''`, and nothing else. `0` and `false`
 * normalise to `'0'` and `'false'` — ordinary lane keys — because they are
 * ordinary VALUES: a score of zero, an unticked checkbox.
 *
 * Arrays and objects stringify (`'a,b'`, `'[object Object]'`). They are NOT
 * guaranteed to miss every declared option, which an earlier version of this
 * comment claimed: a board option whose text equals the stringification would
 * match it, and a view that DISCOVERS its lanes mints a lane named after it
 * either way. What is guaranteed is that they stay visible, which is this
 * helper's job; rendering a structured value legibly is not.
 *
 * EXPORTED, and the export is the point (BUG-3053). ListView had its own
 * inlined version of this question and got a different answer in each of its
 * two passes — it decided "has a group" by FALSINESS while bucketing under the
 * stringified value, so an item scoring 0 was filed under `'0'` and no lane
 * pointed there. It vanished. One predicate, read by every view that groups,
 * is what stops the two halves drifting apart again.
 */
export function laneValue(raw: unknown): string {
	// `safeText`, not `String` (BUG-3052): a stored `{"toString":0}` made
	// `String(raw)` throw here, and every grouped view reads its lanes through
	// this function, so one hostile value took the whole list or board down.
	// Identical to `String` for every value it could already convert.
	return safeText(raw);
}

/**
 * Does this item have no value for the group field?
 *
 * The emptiness test the falsiness test was standing in for. Takes a RAW value
 * or an already-normalised lane key and normalises either way, so the answer
 * cannot depend on whether the caller stringified first — which is precisely
 * what went wrong: one pass asked the question of the raw `0` and the other of
 * the string `'0'`, and they disagreed.
 *
 * Accepting `unknown` rather than `string` is the load-bearing part. With a
 * `string` parameter, `!value` and `value === ''` are the same function and the
 * strict form is decoration; with a raw value reaching it they are opposites,
 * and this is the door a raw value arrives at.
 */
export function isUngrouped(value: unknown): boolean {
	return laneValue(value) === UNCATEGORIZED;
}

/**
 * Bucket `items` into board lanes keyed by their `groupField` value.
 *
 * Every known column in `columns` gets a bucket (so empty real lanes still
 * render), plus an always-present UNCATEGORIZED ('') bucket. Any item whose
 * value is empty, missing, or not one of `columns` lands in UNCATEGORIZED
 * rather than being dropped. The returned map always contains the '' key
 * (possibly empty) — callers decide whether to render the lane based on its
 * length, so it only appears "if needed".
 *
 * A `Map`, not a plain object (BUG-3208). The keys are schema option VALUES,
 * and a plain object keyed by them does not create a lane for `__proto__`:
 * `result['__proto__'] = []` RE-PARENTS the object onto that array, the lane
 * is missing from `Object.keys`, and a caller that puts the result in `$state`
 * loses proxying for the WHOLE board, because Svelte proxies only objects whose
 * prototype is Object.prototype. A caller that needs a `$state` record keys it
 * by `laneKey(value)` instead.
 */
export function bucketByColumn(
	items: Item[],
	groupField: string,
	columns: string[],
	/**
	 * How to read an item's lane value, when the field's own value is not it.
	 * A RELATION field needs this (TASK-2998): values that resolve to nothing
	 * are folded onto a sentinel first, so "no value", "target deleted" and
	 * "points at nothing" do not all collapse into UNCATEGORIZED. Defaults to
	 * the field's own value, so every existing caller is unchanged.
	 */
	valueFor?: (item: Item) => string
): Map<string, Item[]> {
	const result = new Map<string, Item[]>([[UNCATEGORIZED, []]]);
	for (const col of columns) {
		result.set(col, []);
	}
	for (const item of items) {
		const value = valueFor ? valueFor(item) : laneValue(parseFields(item)[groupField]);
		const lane = value ? result.get(value) : undefined;
		(lane ?? result.get(UNCATEGORIZED)!).push(item);
	}
	return result;
}

/**
 * The key a lane's value is stored under in a plain-object lane record.
 *
 * A prefix no Object.prototype member starts with, so every value is an OWN
 * key: `__proto__` creates a lane instead of re-parenting the record, and
 * `constructor` or `toString` read as absent instead of finding the inherited
 * function (BUG-3208, the board's copy of BUG-3054). Not `Object.create(null)`:
 * these records are `$state`, and Svelte proxies only objects whose prototype
 * is Object.prototype, so a null-prototype record would silently lose its
 * reactivity.
 */
export function laneKey(value: string): string {
	return `lane:${value}`;
}

/**
 * The heading a lane shows for its key.
 *
 * ONE copy, read by both grouped views (BUG-3053). ListView and BoardView each
 * carried a byte-identical private version, and each contained its own
 * falsiness test for "no value" — the same conflation this bug is about, one
 * step further downstream, where it would have labelled a `0` lane
 * "Uncategorized" the moment a caller handed it an unnormalised value.
 *
 * Normalises its input for the same reason `isUngrouped` does, so a raw `0`
 * arriving here is titled `0` rather than throwing on `.replace` or being
 * labelled Uncategorized.
 */
export function formatLaneLabel(value: unknown): string {
	const laneKey = laneValue(value);
	if (isUngrouped(laneKey)) return 'Uncategorized';
	return laneKey.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
}
