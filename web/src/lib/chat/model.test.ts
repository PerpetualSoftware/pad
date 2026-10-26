import { describe, it, expect } from 'vitest';
import { ChatWindow, mergeBySeq, unreadDividerIndex } from './model';
import type { ChatMessage } from '$lib/types';

function msg(seq: number, over: Partial<ChatMessage> = {}): ChatMessage {
	return {
		id: `m${seq}`,
		conversation_id: 'c1',
		seq,
		author_agent_key: 'bot',
		author_is_human: false,
		mine: false,
		source: 'cli',
		body: `body ${seq}`,
		created_at: '2026-01-01T00:00:00Z',
		deleted: false,
		...over
	};
}

describe('mergeBySeq', () => {
	it('orders by seq, not created_at (reversed and equal timestamps)', () => {
		const a = msg(1, { created_at: '2026-01-03T00:00:00Z' });
		const b = msg(2, { created_at: '2026-01-02T00:00:00Z' });
		const c = msg(3, { created_at: '2026-01-02T00:00:00Z' });
		expect(mergeBySeq([c, a], [b]).map((m) => m.seq)).toEqual([1, 2, 3]);
	});

	it('dedupes an overlapping page by seq', () => {
		const out = mergeBySeq([msg(1), msg(2), msg(3)], [msg(2), msg(3), msg(4)]);
		expect(out.map((m) => m.seq)).toEqual([1, 2, 3, 4]);
	});

	it('lets a tombstone replace the body in place', () => {
		const out = mergeBySeq([msg(1), msg(2), msg(3)], [msg(2, { body: '', deleted: true })]);
		expect(out.map((m) => m.seq)).toEqual([1, 2, 3]);
		expect(out[1].deleted).toBe(true);
		expect(out[1].body).toBe('');
	});

	it('does not mutate its inputs', () => {
		const w = [msg(2)];
		mergeBySeq(w, [msg(1)]);
		expect(w.map((m) => m.seq)).toEqual([2]);
	});
});

describe('unreadDividerIndex', () => {
	const ws = [msg(1), msg(2), msg(3), msg(4)];

	it('points at the first message above read_seq', () => {
		expect(unreadDividerIndex(ws, 2)).toBe(2);
		expect(unreadDividerIndex(ws, 0)).toBe(0);
	});

	it('is -1 when everything is read', () => {
		expect(unreadDividerIndex(ws, 4)).toBe(-1);
	});

	it('skips my own messages', () => {
		const list = [msg(1), msg(2, { mine: true }), msg(3, { mine: true }), msg(4)];
		expect(unreadDividerIndex(list, 1)).toBe(3);
	});

	it('skips tombstones', () => {
		const list = [msg(1), msg(2, { deleted: true, body: '' }), msg(3)];
		expect(unreadDividerIndex(list, 1)).toBe(2);
	});

	it('is -1 when only own or deleted messages are above read_seq', () => {
		const list = [msg(1), msg(2, { mine: true }), msg(3, { deleted: true, body: '' })];
		expect(unreadDividerIndex(list, 1)).toBe(-1);
	});
});

describe('ChatWindow optimistic sends', () => {
	it('shows a pending bubble after the last confirmed seq', () => {
		const w = new ChatWindow();
		w.merge([msg(1), msg(2)]);
		w.addPending('k1', 'hello');
		w.merge([msg(3)]);
		const kinds = w.items().map((i) => (i.kind === 'message' ? i.message.seq : 'p'));
		expect(kinds).toEqual([1, 2, 3, 'p']);
	});

	it('resolve replaces the bubble with the returned message', () => {
		const w = new ChatWindow();
		w.merge([msg(1)]);
		w.addPending('k1', 'hello');
		w.resolvePending('k1', msg(2, { mine: true, body: 'hello' }));
		expect(w.pending).toEqual([]);
		expect(w.messages.map((m) => m.seq)).toEqual([1, 2]);
		expect(w.items()).toHaveLength(2);
	});

	it('a replayed answer of an already-merged seq leaves exactly one row', () => {
		const w = new ChatWindow();
		w.addPending('k1', 'hello');
		w.merge([msg(5, { mine: true, body: 'hello' })]); // a refetch got there first
		w.resolvePending('k1', msg(5, { mine: true, body: 'hello' })); // replayed: true
		expect(w.messages.filter((m) => m.seq === 5)).toHaveLength(1);
		expect(w.items()).toHaveLength(1);
	});

	it('fail then retry keeps the same op_key', () => {
		const w = new ChatWindow();
		w.addPending('k1', 'hello', 3);
		expect(w.hasInFlight()).toBe(true);
		w.failPending('k1');
		expect(w.pending[0].status).toBe('failed');
		expect(w.hasInFlight()).toBe(false);
		const again = w.retryPending('k1');
		expect(again?.op_key).toBe('k1');
		expect(again?.reply_to_seq).toBe(3);
		expect(again?.status).toBe('sending');
		expect(w.hasInFlight()).toBe(true);
		expect(w.pending).toHaveLength(1);
	});

	it('hasInFlight is false with no pending and after resolve', () => {
		const w = new ChatWindow();
		expect(w.hasInFlight()).toBe(false);
		w.addPending('k1', 'x');
		w.resolvePending('k1', msg(1, { mine: true }));
		expect(w.hasInFlight()).toBe(false);
	});

	it('discardPending removes a failed bubble', () => {
		const w = new ChatWindow();
		w.addPending('k1', 'x');
		w.failPending('k1');
		w.discardPending('k1');
		expect(w.pending).toEqual([]);
	});
});
