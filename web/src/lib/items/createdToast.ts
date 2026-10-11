// The toast for items another actor created (TASK-3593). One item names it
// and links to it; a burst counts them and links to their collection, or to
// the workspace when they landed in several.

export interface CreatedEntry {
	title: string;
	collection?: string;
	link?: string;
}

export function createdToast(
	who: string,
	entries: CreatedEntry[],
	wsPrefix: string,
): { message: string; link?: string } {
	if (entries.length === 1) {
		return { message: `${who} created: ${entries[0].title}`, link: entries[0].link };
	}
	const first = entries[0].collection;
	const oneCollection = !!first && entries.every((e) => e.collection === first);
	return {
		message: `${who} created ${entries.length} items`,
		link: oneCollection ? `${wsPrefix}/${first}` : wsPrefix,
	};
}
