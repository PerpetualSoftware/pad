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

	it('removes quick actions the form cleared, and keeps the ones it has', () => {
		const stored = { settings: JSON.stringify({ quick_actions: [{ label: 'a', prompt: 'p', scope: 'item' }] }) };
		expect('quick_actions' in buildCollectionSettings(stored, form())).toBe(false);
		const qa = [{ label: 'b', prompt: 'q', scope: 'item' as const }];
		expect(buildCollectionSettings(stored, form({ quickActions: qa })).quick_actions).toEqual(qa);
	});

	it('survives unparseable stored settings', () => {
		expect(buildCollectionSettings({ settings: 'not json' }, form()).layout).toBe('balanced');
	});
});
