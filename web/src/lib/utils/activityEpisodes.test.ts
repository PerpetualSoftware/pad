import { describe, it, expect } from 'vitest';
import { foldEpisodes } from './activityEpisodes';
import type { Activity } from '$lib/types';

// Fixed clock: all tests measure liveness against this instant.
const NOW = new Date('2026-08-24T12:00:00Z').getTime();
const now = () => NOW;

let seq = 0;
function act(minutesAgo: number, over: Partial<Activity> = {}): Activity {
	seq += 1;
	return {
		id: `a${seq}`,
		workspace_id: 'ws',
		action: 'updated',
		actor: 'agent',
		source: 'cli',
		metadata: '{"agent":"claude-code"}',
		created_at: new Date(NOW - minutesAgo * 60_000).toISOString(),
		document_id: 'item-1',
		item_ref: 'BUG-1',
		item_title: 'One',
		item_slug: 'one',
		collection_slug: 'bugs',
		...over
	};
}

describe('foldEpisodes', () => {
	it('folds consecutive same-actor same-item events into one episode', () => {
		const eps = foldEpisodes([act(1), act(5), act(9)], { now });
		expect(eps).toHaveLength(1);
		expect(eps[0].count).toBe(3);
		expect(eps[0].latest.created_at > eps[0].first.created_at).toBe(true);
		expect(eps[0].spanMs).toBe(8 * 60_000);
	});

	it('splits a run when the gap exceeds gapMinutes', () => {
		const eps = foldEpisodes([act(1), act(5), act(50), act(52)], { now, gapMinutes: 30 });
		expect(eps).toHaveLength(2);
		expect(eps[0].count).toBe(2);
		expect(eps[1].count).toBe(2);
	});

	it('does NOT split on an interleaved sibling: runs are per (actor,item), not per feed position', () => {
		// Two items being worked simultaneously — rows interleave in the feed.
		const eps = foldEpisodes(
			[
				act(1),
				act(2, { document_id: 'item-2', item_ref: 'BUG-2', item_slug: 'two' }),
				act(3),
				act(4, { document_id: 'item-2', item_ref: 'BUG-2', item_slug: 'two' })
			],
			{ now }
		);
		expect(eps).toHaveLength(2);
		expect(eps.map((e) => e.count)).toEqual([2, 2]);
	});

	it('separates actors on the same item', () => {
		const eps = foldEpisodes([act(1), act(2, { actor: 'user', actor_name: 'Dave', source: 'web' })], {
			now
		});
		expect(eps).toHaveLength(2);
		expect(eps.map((e) => e.actorKind).sort()).toEqual(['agent', 'user']);
	});

	it('claims live only from event age, and only within liveMinutes', () => {
		const eps = foldEpisodes([act(3), act(200, { document_id: 'item-2', item_ref: 'BUG-2' })], {
			now,
			liveMinutes: 10
		});
		expect(eps[0].live).toBe(true);
		expect(eps[1].live).toBe(false);
	});

	it('keeps the first non-empty enrichment when the newest row lacks it', () => {
		const eps = foldEpisodes([act(1, { item_title: undefined, item_ref: undefined }), act(2)], {
			now
		});
		expect(eps).toHaveLength(1);
		expect(eps[0].itemRef).toBe('BUG-1');
		expect(eps[0].itemTitle).toBe('One');
	});

	it('never shows a generic client id as a seat name', () => {
		const eps = foldEpisodes([act(1, { metadata: '{"agent":"claude-code"}' })], { now });
		expect(eps[0].actorLabel).toBe('agent');
	});

	it('surfaces a stamped seat name from metadata and separates seats by it', () => {
		const eps = foldEpisodes(
			[
				act(1, { metadata: '{"agent":"wren"}' }),
				act(2, { metadata: '{"agent":"rook"}' })
			],
			{ now }
		);
		expect(eps).toHaveLength(2);
		expect(eps.map((e) => e.actorLabel).sort()).toEqual(['rook', 'wren']);
	});

	it('tolerates unparseable metadata and workspace-level rows', () => {
		const eps = foldEpisodes(
			[act(1, { metadata: 'not json', document_id: undefined, item_ref: undefined, item_slug: undefined })],
			{ now }
		);
		expect(eps).toHaveLength(1);
		expect(eps[0].actorLabel).toBe('agent');
	});

	it('dedupes actions newest-first', () => {
		const eps = foldEpisodes([act(1), act(2, { action: 'created' }), act(3)], { now });
		expect(eps[0].actions).toEqual(['updated', 'created']);
	});
});
