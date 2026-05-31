// Page-wide item sorting (TASK-1670 / IDEA-1648).
//
// A single comparator factory shared by BoardView (within-lane) and
// ListView (within-group) so a sort chosen on the collection toolbar
// applies consistently in both. `manual` preserves the stored
// `sort_order` (the drag-to-reorder order) and is the default.
import type { Item, Collection } from '$lib/types';
import { parseFields, parseSchema } from '$lib/types';

export type SortMode = 'manual' | 'priority' | 'updated' | 'created' | 'title';

export const SORT_OPTIONS: { value: SortMode; label: string }[] = [
	{ value: 'manual', label: 'Manual' },
	{ value: 'priority', label: 'Priority' },
	{ value: 'updated', label: 'Recently updated' },
	{ value: 'created', label: 'Created' },
	{ value: 'title', label: 'A → Z' }
];

// The select field a "Priority" sort ranks by. Convention is the field
// keyed `priority` (e.g. tasks high/medium/low, conventions
// must/should/nice-to-have). Returns undefined when the collection has
// no such field, so the toolbar can hide the Priority option.
export function priorityField(collection: Collection) {
	const schema = parseSchema(collection);
	return schema.fields.find((f) => f.key === 'priority' && f.type === 'select');
}

// Priority weight = the value's index in the field's `options` array.
// Options are authored top-to-bottom (high…low, must…nice), so a lower
// index is a higher priority. Items missing the field, or carrying a
// value not in the schema, sort last. Reading the field's own option
// order means different priority vocabularies rank naturally without a
// hardcoded weight map.
function priorityWeight(item: Item, options: string[]): number {
	const val = parseFields(item).priority;
	const idx = typeof val === 'string' ? options.indexOf(val) : -1;
	return idx === -1 ? Number.MAX_SAFE_INTEGER : idx;
}

function timeValue(s: string | undefined): number {
	if (!s) return 0;
	const t = Date.parse(s);
	return Number.isNaN(t) ? 0 : t;
}

// Build the within-group comparator for `mode`. `priority` falls back to
// `sort_order` as a stable tie-break; the date modes sort newest-first;
// `title` is case-insensitive A→Z. `manual` (default) is the stored
// `sort_order`, preserving drag ordering.
export function itemComparator(
	mode: SortMode,
	collection: Collection
): (a: Item, b: Item) => number {
	switch (mode) {
		case 'priority': {
			const options = priorityField(collection)?.options ?? [];
			return (a, b) =>
				priorityWeight(a, options) - priorityWeight(b, options) ||
				a.sort_order - b.sort_order;
		}
		case 'updated':
			return (a, b) => timeValue(b.updated_at) - timeValue(a.updated_at);
		case 'created':
			return (a, b) => timeValue(b.created_at) - timeValue(a.created_at);
		case 'title':
			return (a, b) =>
				(a.title || '').localeCompare(b.title || '', undefined, {
					sensitivity: 'base'
				});
		case 'manual':
		default:
			return (a, b) => a.sort_order - b.sort_order;
	}
}
