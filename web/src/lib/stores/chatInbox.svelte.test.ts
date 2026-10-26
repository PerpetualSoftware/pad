// TASK-4044: the chat inbox list store. It runs against the REAL chatStore, so
// the reload-from-version wiring is what is under test. A fresh module pair per
// test (both are singletons that register identity listeners).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';

const h = vi.hoisted(() => ({
	unread: vi.fn(),
	list: vi.fn(),
	sessions: vi.fn(),
	state: { authenticated: true, epoch: 0, listeners: [] as Array<(prev: string) => void> }
}));

vi.mock('$lib/api/client', () => ({
	api: { chat: { unread: h.unread, listConversations: h.list }, sessions: { list: h.sessions } }
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
import { PRESENCE_POLL_MS, PRESENCE_STALE_MS } from './chatInbox.svelte';

type Chat = (typeof import('./chat.svelte'))['chatStore'];
type Inbox = (typeof import('./chatInbox.svelte'))['chatInbox'];
let chat: Chat;
let inbox: Inbox;

const conv = (n: number) => ({
	id: `c${n}`,
	number: n,
	kind: 'dm',
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
	inbox.stop();
	inbox.stopPresence();
	chat.stop();
	vi.useRealTimers();
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
