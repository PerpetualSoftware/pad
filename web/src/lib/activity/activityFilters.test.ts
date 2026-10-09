import { describe, it, expect } from 'vitest';
import { ACTOR_OPTIONS, activityFilterParams, hasActivityFilters, actorBadgeTitle } from './activityFilters';

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
