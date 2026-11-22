// What the collection editor writes back as `settings` (PLAN-3535).
//
// A PATCH's settings are MERGED onto the stored settings (TASK-4732): a key
// it does not mention is kept, and a key sent as null is deleted. So a field
// the form clears is written as null; omitting it would leave the stored
// value in place. The editor still starts from the STORED settings and
// overlays only what it edits, so the blob it sends is the whole intended
// state.

import { storedSettings, type Collection, type QuickAction } from '$lib/types';

export interface CollectionSettingsForm {
	defaultView: string;
	layout: string;
	boardGroupBy: string;
	listGroupBy: string;
	listSortBy: string;
	quickActions: QuickAction[];
	tracksWork: boolean;
	awaitsHuman: boolean;
}

export function buildCollectionSettings(
	collection: Pick<Collection, 'settings'>,
	form: CollectionSettingsForm
): Record<string, unknown> {
	const out: Record<string, unknown> = {
		...storedSettings(collection),
		default_view: form.defaultView,
		layout: form.layout,
		// null clears: the server keeps a key the write does not mention.
		board_group_by: form.boardGroupBy || null,
		list_group_by: form.listGroupBy || null,
		list_sort_by: form.listSortBy || null,
		// The server refuses null for these two, so they are always a boolean,
		// false included.
		tracks_work: form.tracksWork,
		awaits_human: form.awaitsHuman,
		quick_actions: form.quickActions.length > 0 ? form.quickActions : null
	};
	return out;
}
