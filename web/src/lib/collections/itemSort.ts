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
			// Deterministic tiebreak on equal sort_order (created_at ASC —
			// oldest first — then item_number, then id) so a stray updated_at bump (e.g. a
			// spurious collab-flush PATCH, BUG-1941) can't reorder cards that
			// were never actually dragged. Legacy un-dragged cards in a lane
			// share sort_order = 0, so without a tiebreak the comparator was a
			// no-op there and silently inherited whatever order the input array
			// arrived in (updated_at DESC from the API).
			//
			// The direction is the SERVER's (BUG-3527, lead ruling 2026-10-09):
			// child lists and the dashboard order `sort_order ASC, created_at
			// ASC, item_number ASC` (internal/store/items.go getChildItems,
			// ListChildItems and GetChildItemsForParents) and render that order
			// as delivered; this comparator is the same order, one everywhere.
			// created_at has one-second resolution, so siblings created in the
			// same second (decompose does) tie on it; item_number, the
			// workspace sequence, is monotonic in creation order and breaks
			// that tie (BUG-4005). id is only the last resort for an item
			// without a number. The direction used to be DESC here, so the same two never-dragged children read in
			// opposite orders on an item's page and on its collection board.
			// BUG-4005 (Dave's ruling): a PARENTLESS create path writes the
			// MIN(sort_order) - 1 of the collection's parentless items, so it
			// sorts FIRST in a lane; a child is created at 0 and still appends
			// under its parent (created_at ASC, which ship and decompose rely
			// on). The created_at ASC tie-break therefore orders children and
			// never-dragged legacy rows; dragging places a card anywhere.
			return (a, b) =>
				a.sort_order - b.sort_order ||
				timeValue(a.created_at) - timeValue(b.created_at) ||
				(a.item_number ?? 0) - (b.item_number ?? 0) ||
				a.id.localeCompare(b.id);
	}
}
