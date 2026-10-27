// TASK-4044 3b S1 (rulings 3293b1c6, 5c95ef40): a seeded random walk over the
// open-thread store against a fake server. Every API call computes its answer
// when it is MADE and delivers it when the driver releases it, so answers land in
// a random order and a page can be stale by the time it applies. Sends and
// deletes commit on the server at call time. After each step (and at every mock
// entry) the invariants below must hold; at the end the thread settles and must
// equal the server.
//
//   I1 contiguity: messages[i].seq === messages[i-1].seq + 1
//   I2 detached seqs ascend and sit above the window's newest; op_keys are unique
//   I3 at rest the window equals the server, row for row, detached empty
//   I4 detached rows imply hasNewer; hasOlder is oldest > 1
//   I5 a row the reader deleted never reads as live again
//   I6 mark-read marks the window's newest; no cursor reaches a detached row
//   I7 at most one window fetch is outstanding per identity
//   I8 nothing of an earlier identity survives (rows are tagged per server)
//   I9 a committed own row and its pending bubble are never both visible
//   I10 an own row detaches only with a cause recorded FOR THAT ROW (3b r4 C2): a
//       refused list while its bubble waited, overlapping sends, or a gap wider
//       than the fill bound; a fill never pages past its bound
//
// PROP_SEED=k replays one seed; PROP_SEEDS=n sets how many (deep runs). No
// Date, no Math.random: mulberry32 and a counter for crypto.randomUUID.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import type { ChatMessage } from '$lib/types';

const h = vi.hoisted(() => ({
	getConversation: vi.fn(),
	listMessages: vi.fn(),
	markRead: vi.fn(),
	sendMessage: vi.fn(),
	deleteMessage: vi.fn(),
	state: { authenticated: true, epoch: 0, listeners: [] as Array<(prev: string) => void> }
}));

vi.mock('$lib/api/client', async (orig) => {
	const actual = await orig<typeof import('$lib/api/client')>();
	return {
		...actual,
		api: {
			chat: {
				getConversation: h.getConversation,
				listMessages: h.listMessages,
				markRead: h.markRead,
				sendMessage: h.sendMessage,
				deleteMessage: h.deleteMessage
			}
		},
		// the REAL predicates (3b r4 C2): the refusals the walk throws must be ones they recognise
		isNotFoundError: actual.isNotFoundError,
		isReplyTargetDeletedError: actual.isReplyTargetDeletedError,
		isReplyTargetNotFoundError: actual.isReplyTargetNotFoundError
	};
});
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

import { networkUnreachableError, isNotFoundError, isReplyTargetDeletedError, isReplyTargetNotFoundError } from '$lib/api/client';
import { isRequestTooLargeError } from '$lib/chat/compose';
import { chatStore } from './chat.svelte';
import { ChatThread, REFRESH_MAX_PAGES } from './chatThread.svelte';

Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'visible' });

const SEEDS = Number(process.env.PROP_SEEDS ?? 100);
const ONLY = process.env.PROP_SEED === undefined ? null : Number(process.env.PROP_SEED);
const STEPS = 80;

function mulberry32(seed: number) {
	let a = seed | 0;
	return () => {
		a = (a + 0x6d2b79f5) | 0;
		let t = Math.imul(a ^ (a >>> 15), 1 | a);
		t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
		return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
	};
}

function msg(seq: number, tag: string, over: Partial<ChatMessage> = {}): ChatMessage {
	return {
		id: `${tag}${seq}`,
		conversation_id: tag,
		seq,
		author_agent_key: 'agent-1',
		author_is_human: false,
		mine: false,
		source: 'cli',
		body: `${tag} body ${seq}`,
		created_at: '2026-10-26T12:00:00.000Z',
		deleted: false,
		...over
	};
}

class Server {
	rows: ChatMessage[] = [];
	readSeq = 0;
	byKey = new Map<string, ChatMessage>();
	constructor(readonly tag: string) {}
	get last() {
		return this.rows.length;
	}
	add(n: number) {
		for (let i = 0; i < n; i++) this.rows.push(msg(this.last + 1, this.tag));
	}
	tombstone(seq: number) {
		this.rows = this.rows.map((m) => (m.seq === seq ? { ...m, body: '', deleted: true } : m));
	}
	summary() {
		const unread = this.rows.filter((m) => m.seq > this.readSeq && !m.mine && !m.deleted).length;
		return { id: this.tag, number: 1, kind: 'direct', title: '', agent_key: 'agent-1', last_seq: this.last, created_at: '', updated_at: '', read_seq: this.readSeq, unread };
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
			last_seq: this.last,
			read_seq: this.readSeq,
			has_more: more,
			refs: [],
			refs_truncated: false
		};
	}
}

type Held = { kind: string; key?: string; release: (ok: boolean) => void };

let server: Server;
let thread: ChatThread;
let held: Held[] = [];
let immediate = true;
let inflight = new Map<number, number>();
let deleted = new Set<number>();
let uuid = 0;
let causes = new Map<string, Set<string>>();
const addCause = (key: string, why: string) => causes.set(key, (causes.get(key) ?? new Set()).add(why));

/** An error the real predicates accept: the class is not exported, so borrow its constructor. */
function apiErr(code: string, message = code): Error {
	const Ctor = networkUnreachableError(true).constructor as new (e: { code: string; message: string }) => Error;
	return new Ctor({ code, message });
}
const REFUSALS = [
	() => new Error('network'),
	() => apiErr('reply_target_deleted'),
	() => apiErr('reply_target_not_found'),
	() => apiErr('validation_error', 'Invalid JSON body (maximum 128 KiB)')
];
const answered = new Set<string>();
const fillCalls = new Map<string, number>();
let rng: () => number;

async function flush() {
	for (let i = 0; i < 12; i++) {
		await Promise.resolve();
		flushSync();
	}
}

// A mock that throws is swallowed by the store's own catch, so a violation is also recorded.
let violations: string[] = [];
const fail = (rule: string): never => {
	violations.push(rule);
	throw new Error(rule);
};

function check(where: string) {
	const sending = thread.items.flatMap((i) => (i.kind === 'pending' && i.pending.status === 'sending' ? [i.pending.op_key] : []));
	if (sending.length > 1) for (const k of sending) addCause(k, 'overlap');
	const m = thread.messages;
	for (let i = 1; i < m.length; i++) if (m[i].seq !== m[i - 1].seq + 1) fail(`I1 ${where}: ${m[i - 1].seq}->${m[i].seq}`);
	const newest = m.length ? m[m.length - 1].seq : 0;
	if (thread.newestSeq !== newest) fail(`I6 ${where}: newestSeq ${thread.newestSeq} != window end ${newest}`);
	const d = thread.detached;
	for (let i = 0; i < d.length; i++) {
		if (d[i].seq <= newest) fail(`I2 ${where}: detached ${d[i].seq} <= newest ${newest}`);
		if (i > 0 && d[i].seq <= d[i - 1].seq) fail(`I2 ${where}: detached not ascending`);
	}
	if (d.length && !thread.hasNewer) fail(`I4 ${where}: detached without hasNewer`);
	for (const row of d) {
		const key = [...server.byKey].find(([, r]) => r.seq === row.seq)?.[0];
		if (!key || !causes.get(key)?.size) fail(`I10 ${where}: detached ${row.seq} with no cause of its own`);
	}
	if (m.length && thread.hasOlder !== m[0].seq > 1) fail(`I4 ${where}: hasOlder ${thread.hasOlder} oldest ${m[0].seq}`);
	const bubbles = thread.items.flatMap((i) => (i.kind === 'pending' ? [i.pending] : []));
	const keys = bubbles.map((p) => p.op_key);
	if (new Set(keys).size !== keys.length) fail(`I2 ${where}: duplicate op_key`);
	// a failed bubble beside a row whose answer was lost is the disclosed N4; this walk only refuses before the commit
	for (const k of bubbles.filter((p) => p.status === 'sending').map((p) => p.op_key)) {
		const row = server.byKey.get(k);
		if (row && (m.some((x) => x.seq === row.seq) || d.some((x) => x.seq === row.seq))) fail(`I9 ${where}: seq ${row.seq} and its bubble both visible`);
	}
	for (const row of [...m, ...d]) {
		if (!row.id.startsWith(server.tag)) fail(`I8 ${where}: row ${row.id} of an earlier identity`);
		if (deleted.has(row.seq) && !row.deleted) fail(`I5 ${where}: seq ${row.seq} live again`);
	}
	const unread = m.filter((x) => x.seq > thread.openReadSeq && !x.mine && !x.deleted).length;
	if (thread.unreadCount !== unread) fail(`I6 ${where}: unreadCount`);
}

function deliver<T>(kind: string, value: T, rejectIt: Error | false = false, key?: string): Promise<T> {
	if (immediate) return rejectIt ? Promise.reject(new Error('x')) : Promise.resolve(value);
	return new Promise<T>((res, rej) =>
		held.push({
			kind,
			key,
			release: (ok) => {
				if (!ok && kind === 'list') {
					for (const i of thread.items) if (i.kind === 'pending' && i.pending.status === 'sending' && answered.has(i.pending.op_key)) addCause(i.pending.op_key, 'rejected');
				}
				if (ok && !rejectIt && kind === 'send') {
					answered.add(key!);
					const seq = (value as { message: ChatMessage }).message.seq;
					if (seq - thread.newestSeq - 1 > 200 * REFRESH_MAX_PAGES) addCause(key!, 'wideGap');
				}
				if (ok && !rejectIt) res(value);
				else rej(rejectIt || new Error('rejected'));
			}
		})
	);
}

function wire() {
	h.getConversation.mockImplementation(async () => server.summary());
	h.listMessages.mockImplementation((_n: number, o: { after_seq?: number; before_seq?: number; limit?: number }) => {
		check('list');
		const ep = h.state.epoch;
		if (thread.status === 'ready' && thread.messages.length && o.after_seq !== undefined && o.after_seq > thread.newestSeq) {
			fail(`I6 list: after_seq ${o.after_seq} past the window's newest ${thread.newestSeq}`);
		}
		if (o.after_seq !== undefined) {
			// a fill (or anything else paging forward) while an answered send waits for its apply
			for (const i of thread.items) {
				if (i.kind !== 'pending' || i.pending.status !== 'sending' || !answered.has(i.pending.op_key)) continue;
				const k = i.pending.op_key;
				fillCalls.set(k, (fillCalls.get(k) ?? 0) + 1);
				if (fillCalls.get(k)! > REFRESH_MAX_PAGES + 3) fail(`I10 list: ${fillCalls.get(k)} forward pages while ${k} waits to be placed`);
			}
		}
		const n = inflight.get(ep) ?? 0;
		if (n > 0) fail('I7 list: a second window fetch while one is outstanding');
		inflight.set(ep, n + 1);
		return deliver('list', server.list(o)).finally(() => inflight.set(ep, (inflight.get(ep) ?? 1) - 1));
	});
	h.markRead.mockImplementation(async (_n: number, seq: number) => {
		check('markRead');
		if (seq !== thread.newestSeq) fail(`I6 markRead: ${seq} != window newest ${thread.newestSeq}`);
		server.readSeq = Math.max(server.readSeq, seq);
		return { read_seq: seq };
	});
	h.sendMessage.mockImplementation((_n: number, b: { body: string; op_key: string }) => {
		check('send');
		const seen = server.byKey.get(b.op_key);
		if (seen) return deliver('send', { message: { ...seen }, replayed: true, refs: [], refs_truncated: false }, false, b.op_key);
		if (!immediate && rng() < 0.05) return deliver('send', null as never, REFUSALS[Math.floor(rng() * REFUSALS.length)](), b.op_key); // refused before the commit
		const m = msg(server.last + 1, server.tag, { mine: true, author_agent_key: '', author_is_human: true, source: 'web', body: b.body });
		server.rows.push(m);
		server.byKey.set(b.op_key, m);
		return deliver('send', { message: { ...m }, replayed: false, refs: [], refs_truncated: false }, false, b.op_key);
	});
	h.deleteMessage.mockImplementation((_n: number, seq: number) => {
		check('delete');
		server.tombstone(seq);
		return deliver('delete', { message: { ...server.rows[seq - 1] }, deleted: true });
	});
}

function releaseOne(ok: boolean) {
	const i = Math.floor(rng() * held.length);
	const [x] = held.splice(i, 1);
	x.release(ok);
}

async function openFresh() {
	const tag = `s${uuid}-${h.state.epoch}`;
	server = new Server(tag);
	server.add(Math.floor(rng() * 451));
	server.readSeq = Math.floor(rng() * (server.last + 1));
	deleted = new Set();
	immediate = true;
	await thread.open(1);
	immediate = false;
}

type Action = [number, string, () => void | Promise<void>];

async function walk(seed: number, trace: string[]) {
	rng = mulberry32(seed);
	held = [];
	violations = [];
	causes = new Map();
	answered.clear();
	fillCalls.clear();
	inflight = new Map();
	h.state.epoch = 0;
	h.state.listeners = [];
	thread = new ChatThread();
	const fired: Promise<unknown>[] = [];
	const visibleSeqs = () => [...thread.messages, ...thread.detached].map((m) => m.seq);
	const actions: Action[] = [
		[22, 'send', () => {
			const all = visibleSeqs();
			const reply = all.length && rng() < 0.2 ? all[Math.floor(rng() * all.length)] : undefined;
			fired.push(thread.send(`b${uuid++}`, reply));
		}],
		[8, 'remove', () => {
			const all = visibleSeqs();
			if (!all.length) return;
			const seq = all[Math.floor(rng() * all.length)];
			fired.push(thread.remove(seq).then((ok) => ok && deleted.add(seq)));
		}],
		[6, 'loadOlder', () => void fired.push(thread.loadOlder())],
		[8, 'loadNewer', () => void fired.push(thread.loadNewer())],
		[15, 'bump', () => {
			chatStore.version++;
			flushSync();
		}],
		[5, 'markRead', () => {
			thread.atBottom = rng() < 0.7;
			thread.tryMarkRead();
		}],
		[4, 'retry', () => {
			const f = thread.items.find((i) => i.kind === 'pending' && i.pending.status === 'failed');
			if (f && f.kind === 'pending') fired.push(thread.retry(f.pending.op_key));
		}],
		[4, 'discard', () => {
			const f = thread.items.find((i) => i.kind === 'pending' && i.pending.status === 'failed');
			if (f && f.kind === 'pending') thread.discard(f.pending.op_key);
		}],
		[15, 'peer+', () => server.add(1 + Math.floor(rng() * 3))],
		[2, 'burst', () => server.add(210 + Math.floor(rng() * 991))],
		[2, 'wide-burst', () => server.add(1100 + Math.floor(rng() * 1400))],
		[5, 'peer-tombstone', () => server.last && server.tombstone(1 + Math.floor(rng() * server.last))],
		[35, 'release', () => held.length && releaseOne(true)],
		[3, 'reject-list', () => {
			const i = held.findIndex((x) => x.kind === 'list');
			if (i >= 0) held.splice(i, 1)[0].release(false);
		}],
		[1, 'identity', async () => {
			h.state.epoch++;
			for (const fn of [...h.state.listeners]) fn('previous');
			await openFresh();
		}]
	];
	const total = actions.reduce((a, x) => a + x[0], 0);
	await openFresh();
	for (let step = 0; step < STEPS; step++) {
		let r = rng() * total;
		const act = actions.find((x) => (r -= x[0]) < 0) ?? actions[0];
		const d = thread.detached.map((m) => m.seq).join(',');
		trace.push(`${step} ${act[1]} held=${held.length} win=[${thread.messages[0]?.seq ?? '-'}..${thread.newestSeq}] det=[${d}] hasNewer=${thread.hasNewer} server=${server.last}`);
		await act[2]();
		await flush();
		check(`step ${step} after ${act[1]}`);
		if (violations.length) throw new Error(violations[0]);
	}
	// settle: deliver everything, then page to the end
	immediate = true;
	trace.push('settle');
	while (held.length) releaseOne(true);
	await Promise.allSettled(fired);
	await flush();
	for (const i of thread.items) if (i.kind === 'pending' && i.pending.status === 'failed') thread.discard(i.pending.op_key);
	for (let r = 0; r < 200; r++) {
		await thread.refresh();
		await flush();
		if (!thread.hasNewer) break;
		await thread.loadNewer();
	}
	await thread.refresh();
	await flush();
	check('settled');
	if (violations.length) throw new Error(violations[0]);
	const m = thread.messages;
	expect(thread.status).toBe('ready');
	expect(thread.hasNewer, 'I3 hasNewer at rest').toBe(false);
	expect(thread.detached, 'I3 detached at rest').toEqual([]);
	expect(thread.items.filter((i) => i.kind === 'pending'), 'I3 pending at rest').toEqual([]);
	if (m.length) {
		expect(m.length, 'I3 length').toBe(server.last - m[0].seq + 1);
		for (const row of m) {
			const s = server.rows[row.seq - 1];
			const fresh = row.seq > server.last - 200;
			if (fresh) expect({ seq: row.seq, body: row.body, deleted: row.deleted }, `I3 row ${row.seq}`).toEqual({ seq: s.seq, body: s.body, deleted: s.deleted });
			else if (row.deleted) expect(s.deleted, `I3 row ${row.seq} deleted only on the server's say`).toBe(true);
		}
	}
	// I8: a thread of the older identity left behind nothing in the window
	thread.dispose();
}

beforeEach(() => {
	for (const f of [h.getConversation, h.listMessages, h.markRead, h.sendMessage, h.deleteMessage]) f.mockReset();
	vi.spyOn(chatStore, 'resync').mockImplementation(() => {});
	vi.spyOn(globalThis.crypto, 'randomUUID').mockImplementation(() => `00000000-0000-4000-8000-${String(++uuid).padStart(12, '0')}` as `${string}-${string}-${string}-${string}-${string}`);
	wire();
});

afterEach(() => {
	vi.restoreAllMocks();
});

describe('window ops (property)', () => {
	it('throws refusals the real predicates recognise', () => {
		expect(isReplyTargetDeletedError(REFUSALS[1]())).toBe(true);
		expect(isReplyTargetNotFoundError(REFUSALS[2]())).toBe(true);
		expect(isRequestTooLargeError(REFUSALS[3]())).toBe(true);
		expect(isNotFoundError(apiErr('not_found'))).toBe(true);
		expect(isReplyTargetDeletedError(REFUSALS[0]())).toBe(false);
	});

	it(`holds I1-I10 over ${ONLY === null ? SEEDS : 1} seeded walks of ${STEPS} steps`, async () => {
		const seeds = ONLY === null ? Array.from({ length: SEEDS }, (_, i) => i + 1) : [ONLY];
		for (const seed of seeds) {
			const trace: string[] = [];
			uuid = 0;
			try {
				await walk(seed, trace);
			} catch (e) {
				thread?.dispose();
				throw new Error(`seed=${seed} (replay: PROP_SEED=${seed})\n${(e as Error).message}\n--- last 40 steps ---\n${trace.slice(-40).join('\n')}`);
			}
		}
	}, 600_000);
});
