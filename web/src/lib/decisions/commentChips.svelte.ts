import type { ConventionChip } from './conventionChips';

// TASK-3119 U2b: per-comment convention chips, published by DecisionChips
// (which already fetches the item's decisions) and read by each comment card,
// so a card needs no fetch of its own. Keyed by item id: a page can show more
// than one item at once (a master list and a pane).
let byItem = $state<Record<string, Record<string, ConventionChip[]>>>({});

export const commentChipsStore = {
	setFor(itemId: string, chips: Map<string, ConventionChip[]>) {
		byItem = { ...byItem, [itemId]: Object.fromEntries(chips) };
	},
	clearFor(itemId: string) {
		if (!(itemId in byItem)) return;
		const next = { ...byItem };
		delete next[itemId];
		byItem = next;
	},
	chipsFor(itemId: string, commentId: string): ConventionChip[] {
		return byItem[itemId]?.[commentId] ?? [];
	}
};
