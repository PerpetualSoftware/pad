/**
 * Filter assembly for Settings → Storage (PLAN-2392 DR-18).
 *
 * Extracted from StorageTab's `buildFilters()` so the one rule that is easy to
 * get wrong — `item_id` and the attached/unattached selector are mutually
 * exclusive — is pinned by a test instead of living only inside a component
 * that no unit test mounts. The server treats `item_id` + `item=unattached` as
 * a contradiction and returns nothing (see handlers_storage.go), so sending
 * both would silently empty the list.
 */
import type { AttachmentListFilters } from '$lib/types';

export interface StorageFilterSelections {
	category?: string;
	/** '' | 'attached' | 'unattached' — the workspace-wide selector. */
	item?: string;
	/** UUID of a single parent item. Wins over `item` when set. */
	itemId?: string;
	collection?: string;
	sort?: string;
	limit: number;
	offset: number;
}

export function buildStorageFilters(sel: StorageFilterSelections): AttachmentListFilters {
	const f: AttachmentListFilters = {
		limit: sel.limit,
		offset: sel.offset,
	};
	if (sel.sort) f.sort = sel.sort as AttachmentListFilters['sort'];
	if (sel.category) f.category = sel.category as AttachmentListFilters['category'];
	// Item scope wins: it is the narrower, explicitly-requested filter, and the
	// two cannot be combined.
	if (sel.itemId) f.item_id = sel.itemId;
	else if (sel.item) f.item = sel.item as AttachmentListFilters['item'];
	if (sel.collection) f.collection = sel.collection;
	return f;
}

/** True when anything narrows the list — drives the empty-state wording. */
export function hasActiveStorageFilters(sel: StorageFilterSelections): boolean {
	return Boolean(sel.category || sel.itemId || sel.item || sel.collection);
}
