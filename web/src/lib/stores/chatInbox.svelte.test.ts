// TASK-4044: the chat inbox list store. It runs against the REAL chatStore, so
// the reload-from-version wiring is what is under test. A fresh module pair per
// test (both are singletons that register identity listeners).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';

const h = vi.hoisted(() => ({
	unread: vi.fn(),
	list: vi.fn(),
	sessions: vi.fn(),
	ensure: vi.fn(),
	state: { authenticated: true, epoch: 0, listeners: [] as Array<(prev: string) => void> }
}));

vi.mock('$lib/api/client', () => ({
	api: {
		chat: { unread: h.unread, listConversations: h.list, ensureConversation: h.ensure },
		sessions: { list: h.sessions } }
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
			return () => {};
		}
	}
}));

import { CHAT_HINT_DEBOUNCE_MS, CHAT_RETRY_MS } from './chat.svelte';
import { ENSURE_COOLDOWN_MS, PRESENCE_POLL_MS, PRESENCE_STALE_MS } from './chatInbox.svelte';

type Chat = (typeof import('./chat.svelte'))['chatStore'];
type Inbox = (typeof import('./chatInbox.svelte'))['chatInbox'];
let chat: Chat;
let inbox: Inbox;

const conv = (n: number) => ({
	id: `c${n}`,
	number: n,
	kind: 'direct',
	title: '',
	agent_key: `a${n}`,
	last_seq: 1,
	created_at: '',
	updated_at: '',
	read_seq: 0,
	unread: 0
});
const listResp = (...ns: number[]) => ({ conversations: ns.map(conv), unread_total: 0 });
const notice = { kind: 'chat' as const, conversation_id: 'c1', conversation: 1, message_seq: 1 };

async function flush() {
	for (let i = 0; i < 10; i++) {
		await Promise.resolve();
		flushSync();
	}
}

let visibility: 'visible' | 'hidden' = 'visible';
Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibility });
function setVisibility(v: 'visible' | 'hidden') {
	visibility = v;
	document.dispatchEvent(new Event('visibilitychange'));
}

const sess = (agent: string, state: 'online' | 'busy' | 'connected_not_accepting' = 'online') => ({
	id: `s-${agent}`,
	agent,
	state,
	armed: state === 'online',
	connected_at: ''
});
const sessResp = (...ss: ReturnType<typeof sess>[]) => ({ sessions: ss, count: ss.length });
const apiErr = (code: string) => Object.assign(new Error(code), { code });

function changeIdentity() {
	h.state.epoch++;
	for (const fn of h.state.listeners) fn('previous-user');
}

beforeEach(async () => {
	vi.useFakeTimers();
	h.unread.mockReset();
	h.list.mockReset();
	h.sessions.mockReset();
	h.ensure.mockReset();
	h.sessions.mockResolvedValue({ sessions: [], count: 0 });
	setVisibility('visible');
	h.unread.mockResolvedValue({ conversations: [], total: 0 });
	h.list.mockResolvedValue(listResp(1));
	Object.assign(h.state, { authenticated: true, epoch: 0, listeners: [] });
	vi.resetModules();
	chat = (await import('./chat.svelte')).chatStore;
	inbox = (await import('./chatInbox.svelte')).chatInbox;
});

afterEach(() => {
	// The list and the unread store hold their own timers: end them first, so
	// the only timer left standing can be a presence poll.
	inbox.stop();
	chat.stop();
	try {
		// startPresence is reference counted. Every test releases the owners it took
		// (the describes that start presence do it in their own afterEach, which runs
		// before this one), so an armed presence poll here is a leaked owner.
		expect(vi.getTimerCount(), 'a test left a presence owner started').toBe(0);
	} finally {
		// One drain for safety only; the assertion above ran first. Its failure must
		// not leave the fake clock behind for the next test.
		inbox.stopPresence();
		vi.useRealTimers();
	}
});

describe('chatInbox (TASK-4044)', () => {
	it('loads the conversations on start', async () => {
		inbox.start();
		await flush();
		expect(h.list).toHaveBeenCalledTimes(1);
		expect(inbox.conversations.map((c) => c.number)).toEqual([1]);
		expect(inbox.loaded).toBe(true);
		expect(inbox.failed).toBe(false);
	});

	it('a second start is a no-op', async () => {
		inbox.start();
		inbox.start();
		await flush();
		expect(h.list).toHaveBeenCalledTimes(1);
	});

	it('one hint followed by a successful run produces exactly ONE list reload', async () => {
		inbox.start();
		await flush();
		expect(h.list).toHaveBeenCalledTimes(1);

		chat.onHint(notice);
		await vi.advanceTimersByTimeAsync(CHAT_HINT_DEBOUNCE_MS + 1);
		await flush();
		expect(h.unread).toHaveBeenCalledTimes(1);
		expect(h.list).toHaveBeenCalledTimes(2);

		// Nothing else is pending.
		await vi.advanceTimersByTimeAsync(5000);
		await flush();
		expect(h.list).toHaveBeenCalledTimes(2);
	});

	it('a burst of hints coalesces into one reload', async () => {
		inbox.start();
		await flush();
		for (let i = 0; i < 5; i++) chat.onHint(notice);
		await vi.advanceTimersByTimeAsync(CHAT_HINT_DEBOUNCE_MS + 1);
		await flush();
		expect(h.list).toHaveBeenCalledTimes(2);
	});

	it('a resync reloads the list too', async () => {
		inbox.start();
		await flush();
		chat.resync();
		await flush();
		expect(h.list).toHaveBeenCalledTimes(2);
	});

	it('a failed unread fetch does not reload the list', async () => {
		inbox.start();
		await flush();
		h.unread.mockRejectedValue(new Error('boom'));
		chat.onHint(notice);
		await vi.advanceTimersByTimeAsync(CHAT_HINT_DEBOUNCE_MS + 1);
		await flush();
		expect(h.unread).toHaveBeenCalledTimes(1);
		expect(h.list).toHaveBeenCalledTimes(1);
	});

	it('keeps the last list when a reload fails, and flags it', async () => {
		inbox.start();
		await flush();
		h.list.mockRejectedValue(new Error('boom'));
		await inbox.load();
		expect(inbox.conversations.map((c) => c.number)).toEqual([1]);
		expect(inbox.failed).toBe(true);
		expect(inbox.loaded).toBe(true);
		h.list.mockResolvedValue(listResp(1, 2));
		await inbox.load();
		expect(inbox.failed).toBe(false);
		expect(inbox.conversations).toHaveLength(2);
	});

	it('an older answer cannot overwrite a newer one', async () => {
		let resolveOld!: (v: unknown) => void;
		h.list.mockImplementationOnce(() => new Promise((r) => (resolveOld = r)));
		const first = inbox.load();
		h.list.mockResolvedValueOnce(listResp(2));
		await inbox.load();
		resolveOld(listResp(1));
		await first;
		expect(inbox.conversations.map((c) => c.number)).toEqual([2]);
	});

	it('an identity change drops the list and an in-flight answer for the old account', async () => {
		inbox.start();
		await flush();
		expect(inbox.conversations).toHaveLength(1);

		let resolveOld!: (v: unknown) => void;
		h.list.mockImplementationOnce(() => new Promise((r) => (resolveOld = r)));
		const inFlight = inbox.load();
		changeIdentity();
		expect(inbox.conversations).toEqual([]);
		expect(inbox.loaded).toBe(false);

		resolveOld(listResp(9));
		await inFlight;
		expect(inbox.conversations).toEqual([]);
		expect(inbox.loaded).toBe(false);
	});

	it('an answer that outlives its identity fence is dropped even before the change is announced', async () => {
		// The fence (authStore.identityFence) flips with the epoch; the listener
		// that bumps the seq may run later. The answer must not land in between.
		let resolveOld!: (v: unknown) => void;
		h.list.mockImplementationOnce(() => new Promise((r) => (resolveOld = r)));
		const inFlight = inbox.load();
		h.state.epoch++;
		resolveOld(listResp(9));
		await inFlight;
		expect(inbox.conversations).toEqual([]);
		expect(inbox.loaded).toBe(false);
	});

	it('does not fetch when signed out', async () => {
		h.state.authenticated = false;
		await inbox.load();
		expect(h.list).not.toHaveBeenCalled();
	});

	it('stop() ends the reloads', async () => {
		inbox.start();
		await flush();
		inbox.stop();
		chat.resync();
		await flush();
		expect(h.list).toHaveBeenCalledTimes(1);
	});

	// CONDITION ROW (TASK-4044 c00bdf74 / 5b0cd91b): the unread fetch after an
	// identity change fails, its retry succeeds, and the list reloads ONCE, from
	// the retry's version bump. The failed fetch bumped nothing.
	it('identity change, a failed unread fetch, then a successful retry: ONE list reload', async () => {
		chat.start();
		inbox.start();
		await flush();
		const before = h.list.mock.calls.length;

		h.unread.mockRejectedValueOnce(new Error('boom'));
		changeIdentity();
		await flush();
		expect(h.unread).toHaveBeenCalledTimes(2);
		expect(h.list.mock.calls.length).toBe(before);

		await vi.advanceTimersByTimeAsync(CHAT_RETRY_MS);
		await flush();
		expect(h.unread).toHaveBeenCalledTimes(3);
		expect(h.list.mock.calls.length).toBe(before + 1);

		await vi.advanceTimersByTimeAsync(CHAT_RETRY_MS * 3);
		await flush();
		expect(h.unread).toHaveBeenCalledTimes(3);
		expect(h.list.mock.calls.length).toBe(before + 1);
	});
});

describe('chatInbox presence (TASK-4044 2b-i)', () => {
	afterEach(() => {
		// These tests start one presence owner each and do not stop it themselves.
		inbox.stopPresence();
	});

	it('startPresence fires no list fetch', async () => {
		inbox.startPresence();
		await flush();
		expect(h.list).not.toHaveBeenCalled();
		expect(h.sessions).toHaveBeenCalledTimes(1);
	});

	it('polls every 10s while visible, and not before', async () => {
		inbox.startPresence();
		await flush();
		expect(h.sessions).toHaveBeenCalledTimes(1);
		await vi.advanceTimersByTimeAsync(PRESENCE_POLL_MS - 1);
		expect(h.sessions).toHaveBeenCalledTimes(1);
		await vi.advanceTimersByTimeAsync(1);
		expect(h.sessions).toHaveBeenCalledTimes(2);
		await vi.advanceTimersByTimeAsync(PRESENCE_POLL_MS);
		expect(h.sessions).toHaveBeenCalledTimes(3);
	});

	it('does not poll while hidden', async () => {
		inbox.startPresence();
		await flush();
		setVisibility('hidden');
		await vi.advanceTimersByTimeAsync(PRESENCE_POLL_MS * 5);
		expect(h.sessions).toHaveBeenCalledTimes(1);
	});

	it('does not poll when started on a hidden tab', async () => {
		setVisibility('hidden');
		inbox.startPresence();
		await vi.advanceTimersByTimeAsync(PRESENCE_POLL_MS * 3);
		expect(h.sessions).not.toHaveBeenCalled();
	});

	it('becoming visible fetches at once and re-arms the poll', async () => {
		setVisibility('hidden');
		inbox.startPresence();
		await flush();
		setVisibility('visible');
		await flush();
		expect(h.sessions).toHaveBeenCalledTimes(1);
		await vi.advanceTimersByTimeAsync(PRESENCE_POLL_MS);
		expect(h.sessions).toHaveBeenCalledTimes(2);
	});

	it('a second startPresence is a no-op', async () => {
		inbox.startPresence();
		inbox.startPresence();
		await flush();
		expect(h.sessions).toHaveBeenCalledTimes(1);
		await vi.advanceTimersByTimeAsync(PRESENCE_POLL_MS);
		expect(h.sessions).toHaveBeenCalledTimes(2);
		// Two owners were taken; the describe's afterEach releases one.
		inbox.stopPresence();
	});

	it('reads online from a matching session, offline only after a successful read without one', async () => {
		expect(inbox.presenceFor('claude')).toBe('unknown');
		h.sessions.mockResolvedValue(sessResp(sess('claude')));
		inbox.startPresence();
		await flush();
		expect(inbox.presenceFor('claude')).toBe('online');
		expect(inbox.presenceFor('other')).toBe('offline');
		expect(inbox.checkedAt).not.toBeNull();
	});

	it('before any read, presenceFor is unknown, never offline', async () => {
		h.sessions.mockReturnValue(new Promise(() => {}));
		inbox.startPresence();
		await flush();
		expect(inbox.sessions).toBeNull();
		expect(inbox.presenceFor('claude')).toBe('unknown');
	});

	it('a 503 reads unknown at once, with no 30s wait', async () => {
		h.sessions.mockResolvedValueOnce(sessResp(sess('claude')));
		inbox.startPresence();
		await flush();
		expect(inbox.presenceFor('claude')).toBe('online');
		h.sessions.mockRejectedValue(apiErr('unavailable'));
		await vi.advanceTimersByTimeAsync(PRESENCE_POLL_MS);
		expect(inbox.presenceFor('claude')).toBe('unknown');
	});

	it('an unauthorized error reads unknown at once', async () => {
		h.sessions.mockResolvedValueOnce(sessResp(sess('claude')));
		inbox.startPresence();
		await flush();
		h.sessions.mockRejectedValue(apiErr('unauthorized'));
		await vi.advanceTimersByTimeAsync(PRESENCE_POLL_MS);
		expect(inbox.presenceFor('claude')).toBe('unknown');
	});

	it('a transient failure keeps the last good read until 30s, then reads unknown', async () => {
		h.sessions.mockResolvedValueOnce(sessResp(sess('claude')));
		inbox.startPresence();
		await flush();
		h.sessions.mockRejectedValue(new Error('network'));
		await vi.advanceTimersByTimeAsync(PRESENCE_STALE_MS - 100);
		setVisibility('hidden');
		setVisibility('visible');
		await flush();
		expect(h.sessions.mock.calls.length).toBeGreaterThan(2);
		expect(inbox.presenceFor('claude')).toBe('online');
		await vi.advanceTimersByTimeAsync(100);
		setVisibility('hidden');
		setVisibility('visible');
		await flush();
		expect(inbox.presenceFor('claude')).toBe('unknown');
	});

	it('with no good read, a transient failure goes unknown 30s after start', async () => {
		h.sessions.mockRejectedValue(new Error('network'));
		inbox.startPresence();
		await vi.advanceTimersByTimeAsync(PRESENCE_STALE_MS - 1);
		expect(inbox.sessions).toBeNull();
		await vi.advanceTimersByTimeAsync(1);
		expect(inbox.sessions).toBe('unavailable');
	});

	it('a success after a failure restores the read', async () => {
		h.sessions.mockRejectedValueOnce(apiErr('unavailable'));
		inbox.startPresence();
		await flush();
		expect(inbox.sessions).toBe('unavailable');
		h.sessions.mockResolvedValue(sessResp(sess('claude', 'busy')));
		await vi.advanceTimersByTimeAsync(PRESENCE_POLL_MS);
		expect(inbox.presenceFor('claude')).toBe('busy_uncertain');
	});

	it('an identity change clears the sessions and drops an in-flight answer', async () => {
		h.sessions.mockResolvedValueOnce(sessResp(sess('claude')));
		inbox.startPresence();
		await flush();
		expect(inbox.sessions).not.toBeNull();
		let resolveOld!: (v: unknown) => void;
		h.sessions.mockImplementationOnce(() => new Promise((r) => (resolveOld = r)));
		await vi.advanceTimersByTimeAsync(PRESENCE_POLL_MS);
		changeIdentity();
		expect(inbox.sessions).toBeNull();
		expect(inbox.checkedAt).toBeNull();
		resolveOld(sessResp(sess('old-user-agent')));
		await flush();
		expect(inbox.sessions).toBeNull();
	});

	it('an answer past its identity fence is dropped before the change is announced', async () => {
		let resolveOld!: (v: unknown) => void;
		h.sessions.mockImplementationOnce(() => new Promise((r) => (resolveOld = r)));
		inbox.startPresence();
		await flush();
		h.state.epoch++;
		resolveOld(sessResp(sess('claude')));
		await flush();
		expect(inbox.sessions).toBeNull();
	});

	it('a slow older poll cannot overwrite a newer one', async () => {
		let resolveOld!: (v: unknown) => void;
		h.sessions.mockImplementationOnce(() => new Promise((r) => (resolveOld = r)));
		inbox.startPresence();
		await flush();
		inbox.stopPresence();
		h.sessions.mockResolvedValue(sessResp(sess('new')));
		inbox.startPresence();
		await flush();
		expect(inbox.presenceFor('new')).toBe('online');
		resolveOld(sessResp(sess('old')));
		await flush();
		expect(inbox.presenceFor('new')).toBe('online');
		expect(inbox.presenceFor('old')).toBe('offline');
	});

	it('stopPresence stops the timer and the listener', async () => {
		inbox.startPresence();
		await flush();
		inbox.stopPresence();
		await vi.advanceTimersByTimeAsync(PRESENCE_POLL_MS * 3);
		setVisibility('hidden');
		setVisibility('visible');
		await flush();
		expect(h.sessions).toHaveBeenCalledTimes(1);
	});

	it('does not poll when signed out', async () => {
		h.state.authenticated = false;
		inbox.startPresence();
		await flush();
		expect(h.sessions).not.toHaveBeenCalled();
	});

	it('exposes presence over the conversation agent keys', async () => {
		h.sessions.mockResolvedValue(sessResp(sess('a1')));
		h.list.mockResolvedValue(listResp(1, 2));
		await inbox.load();
		inbox.startPresence();
		await flush();
		expect(inbox.presences.get('a1')).toBe('online');
		expect(inbox.presences.get('a2')).toBe('offline');
	});
});

// TASK-4044 2b-ii: the roster and ensureDM (ruling 018faf64).
const ensureOk = (n: number, created = true) => ({ conversation: conv(n), created });
const rateErr = (retryAfterMs?: number) =>
	Object.assign(new Error('rate_limited'), { code: 'rate_limited', retryAfterMs });
const rosterKeys = () => inbox.roster.map((r) => r.key);

describe('chatInbox roster (TASK-4044 2b-ii)', () => {
	afterEach(() => {
		// These tests start one presence owner each and do not stop it themselves.
		inbox.stopPresence();
	});

	async function loadWith(convs: ReturnType<typeof conv>[], sessions: ReturnType<typeof sess>[] | 'down') {
		h.list.mockResolvedValue({ conversations: convs, unread_total: 0 });
		if (sessions === 'down') h.sessions.mockRejectedValue(apiErr('unavailable'));
		else h.sessions.mockResolvedValue(sessResp(...sessions));
		inbox.start();
		inbox.startPresence();
		await flush();
	}

	it('orders online, busy, connected-not-accepting, unknown, offline, ties by name', async () => {
		const named = (n: number, key: string) => ({ ...conv(n), agent_key: key });
		await loadWith(
			[named(1, 'zed'), named(2, 'amy'), named(3, 'bob'), named(4, 'cat'), named(5, 'dan'), named(6, 'eve')],
			[
				sess('zed', 'online'),
				sess('amy', 'online'),
				sess('bob', 'busy'),
				sess('cat', 'connected_not_accepting')
			]
		);
		expect(rosterKeys()).toEqual(['amy', 'zed', 'bob', 'cat', 'dan', 'eve']);
		expect(inbox.roster.map((r) => r.presence)).toEqual([
			'online',
			'online',
			'busy_uncertain',
			'connected_not_accepting',
			'offline',
			'offline'
		]);
	});

	it('carries the DM number, and null for a session-only agent', async () => {
		await loadWith([conv(1)], [sess('ghost'), sess('a1')]);
		expect(inbox.roster).toEqual([
			{ key: 'a1', presence: 'online', number: 1 },
			{ key: 'ghost', presence: 'online', number: null }
		]);
	});

	// Row 15: unread belongs to chatStore alone.
	it('carries no unread count', async () => {
		await loadWith([{ ...conv(1), unread: 4 }], []);
		for (const r of inbox.roster) expect(Object.keys(r).sort()).toEqual(['key', 'number', 'presence']);
	});

	// Row 16 / Q-e: while sessions cannot be read, only DM agents are listed, as unknown.
	it('shows session-only agents after a good read and drops them when unavailable', async () => {
		await loadWith([conv(1)], [sess('ghost')]);
		expect(rosterKeys()).toContain('ghost');
		h.sessions.mockRejectedValue(apiErr('unavailable'));
		await vi.advanceTimersByTimeAsync(PRESENCE_POLL_MS);
		await flush();
		expect(rosterKeys()).toEqual(['a1']);
		expect(inbox.roster[0].presence).toBe('unknown');
	});

	it('before the first read the roster is the DM agents as unknown', async () => {
		h.sessions.mockReturnValue(new Promise(() => {}));
		h.list.mockResolvedValue(listResp(1));
		inbox.start();
		inbox.startPresence();
		await flush();
		expect(inbox.roster).toEqual([{ key: 'a1', presence: 'unknown', number: 1 }]);
	});

	// Q3
	it("a group conversation (kind 'group') is not on the roster", async () => {
		await loadWith([conv(1), { ...conv(2), kind: 'group', agent_key: 'notadm' }], []);
		expect(rosterKeys()).toEqual(['a1']);
	});
});

describe('chatInbox ensureDM (TASK-4044 2b-ii)', () => {
	it('returns the number for a created (201) and a reused (200) DM, and reloads each time', async () => {
		h.ensure.mockResolvedValueOnce(ensureOk(7, true)).mockResolvedValueOnce(ensureOk(7, false));
		expect(await inbox.ensureDM('amy')).toBe(7);
		expect(await inbox.ensureDM('amy')).toBe(7);
		expect(h.ensure).toHaveBeenCalledTimes(2);
		expect(h.list).toHaveBeenCalledTimes(2);
		expect(inbox.ensuring.size).toBe(0);
	});

	// Row 7
	it('a double click fires one POST', async () => {
		let resolve!: (v: unknown) => void;
		h.ensure.mockReturnValue(new Promise((r) => (resolve = r)));
		const first = inbox.ensureDM('amy');
		const second = await inbox.ensureDM('amy');
		expect(second).toBeNull();
		expect(h.ensure).toHaveBeenCalledTimes(1);
		resolve(ensureOk(7));
		expect(await first).toBe(7);
	});

	it('an empty key posts nothing', async () => {
		expect(await inbox.ensureDM('  ')).toBeNull();
		expect(h.ensure).not.toHaveBeenCalled();
	});

	// Row 8
	it('a 429 is one POST, never retried, and flagged', async () => {
		h.ensure.mockRejectedValue(rateErr());
		expect(await inbox.ensureDM('amy')).toBeNull();
		expect(inbox.rateLimited['amy']).toBe(true);
		await vi.advanceTimersByTimeAsync(60_000);
		await flush();
		expect(h.ensure).toHaveBeenCalledTimes(1);
	});

	// Row 9
	it.each([
		[undefined, ENSURE_COOLDOWN_MS],
		[1000, ENSURE_COOLDOWN_MS],
		[9000, 9000]
	])('after a 429 with retryAfterMs=%s the key stays off until %dms', async (retryAfterMs, wait) => {
		h.ensure.mockRejectedValue(rateErr(retryAfterMs));
		await inbox.ensureDM('amy');
		await vi.advanceTimersByTimeAsync(wait - 1);
		expect(inbox.rateLimited['amy']).toBe(true);
		expect(await inbox.ensureDM('amy')).toBeNull();
		expect(h.ensure).toHaveBeenCalledTimes(1);
		await vi.advanceTimersByTimeAsync(1);
		expect(inbox.rateLimited['amy']).toBeUndefined();
	});

	// Row 9b
	it('another failure is flagged and allows a retry at once', async () => {
		h.ensure.mockRejectedValueOnce(new Error('500')).mockResolvedValueOnce(ensureOk(7));
		expect(await inbox.ensureDM('amy')).toBeNull();
		expect(inbox.ensureFailed['amy']).toBe(true);
		expect(inbox.rateLimited['amy']).toBeUndefined();
		expect(inbox.ensuring.size).toBe(0);
		expect(await inbox.ensureDM('amy')).toBe(7);
		expect(inbox.ensureFailed['amy']).toBeUndefined();
	});

	// K1
	it('an identity change while the POST is in flight fences it: no load, no number, ensuring cleared', async () => {
		let resolve!: (v: unknown) => void;
		h.ensure.mockReturnValue(new Promise((r) => (resolve = r)));
		const p = inbox.ensureDM('amy');
		await flush();
		expect(inbox.ensuring.has('amy')).toBe(true);
		changeIdentity();
		expect(inbox.ensuring.size).toBe(0);
		resolve(ensureOk(7));
		expect(await p).toBeNull();
		expect(h.list).not.toHaveBeenCalled();
		expect(inbox.ensuring.size).toBe(0);
	});

	it('an identity change while the reload runs also yields null', async () => {
		let resolveList!: (v: unknown) => void;
		h.ensure.mockResolvedValue(ensureOk(7));
		h.list.mockReturnValue(new Promise((r) => (resolveList = r)));
		const p = inbox.ensureDM('amy');
		await flush();
		changeIdentity();
		resolveList(listResp(7));
		expect(await p).toBeNull();
	});

	// K2
	it('an identity change in the cool-down clears the flag and its timer', async () => {
		h.ensure.mockRejectedValue(rateErr());
		await inbox.ensureDM('amy');
		expect(inbox.rateLimited['amy']).toBe(true);
		const timers = vi.getTimerCount();
		changeIdentity();
		expect(inbox.rateLimited['amy']).toBeUndefined();
		expect(vi.getTimerCount()).toBe(timers - 1);
		h.ensure.mockResolvedValue(ensureOk(7));
		expect(await inbox.ensureDM('amy')).toBe(7);
	});
});

describe('chatInbox presence ref count (TASK-4044 2b-ii)', () => {
	const seen = () => h.sessions.mock.calls.length;

	it.each([
		['start before stop', true],
		['stop before start', false]
	])('overlapping owners, %s: one timer, one poll per tick, 0 after the last stop', async (_n, startFirst) => {
		inbox.startPresence();
		await flush();
		expect(vi.getTimerCount()).toBe(1);
		if (startFirst) {
			inbox.startPresence();
			inbox.stopPresence();
		} else {
			inbox.stopPresence();
			inbox.startPresence();
		}
		await flush();
		expect(vi.getTimerCount()).toBe(1);
		const before = seen();
		await vi.advanceTimersByTimeAsync(PRESENCE_POLL_MS);
		expect(seen() - before).toBe(1);
		inbox.stopPresence();
		expect(vi.getTimerCount()).toBe(0);
	});

	it('a stop with no owner is a no-op and does not go negative', async () => {
		inbox.stopPresence();
		inbox.stopPresence();
		inbox.startPresence();
		await flush();
		expect(vi.getTimerCount()).toBe(1);
		inbox.stopPresence();
		expect(vi.getTimerCount()).toBe(0);
	});
});
