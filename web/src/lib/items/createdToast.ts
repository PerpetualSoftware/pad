// The toast for items another actor created (TASK-3593). One item names it
// and links to it; a burst counts them and links to their collection, or to
// the workspace when they landed in several. The burst is folded into a
// bounded summary as it arrives, never kept as a list of every item.

export interface CreatedEntry {
	title: string;
	collection?: string;
	link?: string;
}

export interface CreatedBurst {
	count: number;
	first: CreatedEntry;
	/** The one collection every item landed in, or null once they differ. */
	collection: string | null;
}

export function foldCreated(acc: CreatedBurst | undefined, entry: CreatedEntry): CreatedBurst {
	if (!acc) return { count: 1, first: entry, collection: entry.collection ?? null };
	return {
		count: acc.count + 1,
		first: acc.first,
		collection: acc.collection !== null && entry.collection === acc.collection ? acc.collection : null,
	};
}

export function createdToast(
	who: string,
	burst: CreatedBurst,
	wsPrefix: string,
): { message: string; link?: string } {
	if (burst.count === 1) {
		return { message: `${who} created: ${burst.first.title}`, link: burst.first.link };
	}
	return {
		message: `${who} created ${burst.count} items`,
		link: burst.collection ? `${wsPrefix}/${burst.collection}` : wsPrefix,
	};
}
