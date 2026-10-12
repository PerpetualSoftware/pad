// The one-line summary of a collection's active filters (TASK-3592). With
// the filter panel closed, a filtered view used to look like the whole
// collection: the only sign was a dot on the filter button. The page shows
// this summary, an "N of M" count and a Clear button instead.
import type { ItemIndexRow } from '$lib/types';
import { narrowRelationRow, relationChipFor } from './relationGroups';

export interface FilterSummaryInput {
	/** Field key → filter value, as the page's `activeFilters`. */
	filters: Record<string, string>;
	tags: string[];
	search: string;
	/** "Unparented only", as APPLIED (not raw intent). */
	unparented: boolean;
	/** The name to show for a field key: its schema label, or the key. */
	fieldLabel: (key: string) => string;
	/** The text to show for a value: an option as-is, an item id as its title. */
	valueLabel: (key: string, value: string) => string;
}

/** One part per active filter, in the order the panel shows them. */
export function filterSummaryParts(input: FilterSummaryInput): string[] {
	const parts: string[] = [];
	for (const [key, value] of Object.entries(input.filters)) {
		parts.push(`${input.fieldLabel(key).toLowerCase()}: ${input.valueLabel(key, value)}`);
	}
	if (input.tags.length > 0) parts.push(`tags: ${input.tags.join(', ')}`);
	if (input.unparented) parts.push('unparented only');
	const q = input.search.trim();
	if (q) parts.push(`search: “${q}”`);
	return parts;
}

/** "12 of 84" when filtered; the plain total otherwise. */
export function filteredCountLabel(shown: number, total: number, filtered: boolean): string {
	return filtered ? `${shown} of ${total}` : String(total);
}

/**
 * How the summary names a relation filter's value (TASK-3599, retro of
 * #2072). Through the filter chip's own resolver, by id only and within the
 * field's target collection: a workspace-wide id-or-slug lookup would name a
 * legacy slug or a same-id row from another collection as the value. The
 * label is the chip's, so the summary and the chip never disagree.
 */
export function relationSummaryLabel(
	value: string,
	lookup: (id: string) => ItemIndexRow | null | undefined,
	declaredCollection: string | undefined,
	knownCollectionSlugs: ReadonlySet<string> | null,
): string {
	const chip = relationChipFor(value, (id) => narrowRelationRow(lookup(id), id, declaredCollection, knownCollectionSlugs));
	return chip ? (chip.title ?? chip.label) : value;
}
