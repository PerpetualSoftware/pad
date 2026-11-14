import { describe, expect, it } from 'vitest';
import type { AgendaEntry, AgendaWorkEntry } from '$lib/types';
import { classify, defaultAudience, audienceChip, filterAudience, audienceCounts, parseAudience, splitAudience, type AudienceView } from './audience';

const entry = (overrides: Partial<AgendaEntry> = {}): AgendaEntry => ({
	type: 'schedule', at: '2031-03-10T09:00:00Z', day: '2031-03-10',
	item_ref: 'TASK-1', item_title: 'A reminder', collection: 'tasks', state: 'scheduled', ...overrides
});
const views: AudienceView[] = ['mine', 'agents', 'all'];

describe('agenda audience', () => {
	it.each([
		['human', 'mine'], ['agent', 'agents'], ['both', 'mine'], [undefined, 'mine']
	] as const)('classifies schedule audience %s as %s', (audience, expected) => {
		expect(classify(entry({ audience }))).toBe(expected);
	});
	it.each(['date', 'created', 'completed'] as const)('classifies %s as mine even with agent metadata', type => {
		expect(classify(entry({ type, audience: 'agent' }))).toBe('mine');
	});
	const mine = [entry({ audience: 'human' }), entry({ audience: 'both' }), entry(), entry({ type: 'date' }), entry({ type: 'created' }), entry({ type: 'completed' })];
	const agent = entry({ audience: 'agent' });
	it.each(views)('filters %s without assignee restrictions', view => {
		expect(filterAudience([...mine, agent], view)).toEqual(view === 'mine' ? mine : view === 'agents' ? [agent] : [...mine, agent]);
	});
	it('counts only non-completed dated entries in the response, with disjoint totals', () => {
		const counts = audienceCounts([...mine, agent,
			entry({ state: 'done' }), entry({ audience: 'agent', state: 'done' }),
			entry({ type: 'date', state: 'done' }), entry({ state: 'missed' }), entry({ state: 'acknowledged' })]);
		expect(counts).toEqual({ mine: 6, agents: 1, all: 7 });
		expect(counts.all).toBe(counts.mine + counts.agents);
		expect(audienceCounts([])).toEqual({ mine: 0, agents: 0, all: 0 });
	});
	it.each(views)('keeps all attention in %s, including entries outside the window', view => {
		const humanMiss = entry({ state: 'missed', schedule_id: 'human' });
		const agentMiss = entry({ audience: 'agent', state: 'missed', schedule_id: 'agent' });
		const outside: AgendaWorkEntry = { ...entry({ schedule_id: 'outside' }), reason: 'deadline_still_owed', type: 'schedule' };
		const split = splitAudience([humanMiss, agentMiss, agent], [outside], view);
		expect(split.attention.map(item => item.entry)).toEqual([outside, humanMiss, agentMiss]);
		expect(split.entries).toEqual(filterAudience([humanMiss, agentMiss, agent], view));
	});
	it.each([
		['mine', 'mine', false, null], ['agents', 'mine', false, 'agents'],
		['mine', 'agents', false, 'you'], ['agents', 'agents', false, null],
		['mine', 'all', false, null], ['agents', 'all', false, 'agents'],
		['mine', 'mine', true, null], ['agents', 'mine', true, 'agents'],
		['mine', 'agents', true, 'you'], ['agents', 'agents', true, null],
		['mine', 'all', true, 'you'], ['agents', 'all', true, null]
	] as const)('chip for %s in %s (agent=%s) is %s', (kind, view, isAgent, expected) => {
		expect(audienceChip(kind, view, isAgent)).toBe(expected);
	});
	it('defaults to the session audience', () => {
		expect(defaultAudience(false)).toBe('mine');
		expect(defaultAudience(true)).toBe('agents');
	});
	it.each(views)('parses valid URL value %s for either session', view => {
		expect(parseAudience(view, false)).toBe(view);
		expect(parseAudience(view, true)).toBe(view);
	});
	it.each([null, undefined, '', 'agent', 'Mine', 'invalid'])('defaults invalid or missing URL value %s', value => {
		expect(parseAudience(value, false)).toBe('mine');
		expect(parseAudience(value, true)).toBe('agents');
	});
});
