// The collection page's parent filter, for any collection (TASK-2215, audit
// C35).
//
// It used to exist only on a collection slugged exactly `tasks`, listing the
// `plans` collection: bugs under plans, renamed collections and the hiring
// template's hierarchies had no parent filter, although every card showed its
// parent and `?parent=` already filtered any collection. The options now come
// from the parents the collection's own items have: each local-index row
// carries `parent_link_id` (the parent item's id, which is what the filter
// compares) with the parent's ref and title. A collection none of whose items
// has a parent shows no filter, as before.

export interface ParentRow {
	parent_link_id?: string;
	parent_ref?: string;
	parent_title?: string;
	parent_collection_slug?: string;
}

export interface ParentFilterOptions {
	/** parent item id → its label, ordered by label. */
	labels: Record<string, string>;
	/** What to call the options: the parents' collection when they share one, else "parents". */
	noun: string;
}

export function parentFilterOptions(
	rows: ParentRow[],
	collectionName: (slug: string) => string | undefined
): ParentFilterOptions {
	const byId = new Map<string, string>();
	const parentCollections = new Set<string>();
	for (const r of rows) {
		const id = r.parent_link_id;
		if (!id || byId.has(id)) continue;
		const title = r.parent_title?.trim() || id;
		byId.set(id, r.parent_ref ? `${r.parent_ref}: ${title}` : title);
		parentCollections.add(r.parent_collection_slug ?? '');
	}
	const sorted = [...byId.entries()].sort((a, b) => a[1].localeCompare(b[1], undefined, { numeric: true }));
	const labels: Record<string, string> = {};
	for (const [id, label] of sorted) labels[id] = label;

	let noun = 'parents';
	if (parentCollections.size === 1) {
		const [only] = parentCollections;
		const name = only ? collectionName(only) : undefined;
		if (name) noun = name.toLowerCase();
	}
	return { labels, noun };
}
