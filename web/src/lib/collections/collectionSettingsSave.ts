// What the collection editor writes back as `settings` (PLAN-3535).
//
// Settings are stored WHOLESALE: a PATCH replaces the whole blob. The editor
// used to rebuild it from its own form fields alone, which silently dropped
// every key it did not render (content_template, and any key added later).
// It now starts from the STORED settings and overlays only what it edits.

import { storedSettings, type Collection, type QuickAction } from '$lib/types';

export interface CollectionSettingsForm {
	defaultView: string;
	layout: string;
	boardGroupBy: string;
	listGroupBy: string;
	listSortBy: string;
	quickActions: QuickAction[];
	tracksWork: boolean;
}

export function buildCollectionSettings(
	collection: Pick<Collection, 'settings'>,
	form: CollectionSettingsForm
): Record<string, unknown> {
	const out: Record<string, unknown> = {
		...storedSettings(collection),
		default_view: form.defaultView,
		layout: form.layout,
		board_group_by: form.boardGroupBy || undefined,
		list_group_by: form.listGroupBy || undefined,
		list_sort_by: form.listSortBy || undefined,
		tracks_work: form.tracksWork
	};
	if (form.quickActions.length > 0) out.quick_actions = form.quickActions;
	else delete out.quick_actions;
	return out;
}
