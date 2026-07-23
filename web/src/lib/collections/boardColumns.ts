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

/** Lane key for items with no (or an unrecognised) group value. */
export const UNCATEGORIZED = '';

/**
 * Normalise a raw field value to the string lane key it groups under.
 * Non-strings coerce (a lone `null`/`undefined` → ''); arrays/objects
 * stringify and simply won't match a known option, landing in UNCATEGORIZED.
 */
function laneValue(raw: unknown): string {
	if (typeof raw === 'string') return raw;
	if (raw == null) return '';
	return String(raw);
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
 */
export function bucketByColumn(
	items: Item[],
	groupField: string,
	columns: string[]
): Record<string, Item[]> {
	const known = new Set(columns);
	const result: Record<string, Item[]> = { [UNCATEGORIZED]: [] };
	for (const col of columns) {
		result[col] = [];
	}
	for (const item of items) {
		const value = laneValue(parseFields(item)[groupField]);
		if (value && known.has(value)) {
			result[value].push(item);
		} else {
			result[UNCATEGORIZED].push(item);
		}
	}
	return result;
}
