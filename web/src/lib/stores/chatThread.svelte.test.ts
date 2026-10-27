// TASK-4044 PR 3a: the open-thread store. The API is a small fake of the real
// server's list semantics (store/conversations.go ListConversationMessages: ASC
// for after_seq, DESC-then-reversed otherwise, limit default 50 and max 200,
// has_more = more rows exist in the direction of travel), so a paging bug in the
// store is visible here rather than only in a browser.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import type { ChatMessage } from '$lib/types';

const h = vi.hoisted(() => ({
	getConversation: vi.fn(),
	listMessages: vi.fn(),
	markRead: vi.fn(),
	unread: vi.fn(),
	sendMessage: vi.fn(),
	deleteMessage: vi.fn(),
	state: { authenticated: true, epoch: 0, listeners: [] as Array<(prev: string) => void> }
}));

class NotFound extends Error {
	code = 'not_found';
}

vi.mock('$lib/api/client', () => ({
	api: {
		chat: {
			getConversation: h.getConversation,
			listMessages: h.listMessages,
			markRead: h.markRead,
			unread: h.unread,
			sendMessage: h.sendMessage,
			deleteMessage: h.deleteMessage
		}
	},
	isNotFoundError: (e: unknown) => e instanceof NotFound,
	isReplyTargetDeletedError: (e: unknown) => (e as { code?: string })?.code === 'reply_target_deleted',
	isReplyTargetNotFoundError: (e: unknown) => (e as { code?: string })?.code === 'reply_target_not_found'
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get authenticated() {
			return h.state.authenticated;
		},
		identityFence() {
			const at = h.state.epoch;
			return () => h.state.epoch === at;
		},
		onIdentityChange(fn: (prev: string) => void) {
			h.state.listeners.push(fn);
			return () => {
				const i = h.state.listeners.indexOf(fn);
				if (i >= 0) h.state.listeners.splice(i, 1);
			};
		}
	}
}));

import { chatStore } from './chat.svelte';
import { ChatThread, REFRESH_MAX_PAGES } from './chatThread.svelte';

let visible = true;
Object.defineProperty(document, 'visibilityState', {
	configurable: true,
	get: () => (visible ? 'visible' : 'hidden')
});

/** The shape the real server sends. A human's own message has agent_key "" and mine true. */
function msg(seq: number, over: Partial<ChatMessage> = {}): ChatMessage {
	return {
		id: `m${seq}`,
		conversation_id: 'c1',
		seq,
		author_agent_key: 'agent-1',
		author_is_human: false,
		mine: false,
		source: 'cli',
		body: `body ${seq}`,
		created_at: new Date(Date.UTC(2026, 9, 26, 12, 0, seq)).toISOString(),
		deleted: false,
		...over
	};
}

class FakeServer {
	rows: ChatMessage[] = [];
	readSeq = 0;
	fill(count: number) {
		this.rows = Array.from({ length: count }, (_, i) => msg(i + 1));
	}
	add(m: ChatMessage) {
		this.rows.push(m);
	}
	tombstone(seq: number) {
		this.rows = this.rows.map((m) => (m.seq === seq ? { ...m, body: '', deleted: true } : m));
	}
	get lastSeq() {
		return this.rows.length;
	}
	summary() {
		const unread = this.rows.filter((m) => m.seq > this.readSeq && !m.mine && !m.deleted).length;
		return {
			id: 'c1',
			number: 1,
			kind: 'direct',
			title: '',
			agent_key: 'agent-1',
			last_seq: this.lastSeq,
			created_at: '',
			updated_at: '',
			read_seq: this.readSeq,
			unread
		};
	}
	list(opts: { before_seq?: number; after_seq?: number; limit?: number } = {}) {
		const limit = Math.min(opts.limit ?? 50, 200);
		let rows: ChatMessage[];
		let more: boolean;
		if (opts.after_seq !== undefined) {
			const all = this.rows.filter((m) => m.seq > opts.after_seq!);
			rows = all.slice(0, limit);
			more = all.length > limit;
		} else {
			const all = this.rows.filter((m) => opts.before_seq === undefined || m.seq < opts.before_seq);
			rows = all.slice(Math.max(0, all.length - limit));
			more = all.length > limit;
		}
		return {
			messages: rows.map((m) => ({ ...m })),
			oldest_seq: rows.length ? rows[0].seq : 0,
			newest_seq: rows.length ? rows[rows.length - 1].seq : 0,
			last_seq: this.lastSeq,
			read_seq: this.readSeq,
			has_more: more,
			refs: [],
			refs_truncated: false
		};
	}
}

let server: FakeServer;
let resync: ReturnType<typeof vi.spyOn>;
let thread: ChatThread;

async function flush() {
	for (let i = 0; i < 12; i++) {
		await Promise.resolve();
		flushSync();
	}
}

/** What chatStore does after each applied unread fetch. */
async function bump() {
	chatStore.version++;
	flushSync();
	await flush();
}

const seqs = () => thread.messages.map((m) => m.seq);

beforeEach(() => {
	visible = true;
	h.getConversation.mockReset();
	h.listMessages.mockReset();
	h.markRead.mockReset();
	h.markRead.mockResolvedValue({ read_seq: 0 });
	h.sendMessage.mockReset();
	h.deleteMessage.mockReset();
	Object.assign(h.state, { authenticated: true, epoch: 0, listeners: [] });
	server = new FakeServer();
	h.getConversation.mockImplementation(async () => server.summary());
	h.listMessages.mockImplementation(async (_n: number, o: object) => server.list(o));
	resync = vi.spyOn(chatStore, 'resync').mockImplementation(() => {});
	thread = new ChatThread();
});

afterEach(() => {
	thread.dispose();
	resync.mockRestore();
});

describe('open', () => {
	// U4
	it('orders by seq, not by created_at', async () => {
		server.rows = [
			msg(1, { created_at: '2026-10-26T12:00:09.000Z' }),
			msg(2, { created_at: '2026-10-26T12:00:05.000Z' }),
			msg(3, { created_at: '2026-10-26T12:00:01.000Z' })
		];
		server.list = ((o: object) => {
			const page = FakeServer.prototype.list.call(server, o);
			page.messages = [...page.messages].reverse();
			return page;
		}) as typeof server.list;
		await thread.open(1);
		expect(seqs()).toEqual([1, 2, 3]);
	});

	// U5: the count is the predicate, not the number of rows after the marker.
	it('counts the divider like the server: not mine, not deleted, after the open marker', async () => {
		server.rows = [
			msg(1),
			msg(2, { mine: true, author_agent_key: '', author_is_human: true }),
			msg(3, { deleted: true, body: '' }),
			msg(4),
			msg(5)
		];
		server.readSeq = 1;
		await thread.open(1);
		expect(thread.dividerIndex).toBe(2); // the window starts at seq 2
		expect(thread.unreadCount).toBe(2);
		expect(thread.unreadCount).toBe(server.summary().unread);
	});

	// U6
	it('freezes the divider for the mount when the marker advances', async () => {
		server.fill(4);
		server.readSeq = 1;
		await thread.open(1);
		const at = thread.dividerIndex;
		server.readSeq = 4;
		server.add(msg(5));
		await bump();
		expect(thread.openReadSeq).toBe(1);
		expect(thread.dividerIndex).toBe(at);
	});

	// U7
	it('opens after read_seq when there is unread, so the divider is inside the window', async () => {
		server.fill(120);
		server.readSeq = 60;
		await thread.open(1);
		expect(h.listMessages).toHaveBeenCalledWith(1, { after_seq: 60, limit: 200 });
		expect(thread.messages[0].seq).toBe(61);
		expect(thread.dividerIndex).toBe(0);
		expect(thread.hasOlder).toBe(true);
		expect(thread.hasNewer).toBe(false);
	});

	it('opens on the newest page when nothing is unread', async () => {
		server.fill(120);
		server.readSeq = 120;
		await thread.open(1);
		expect(h.listMessages).toHaveBeenCalledWith(1, { limit: 50 });
		expect(thread.messages).toHaveLength(50);
		expect(thread.newestSeq).toBe(120);
		expect(thread.dividerIndex).toBe(-1);
	});

	it('an unread run longer than a page opens with hasNewer', async () => {
		server.fill(300);
		server.readSeq = 10;
		await thread.open(1);
		expect(thread.messages).toHaveLength(200);
		expect(thread.hasNewer).toBe(true);
	});
});

describe('refresh (R1)', () => {
	// U12: one bump costs the newest-anchored page and the tombstone page, no more.
	it('a bump is two reads; five bumps during one refresh give one more run', async () => {
		server.fill(10);
		server.readSeq = 10;
		await thread.open(1);
		h.listMessages.mockClear();
		let release!: () => void;
		h.listMessages.mockImplementationOnce(
			(_n: number, o: object) => new Promise((res) => (release = () => res(server.list(o))))
		);
		await bump();
		await bump();
		await bump();
		await bump();
		await bump();
		expect(h.listMessages).toHaveBeenCalledTimes(1);
		release();
		await flush();
		// first run: after_seq page + tombstone page; the dirty re-run: the same two.
		expect(h.listMessages).toHaveBeenCalledTimes(4);
	});

	it('a bump with nothing in flight reads after the newest row, then the newest 200', async () => {
		server.fill(10);
		server.readSeq = 10;
		await thread.open(1);
		h.listMessages.mockClear();
		await bump();
		expect(h.listMessages.mock.calls).toEqual([
			[1, { after_seq: 10, limit: 200 }],
			[1, { limit: 200 }]
		]);
	});

	// U12b: red under the oldest-anchored query (M7b), which returns rows 1..200 only.
	it('a window of 250 rows still receives a new message', async () => {
		server.fill(250);
		server.readSeq = 250;
		await thread.open(1);
		// grow the window to 250 rows through load-earlier pages
		while (thread.hasOlder) await thread.loadOlder();
		expect(thread.messages).toHaveLength(250);
		server.add(msg(251, { body: 'the new one' }));
		await bump();
		expect(thread.newestSeq).toBe(251);
		expect(thread.messages.at(-1)?.body).toBe('the new one');
		expect(thread.messages).toHaveLength(251);
	});

	it('pages the new rows while has_more, in order and without duplicates', async () => {
		server.fill(10);
		server.readSeq = 10;
		await thread.open(1);
		for (let i = 11; i <= 460; i++) server.add(msg(i));
		await bump();
		expect(thread.newestSeq).toBe(460);
		expect(new Set(seqs()).size).toBe(thread.messages.length);
		expect(thread.hasNewer).toBe(false);
	});

	it('past the page bound it stops, sets hasNewer, and Show newer continues', async () => {
		server.fill(10);
		server.readSeq = 10;
		await thread.open(1);
		const total = 10 + 200 * REFRESH_MAX_PAGES + 50;
		for (let i = 11; i <= total; i++) server.add(msg(i));
		await bump();
		expect(thread.newestSeq).toBe(10 + 200 * REFRESH_MAX_PAGES);
		expect(thread.hasNewer).toBe(true);
		await thread.loadNewer();
		expect(thread.newestSeq).toBe(total);
		expect(thread.hasNewer).toBe(false);
	});

	// The gap: opened with unread > 200.
	it('does not append past a gap, but still shows a tombstone inside the window', async () => {
		server.fill(300);
		server.readSeq = 10;
		await thread.open(1);
		expect(thread.hasNewer).toBe(true);
		const newest = thread.newestSeq;
		server.add(msg(301));
		server.tombstone(20);
		h.listMessages.mockClear();
		await bump();
		expect(thread.newestSeq).toBe(newest);
		expect(thread.hasNewer).toBe(true);
		expect(thread.messages.find((m) => m.seq === 20)?.deleted).toBe(true);
		expect(h.listMessages).toHaveBeenCalledTimes(1);
		expect(h.listMessages).toHaveBeenCalledWith(1, { after_seq: 10, limit: 200 });
	});

	// U9
	it('a tombstone replaces the body in place', async () => {
		server.fill(5);
		server.readSeq = 5;
		await thread.open(1);
		server.tombstone(3);
		await bump();
		const row = thread.messages.find((m) => m.seq === 3)!;
		expect(row.deleted).toBe(true);
		expect(row.body).toBe('');
		expect(seqs()).toEqual([1, 2, 3, 4, 5]);
	});

	it('does not prepend rows older than the window from the tombstone page', async () => {
		server.fill(100);
		server.readSeq = 60;
		await thread.open(1); // window 61..100
		server.add(msg(101));
		await bump();
		expect(thread.messages[0].seq).toBe(61);
		expect(thread.newestSeq).toBe(101);
	});

	it('a bump that arrives while the open is loading refreshes once after it', async () => {
		server.fill(5);
		server.readSeq = 5;
		let release!: () => void;
		h.getConversation.mockImplementationOnce(
			() => new Promise((res) => (release = () => res(server.summary())))
		);
		const opening = thread.open(1);
		await flush();
		await bump();
		expect(h.listMessages).not.toHaveBeenCalled();
		server.add(msg(6));
		release();
		await opening;
		await flush();
		expect(thread.newestSeq).toBe(6);
	});

	// U13
	it('never subscribes to hints and survives a garbage notice', async () => {
		const sub = vi.spyOn(chatStore, 'subscribe');
		server.fill(3);
		server.readSeq = 3;
		const t = new ChatThread();
		await t.open(1);
		h.unread.mockResolvedValue({ conversations: [], total: 0 });
		h.listMessages.mockClear();
		chatStore.onHint({ kind: 'chat', conversation: 'x' } as never);
		expect(sub).not.toHaveBeenCalled();
		t.dispose();
		sub.mockRestore();
	});
});

describe('fences', () => {
	// U14
	it('drops an answer that resolves after an identity change', async () => {
		server.fill(3);
		let release!: () => void;
		h.listMessages.mockImplementationOnce(
			(_n: number, o: object) => new Promise((res) => (release = () => res(server.list(o))))
		);
		const opening = thread.open(1);
		await flush();
		h.state.epoch++;
		for (const fn of [...h.state.listeners]) fn('previous');
		release();
		await opening;
		await flush();
		expect(thread.messages).toEqual([]);
		expect(thread.status).toBe('loading');
	});

	// U14b (r1 H1): an identity change closes the thread, so the next bump reads nothing
	// as the new identity. Red with #reset() not clearing #n and the status.
	it('fetches nothing on a bump after an identity change', async () => {
		server.fill(3);
		server.readSeq = 3;
		await thread.open(1);
		expect(thread.status).toBe('ready');
		h.state.epoch++;
		for (const fn of [...h.state.listeners]) fn('previous');
		h.listMessages.mockClear();
		h.getConversation.mockClear();
		await bump();
		thread.atBottom = true;
		thread.tryMarkRead();
		await thread.loadOlder();
		await thread.loadNewer();
		await flush();
		expect(h.listMessages).not.toHaveBeenCalled();
		expect(h.getConversation).not.toHaveBeenCalled();
		expect(h.markRead).not.toHaveBeenCalled();
		expect(thread.messages).toEqual([]);
		expect(thread.status).toBe('loading');
	});

	it('drops an answer that resolves after dispose()', async () => {
		server.fill(3);
		let release!: () => void;
		h.listMessages.mockImplementationOnce(
			(_n: number, o: object) => new Promise((res) => (release = () => res(server.list(o))))
		);
		const opening = thread.open(1);
		await flush();
		thread.dispose();
		release();
		await opening;
		await flush();
		expect(thread.messages).toEqual([]);
		expect(thread.status).toBe('loading');
	});
});

describe('load earlier (U15) and failures (U16)', () => {
	it('prepends the page before the oldest row and ends hasOlder at seq 1', async () => {
		server.fill(120);
		server.readSeq = 120;
		await thread.open(1);
		h.listMessages.mockClear();
		await thread.loadOlder();
		expect(h.listMessages).toHaveBeenCalledWith(1, { before_seq: 71, limit: 50 });
		expect(thread.messages[0].seq).toBe(21);
		await thread.loadOlder();
		await thread.loadOlder();
		expect(thread.messages[0].seq).toBe(1);
		expect(thread.hasOlder).toBe(false);
	});

	it('a failed load-earlier keeps the window and flags it', async () => {
		server.fill(120);
		server.readSeq = 120;
		await thread.open(1);
		h.listMessages.mockRejectedValueOnce(new Error('500'));
		await thread.loadOlder();
		expect(thread.messages).toHaveLength(50);
		expect(thread.olderFailed).toBe(true);
		await thread.loadOlder();
		expect(thread.olderFailed).toBe(false);
		expect(thread.messages).toHaveLength(100);
	});

	it('a failed refresh keeps the window and flags it; a retry clears it', async () => {
		server.fill(5);
		server.readSeq = 5;
		await thread.open(1);
		h.listMessages.mockRejectedValueOnce(new Error('500'));
		await bump();
		expect(thread.refreshFailed).toBe(true);
		expect(seqs()).toEqual([1, 2, 3, 4, 5]);
		server.add(msg(6));
		await thread.refresh();
		expect(thread.refreshFailed).toBe(false);
		expect(seqs()).toEqual([1, 2, 3, 4, 5, 6]);
	});

	it('an initial failure is failed, a 404 is notFound, and reopening does not duplicate', async () => {
		server.fill(3);
		h.getConversation.mockRejectedValueOnce(new Error('500'));
		await thread.open(1);
		expect(thread.status).toBe('failed');
		await thread.open(1);
		expect(thread.status).toBe('ready');
		expect(seqs()).toEqual([1, 2, 3]);
		h.getConversation.mockRejectedValueOnce(new NotFound('nope'));
		await thread.open(2);
		expect(thread.status).toBe('notFound');
	});

	it('a null conversation number is notFound with no request', async () => {
		await thread.open(null);
		expect(thread.status).toBe('notFound');
		expect(h.getConversation).not.toHaveBeenCalled();
	});
});

// U17
describe('mark read', () => {
	async function opened(unread = 3) {
		server.fill(unread);
		server.readSeq = 0;
		await thread.open(1);
		thread.atBottom = true;
	}

	it('sends the newest confirmed seq when the end is in view on a visible tab, then resyncs the badge', async () => {
		await opened();
		thread.tryMarkRead();
		await flush();
		expect(h.markRead).toHaveBeenCalledWith(1, 3);
		expect(resync).toHaveBeenCalledTimes(1);
	});

	it('sends nothing until the end is in view', async () => {
		await opened();
		thread.atBottom = false;
		thread.tryMarkRead();
		await flush();
		expect(h.markRead).not.toHaveBeenCalled();
	});

	it('sends nothing on a hidden tab', async () => {
		await opened();
		visible = false;
		thread.tryMarkRead();
		await flush();
		expect(h.markRead).not.toHaveBeenCalled();
	});

	it('sends the displayed seq, not last_seq, when a gap hides newer rows', async () => {
		server.fill(300);
		server.readSeq = 10;
		await thread.open(1);
		thread.atBottom = true;
		thread.tryMarkRead();
		await flush();
		expect(h.markRead).toHaveBeenCalledWith(1, 210);
	});

	it('marks nothing locally on a failure and retries on the next call', async () => {
		await opened();
		h.markRead.mockRejectedValueOnce(new Error('500'));
		thread.tryMarkRead();
		await flush();
		expect(resync).not.toHaveBeenCalled();
		thread.tryMarkRead();
		await flush();
		expect(h.markRead).toHaveBeenCalledTimes(2);
		expect(resync).toHaveBeenCalledTimes(1);
	});

	// U17b (r2 hunter): rows that land while a mark is in flight are marked when it settles.
	it('marks rows that arrived while a mark was in flight', async () => {
		await opened();
		let release!: () => void;
		h.markRead.mockImplementationOnce(() => new Promise((res) => (release = () => res({ read_seq: 3 }))));
		thread.tryMarkRead();
		await flush();
		server.add(msg(4));
		await bump();
		expect(thread.newestSeq).toBe(4);
		expect(h.markRead).toHaveBeenCalledTimes(1);
		release();
		await flush();
		expect(h.markRead).toHaveBeenCalledTimes(2);
		expect(h.markRead).toHaveBeenLastCalledWith(1, 4);
	});

	// U17c: red when the retry is not gated on success.
	it('a failed mark does not retry by itself, even when rows arrived', async () => {
		await opened();
		let fail!: () => void;
		h.markRead.mockImplementationOnce(() => new Promise((_res, rej) => (fail = () => rej(new Error('500')))));
		thread.tryMarkRead();
		await flush();
		server.add(msg(4));
		await bump();
		fail();
		await flush();
		expect(h.markRead).toHaveBeenCalledTimes(1);
	});

	// U17d: red when the retry is not gated on the window having grown past the marked seq.
	it('a successful mark with nothing new does not try again', async () => {
		await opened();
		const again = vi.spyOn(thread, 'tryMarkRead');
		thread.tryMarkRead();
		await flush();
		expect(h.markRead).toHaveBeenCalledTimes(1);
		expect(again).toHaveBeenCalledTimes(1);
	});

	it('does not repeat a seq the server already has', async () => {
		await opened();
		thread.tryMarkRead();
		await flush();
		thread.tryMarkRead();
		await flush();
		expect(h.markRead).toHaveBeenCalledTimes(1);
	});
});

// ── PR 3b: the write path ────────────────────────────────────────────────────

/** A server that stores a send under its op_key, like the real one (V10: a replay returns the stored row). */
function wireSend() {
	const byKey = new Map<string, ChatMessage>();
	h.sendMessage.mockImplementation(async (_n: number, body: { body: string; op_key: string; reply_to_seq?: number }) => {
		const seen = byKey.get(body.op_key);
		if (seen) return { message: { ...seen }, replayed: true, refs: [], refs_truncated: false };
		const m = msg(server.lastSeq + 1, {
			mine: true,
			author_agent_key: '',
			author_is_human: true,
			source: 'web',
			body: body.body
		});
		server.add(m);
		byKey.set(body.op_key, m);
		return { message: { ...m }, replayed: false, refs: [], refs_truncated: false };
	});
	return byKey;
}

const apiError = (code: string, message = code) => Object.assign(new Error(message), { code });
const pendingOf = () => thread.items.filter((i) => i.kind === 'pending');

async function readyThread(rows = 3) {
	server.fill(rows);
	await thread.open(1);
}

describe('send', () => {
	// U21: red if resolvePending appends instead of merging by seq (B2).
	it('shows a sending bubble at once and replaces it with the confirmed row', async () => {
		await readyThread();
		wireSend();
		let release!: () => void;
		const real = h.sendMessage.getMockImplementation()!;
		h.sendMessage.mockImplementationOnce(
			(n: number, b: never) => new Promise((res) => (release = () => res(real(n, b))))
		);
		const done = thread.send('hello');
		expect(pendingOf()).toHaveLength(1);
		expect(thread.items.at(-1)).toMatchObject({ kind: 'pending', pending: { body: 'hello', status: 'sending' } });
		release();
		expect(await done).toBe('sent');
		expect(pendingOf()).toHaveLength(0);
		expect(seqs()).toEqual([1, 2, 3, 4]);
		expect(thread.messages.at(-1)).toMatchObject({ body: 'hello', mine: true, source: 'web', author_agent_key: '' });
	});

	it('never sends a context_workspace (Q6)', async () => {
		await readyThread();
		wireSend();
		await thread.send('hi');
		expect(Object.keys(h.sendMessage.mock.calls[0][1]).sort()).toEqual(['body', 'op_key']);
	});

	it('leaves one row when a refresh got the message first (replayed)', async () => {
		await readyThread();
		wireSend();
		const real = h.sendMessage.getMockImplementation()!;
		let release!: () => void;
		h.sendMessage.mockImplementationOnce(
			(n: number, b: never) => new Promise((res) => (release = () => res(real(n, b))))
		);
		const done = thread.send('hello');
		await flush();
		release();
		await done;
		await thread.refresh();
		expect(seqs()).toEqual([1, 2, 3, 4]);
	});

	// U22: red if a retry makes a new key (B1).
	it('keeps a failed bubble and retries with the SAME op_key', async () => {
		await readyThread();
		wireSend();
		h.sendMessage.mockRejectedValueOnce(new Error('network'));
		expect(await thread.send('hello')).toBe('failed');
		expect(pendingOf()).toMatchObject([{ pending: { status: 'failed', body: 'hello' } }]);
		const key = (pendingOf()[0] as { pending: { op_key: string } }).pending.op_key;
		expect(await thread.retry(key)).toBe('sent');
		expect(h.sendMessage.mock.calls[1][1].op_key).toBe(h.sendMessage.mock.calls[0][1].op_key);
		expect(pendingOf()).toHaveLength(0);
		expect(seqs()).toEqual([1, 2, 3, 4]);
	});

	// N4: the write committed, the answer was lost, a refresh shows the row beside the failed bubble.
	it('collapses to one row when Retry replays a send whose answer was lost', async () => {
		await readyThread();
		const byKey = wireSend();
		const real = h.sendMessage.getMockImplementation()!;
		h.sendMessage.mockImplementationOnce(async (n: number, b: never) => {
			await real(n, b); // committed on the server
			throw new Error('timeout');
		});
		expect(await thread.send('hello')).toBe('failed');
		await thread.refresh();
		expect(seqs()).toEqual([1, 2, 3, 4]);
		expect(pendingOf()).toHaveLength(1); // the disclosed duplicate
		const key = (pendingOf()[0] as { pending: { op_key: string } }).pending.op_key;
		expect(await thread.retry(key)).toBe('sent');
		expect((await h.sendMessage.mock.results[1].value).replayed).toBe(true);
		expect(byKey.size).toBe(1);
		expect(seqs()).toEqual([1, 2, 3, 4]);
		expect(pendingOf()).toHaveLength(0);
	});

	it('discards a failed bubble', async () => {
		await readyThread();
		h.sendMessage.mockRejectedValueOnce(new Error('network'));
		await thread.send('hello');
		thread.discard((pendingOf()[0] as { pending: { op_key: string } }).pending.op_key);
		expect(pendingOf()).toHaveLength(0);
	});

	// U23
	it('sends reply_to_seq, and a deleted or missing target leaves no bubble', async () => {
		await readyThread();
		wireSend();
		await thread.send('re', 2);
		expect(h.sendMessage.mock.calls[0][1].reply_to_seq).toBe(2);
		h.sendMessage.mockRejectedValueOnce(apiError('reply_target_deleted', 'Reply target was deleted'));
		expect(await thread.send('again', 3)).toBe('reply_deleted');
		h.sendMessage.mockRejectedValueOnce(apiError('reply_target_not_found'));
		expect(await thread.send('again', 3)).toBe('reply_missing');
		expect(pendingOf()).toHaveLength(0);
	});

	// N1, N2, N3: refused before the network, so the call count is the assertion.
	it.each([
		['\u0085', 'empty'],
		['😀'.repeat(20_000), 'too_long'],
		['"'.repeat(65_536), 'too_large'],
		['\u0001'.repeat(22_000), 'too_large']
	] as const)('does not send a draft the server would refuse (%#)', async (body, outcome) => {
		await readyThread();
		expect(await thread.send(body)).toBe(outcome);
		expect(h.sendMessage).not.toHaveBeenCalled();
		expect(pendingOf()).toHaveLength(0);
	});

	it('sends a body of only U+FEFF, which the server does not trim', async () => {
		await readyThread();
		wireSend();
		expect(await thread.send('﻿')).toBe('sent');
	});

	// N3, defence in depth: the server's own 400 maps to the same outcome.
	it('maps the server 400 for an over-size request to too_large, with no bubble', async () => {
		await readyThread();
		h.sendMessage.mockRejectedValueOnce(apiError('validation_error', 'Invalid JSON body (maximum 128 KiB)'));
		expect(await thread.send('x')).toBe('too_large');
		expect(pendingOf()).toHaveLength(0);
	});

	// U-gap (B15): red if the confirmed row is discarded when a gap is open. The
	// window moves to the newest page, so the sender sees the row and no row of
	// the old window sits beside it as if contiguous.
	it('re-anchors at the newest page after a send beyond a gap, so the sender sees it', async () => {
		server.fill(400);
		server.readSeq = 0;
		await thread.open(1); // unread 400 > 200: the window ends at 200, hasNewer
		expect(thread.hasNewer).toBe(true);
		const scrollBefore = thread.ownSendTick;
		wireSend();
		expect(await thread.send('hello')).toBe('sent');
		const seqs = thread.messages.map((m) => m.seq);
		const sent = thread.messages.find((m) => m.body === 'hello');
		expect(sent).toBeDefined();
		expect(thread.newestSeq).toBe(sent!.seq);
		for (let i = 1; i < seqs.length; i++) expect(seqs[i]).toBe(seqs[i - 1] + 1);
		expect(seqs).not.toContain(200);
		expect(thread.hasNewer).toBe(false);
		expect(thread.hasOlder).toBe(true);
		expect(pendingOf()).toHaveLength(0);
		expect(thread.ownSendTick).toBeGreaterThan(scrollBefore);
	});

	it('falls back to the confirmed row alone when the newest page cannot be fetched', async () => {
		server.fill(400);
		server.readSeq = 0;
		await thread.open(1);
		wireSend();
		h.listMessages.mockRejectedValueOnce(new Error('offline'));
		expect(await thread.send('hello')).toBe('sent');
		expect(thread.messages.map((m) => m.body)).toEqual(['hello']);
		expect(thread.hasNewer).toBe(false);
		expect(pendingOf()).toHaveLength(0);
	});

	// 3b r1 hunter finding 2: red if a re-anchor whose fetch lands last replaces the
	// window a later send's re-anchor already moved (B16).
	it('keeps both own rows when two re-anchors land out of order', async () => {
		server.fill(400);
		server.readSeq = 0;
		await thread.open(1);
		expect(thread.hasNewer).toBe(true);
		wireSend();
		let releaseA!: () => void;
		h.listMessages.mockImplementationOnce((_n: number, o: object) => {
			const page = server.list(o);
			return new Promise((res) => (releaseA = () => res(page)));
		});
		const a = thread.send('A');
		await flush();
		expect(typeof releaseA).toBe('function');
		expect(await thread.send('B')).toBe('sent');
		releaseA();
		expect(await a).toBe('sent');
		await flush();
		const bodies = thread.messages.map((m) => m.body);
		expect(bodies.slice(-2)).toEqual(['A', 'B']);
		const s = seqs();
		for (let i = 1; i < s.length; i++) expect(s[i]).toBe(s[i - 1] + 1);
		expect(pendingOf()).toHaveLength(0);
	});

	it('is stale after an identity change, with nothing of the old bubble left', async () => {
		await readyThread();
		wireSend();
		let release!: () => void;
		const real = h.sendMessage.getMockImplementation()!;
		h.sendMessage.mockImplementationOnce((n: number, b: never) => new Promise((res) => (release = () => res(real(n, b)))));
		const done = thread.send('hello');
		h.state.epoch++;
		for (const fn of [...h.state.listeners]) fn('old');
		release();
		expect(await done).toBe('stale');
		expect(thread.items).toEqual([]);
	});
});

describe('refresh during a send', () => {
	// U-defer: red if the in-flight deferral is removed (B12).
	it('defers a realtime refresh until the send settles, then refreshes once', async () => {
		await readyThread();
		wireSend();
		const real = h.sendMessage.getMockImplementation()!;
		let release!: () => void;
		h.sendMessage.mockImplementationOnce((n: number, b: never) => new Promise((res) => (release = () => res(real(n, b)))));
		const done = thread.send('hello');
		h.listMessages.mockClear();
		await bump();
		await bump();
		expect(h.listMessages).not.toHaveBeenCalled();
		release();
		await done;
		await flush();
		const calls = h.listMessages.mock.calls.length;
		expect(calls).toBeGreaterThan(0);
		await flush();
		expect(h.listMessages.mock.calls.length).toBe(calls);
		expect(seqs()).toEqual([1, 2, 3, 4]);
	});

	// 3b r1 hunter finding 1 (refuted): the deferred refresh's cursor starts past a
	// peer row committed during the send, and the sweep leg brings it in.
	it('does not skip a peer row committed while a send was in flight', async () => {
		await readyThread();
		wireSend();
		const real = h.sendMessage.getMockImplementation()!;
		let release!: () => void;
		h.sendMessage.mockImplementationOnce((n: number, b: never) => new Promise((res) => (release = () => res(real(n, b)))));
		const done = thread.send('hello');
		server.add(msg(4));
		await bump();
		release();
		await done;
		await flush();
		expect(seqs()).toEqual([1, 2, 3, 4, 5]);
	});

	// r1 hunter 1 (ruling a61ca213): red if the one-shot cursor floor is dropped (B18).
	// More than the sweep's 200: only step 1 from below the own row can fetch them.
	it('does not skip any of 201 peer rows committed while a send was in flight', async () => {
		await readyThread();
		wireSend();
		const real = h.sendMessage.getMockImplementation()!;
		let release!: () => void;
		h.sendMessage.mockImplementationOnce((n: number, b: never) => new Promise((res) => (release = () => res(real(n, b)))));
		const done = thread.send('hello');
		for (let seq = 4; seq <= 204; seq++) server.add(msg(seq));
		await bump();
		release();
		await done;
		await flush();
		await flush();
		expect(seqs()).toEqual(Array.from({ length: 205 }, (_, i) => i + 1));
		expect(thread.hasNewer).toBe(false);
	});
});

describe('remove', () => {
	// U24, N6: red if the tombstone is not merged (B5).
	it('replaces the row with the tombstone in place', async () => {
		await readyThread();
		server.tombstone(2);
		h.deleteMessage.mockResolvedValue({ message: { ...server.rows[1] }, deleted: true });
		expect(await thread.remove(2)).toBe(true);
		expect(h.deleteMessage).toHaveBeenCalledWith(1, 2);
		expect(thread.messages[1]).toMatchObject({ seq: 2, deleted: true, body: '' });
	});

	it('treats deleted:false with the tombstone as success', async () => {
		await readyThread();
		server.tombstone(2);
		h.deleteMessage.mockResolvedValue({ message: { ...server.rows[1] }, deleted: false });
		expect(await thread.remove(2)).toBe(true);
		expect(thread.messages[1].deleted).toBe(true);
	});

	it('keeps the row when the delete fails', async () => {
		await readyThread();
		h.deleteMessage.mockRejectedValue(apiError('forbidden'));
		expect(await thread.remove(2)).toBe(false);
		expect(thread.messages[1]).toMatchObject({ seq: 2, deleted: false, body: 'body 2' });
		expect(thread.status).toBe('ready');
	});

	it('says not found when the conversation is gone', async () => {
		await readyThread();
		h.deleteMessage.mockRejectedValue(new NotFound());
		expect(await thread.remove(2)).toBe(false);
		expect(thread.status).toBe('notFound');
	});
});

describe('3b r2 ordering', () => {
	const contiguous = () => {
		const s = seqs();
		for (let i = 1; i < s.length; i++) if (s[i] !== s[i - 1] + 1) return `break at ${s[i - 1]}->${s[i]}`;
		return 'ok';
	};

	// r2 H1: red if a refresh clears a floor it did not start from (B19).
	it('keeps a floor that a send set while a refresh was in flight', async () => {
		await readyThread();
		wireSend();
		let release!: () => void;
		h.listMessages.mockImplementationOnce((_n: number, o: object) => {
			const page = server.list(o);
			return new Promise((res) => (release = () => res(page)));
		});
		await bump();
		for (let seq = 4; seq <= 204; seq++) server.add(msg(seq));
		expect(await thread.send('hello')).toBe('sent');
		release();
		await flush();
		await flush();
		expect(contiguous()).toBe('ok');
		expect(thread.newestSeq).toBe(205);
	});

	// r2 C1/H2: red if a late re-anchor merges its row past unfetched peer rows
	// without a floor (B20).
	it('fills the peer row between when a late re-anchor merges a newer own row', async () => {
		server.fill(400);
		server.readSeq = 0;
		await thread.open(1);
		wireSend();
		const holds: Array<() => void> = [];
		h.listMessages.mockImplementation((_n: number, o: object) => {
			const page = server.list(o);
			return new Promise((res) => holds.push(() => res(page)));
		});
		const b = thread.send('B');
		await flush();
		server.add(msg(402));
		const a = thread.send('A');
		await flush();
		expect(holds).toHaveLength(2);
		holds[0]();
		await flush();
		h.listMessages.mockImplementation(async (_n: number, o: object) => server.list(o));
		holds[1]();
		expect(await b).toBe('sent');
		expect(await a).toBe('sent');
		await flush();
		await flush();
		expect(contiguous()).toBe('ok');
		expect(thread.messages.slice(-3).map((m) => m.seq)).toEqual([401, 402, 403]);
		expect(pendingOf()).toHaveLength(0);
	});

	// r2 H3: red if a delete answer merges a row the window no longer holds (B21).
	it('does not insert a tombstone below a window that a re-anchor moved', async () => {
		server.fill(400);
		server.readSeq = 0;
		await thread.open(1);
		wireSend();
		server.tombstone(5);
		let release!: () => void;
		h.deleteMessage.mockImplementation(
			() => new Promise((res) => (release = () => res({ message: { ...server.rows[4] }, deleted: true })))
		);
		const d = thread.remove(5);
		expect(await thread.send('hello')).toBe('sent');
		release();
		expect(await d).toBe(true);
		await flush();
		expect(contiguous()).toBe('ok');
		expect(seqs()).not.toContain(5);
	});
});
