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

	// Verdicts pasted from Go 1.26 (time.Parse "2006-01-02" then
	// time.RFC3339, the handler's order), not derived from this code. Every
	// "refused" row here is one Date.parse accepts or normalises, or a range
	// edge (TASK-3615 PR B r1).
	it.each([
		['2026-02-30', false],
		['2026-02-29', false],
		['2026-04-31', false],
		['1900-02-29', false],
		['2024-02-29', true],
		['2000-02-29', true],
		['0000-02-29', true],
		['2026-02-28T24:00:00Z', false],
		['2026-02-28T23:59:60Z', false],
		['2026-02-28T12:00:00+24:00', true],
		['2026-02-28T12:00:00+24:60', true],
		['2026-02-28T12:00:00-24:60', true],
		['2026-02-28T12:00:00+05:60', true],
		['2026-02-28T12:00:00+25:00', false],
		['2026-02-28T12:00:00+24:61', false],
		['2026-02-28T12:00:00+00:99', false],
		['0000-01-01T00:00:00Z', true],
		['9999-12-31T23:59:59Z', true],
		['2026-02-28T12:00:00.123456789123Z', true],
		// Go parses these; the handler refuses a UTC year past 9999 (PR B r5).
		['9999-12-31T23:59:59-01:00', false],
		['9999-12-30T23:30:00-24:60', false],
		['9999-12-31T22:59:59-01:00', true],
		['9999-12-31T23:59:59+01:00', true]
	])('sinceFromUrl(%s) matches Go: accepted=%s', (raw, accepted) => {
		expect(sinceFromUrl(raw)).toBe(accepted ? raw : '');
	});
});
