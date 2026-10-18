import { describe, it, expect } from 'vitest';
import { ACTOR_OPTIONS, activityFilterParams, hasActivityFilters, actorBadgeTitle, sinceFromUrl } from './activityFilters';

const none = { action: '', source: '', actor: '', collection: '' };

describe('activity filters (TASK-2219)', () => {
	it('sends only the filters that are set, collection and actor included', () => {
		expect(activityFilterParams(none)).toEqual({});
		expect(activityFilterParams({ action: 'created', source: 'cli', actor: 'agent', collection: 'tasks' })).toEqual({
			action: 'created',
			source: 'cli',
			actor: 'agent',
			collection: 'tasks'
		});
		expect(activityFilterParams({ ...none, collection: 'ideas' })).toEqual({ collection: 'ideas' });
	});

	it('counts any one filter as filtered', () => {
		expect(hasActivityFilters(none)).toBe(false);
		for (const key of ['action', 'source', 'actor', 'collection'] as const) {
			expect(hasActivityFilters({ ...none, [key]: 'x' })).toBe(true);
		}
	});

	it("offers the server's actor values", () => {
		expect(ACTOR_OPTIONS.map((o) => o.value)).toEqual(['', 'user', 'agent']);
	});
});

describe('actorBadgeTitle (audit C117)', () => {
	it('says web or CLI in words, not colour alone', () => {
		expect(actorBadgeTitle('cli', 'cli', false)).toBe('Via CLI');
		expect(actorBadgeTitle('web', 'web', false)).toBe('Via web');
		expect(actorBadgeTitle('cli', 'Dana', true)).toBe('Dana, via CLI');
		expect(actorBadgeTitle('user', 'Dana', true)).toBe('Dana, via web');
	});

	it('names an agent as one', () => {
		expect(actorBadgeTitle('agent', 'agent', false)).toBe('An agent');
		expect(actorBadgeTitle('agent', 'Wren', true)).toBe('Wren (agent)');
	});
});

describe('since (TASK-3615)', () => {
	it('is sent when set and counts as a filter', () => {
		expect(activityFilterParams({ ...none, since: '2026-10-17T18:40:00Z' })).toEqual({ since: '2026-10-17T18:40:00Z' });
		expect(hasActivityFilters({ ...none, since: '2026-10-17T18:40:00Z' })).toBe(true);
		expect(activityFilterParams({ ...none, since: '' })).toEqual({});
	});

	it('sinceFromUrl keeps only the forms the server accepts', () => {
		for (const ok of ['2026-10-17', '2026-10-17T18:40:00Z', '2026-10-17T18:40:00.5Z', '2026-10-17T20:40:00+02:00']) {
			expect(sinceFromUrl(ok)).toBe(ok);
		}
		for (const bad of [null, '', 'yesterday', '2026-10-17T18:40', '2026-10-17 18:40:00Z', '2026-13-45']) {
			expect(sinceFromUrl(bad)).toBe('');
		}
	});
});
