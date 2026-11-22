import { describe, expect, it } from 'vitest';
import { buildCollectionSettings, type CollectionSettingsForm } from './collectionSettingsSave';

// PLAN-3535: the editor's settings write keeps the keys it does not edit.

const form = (over: Partial<CollectionSettingsForm> = {}): CollectionSettingsForm => ({
	defaultView: 'list',
	layout: 'balanced',
	boardGroupBy: 'status',
	listGroupBy: '',
	listSortBy: '',
	quickActions: [],
	tracksWork: true,
	awaitsHuman: false,
	...over
});

describe('buildCollectionSettings (PLAN-3535)', () => {
	it('keeps keys the form does not render (content_template, unknown future keys)', () => {
		const out = buildCollectionSettings(
			{ settings: JSON.stringify({ content_template: '# T', some_future_key: 7, default_view: 'board' }) },
			form()
		);
		expect(out.content_template).toBe('# T');
		expect(out.some_future_key).toBe(7);
		expect(out.default_view).toBe('list'); // the form's edit wins
	});

	it('writes the toggle, false included', () => {
		expect(buildCollectionSettings({ settings: '{}' }, form({ tracksWork: false })).tracks_work).toBe(false);
		expect(buildCollectionSettings({ settings: '{"tracks_work":false}' }, form({ tracksWork: true })).tracks_work).toBe(true);
	});

	it('writes awaits_human, false included, so a stored true can be turned off (IDEA-3906)', () => {
		expect(buildCollectionSettings({ settings: '{}' }, form({ awaitsHuman: true })).awaits_human).toBe(true);
		const off = buildCollectionSettings({ settings: '{"awaits_human":true}' }, form({ awaitsHuman: false }));
		expect('awaits_human' in off).toBe(true);
		expect(off.awaits_human).toBe(false);
	});

	it('clears quick actions the form emptied with null, and keeps the ones it has (TASK-4732)', () => {
		const stored = { settings: JSON.stringify({ quick_actions: [{ label: 'a', prompt: 'p', scope: 'item' }] }) };
		const cleared = buildCollectionSettings(stored, form());
		expect('quick_actions' in cleared).toBe(true);
		expect(cleared.quick_actions).toBeNull();
		expect(JSON.parse(JSON.stringify(cleared)).quick_actions).toBeNull();
		const qa = [{ label: 'b', prompt: 'q', scope: 'item' as const }];
		expect(buildCollectionSettings(stored, form({ quickActions: qa })).quick_actions).toEqual(qa);
	});

	it('sends a cleared group-by or sort as null so the merge deletes it (TASK-4732)', () => {
		const stored = { settings: JSON.stringify({ board_group_by: 'priority', list_group_by: 'status', list_sort_by: 'title' }) };
		const out = buildCollectionSettings(stored, form({ boardGroupBy: '', listGroupBy: '', listSortBy: '' }));
		const wire = JSON.parse(JSON.stringify(out));
		for (const key of ['board_group_by', 'list_group_by', 'list_sort_by']) {
			expect(key in wire).toBe(true);
			expect(wire[key]).toBeNull();
		}
		const set = buildCollectionSettings(stored, form({ boardGroupBy: 'status', listGroupBy: 'priority', listSortBy: 'created' }));
		expect([set.board_group_by, set.list_group_by, set.list_sort_by]).toEqual(['status', 'priority', 'created']);
	});

	it('survives unparseable stored settings', () => {
		expect(buildCollectionSettings({ settings: 'not json' }, form()).layout).toBe('balanced');
	});
});
