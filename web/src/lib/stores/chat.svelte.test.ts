// TASK-4044: the chat unread store behind the access stream's chat hints. A
// fresh module per test (it is a singleton that registers an identity listener).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const h = vi.hoisted(() => ({
	unread: vi.fn(),
	state: { authenticated: true, epoch: 0, listeners: [] as Array<(prev: string) => void> }
}));

vi.mock('$lib/api/client', () => ({ api: { chat: { unread: h.unread } } }));
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

import { CHAT_HINT_DEBOUNCE_MS } from './chat.svelte';

type Store = (typeof import('./chat.svelte'))['chatStore'];
let store: Store;

const resp = (rows: Array<[number, number]>) => ({
	conversations: rows.map(([number, unread]) => ({
		conversation_id: `c${number}`,
		number,
		agent_key: 'a',
		unread,
		last_seq: unread,
		read_seq: 0,
		delivered_seq: 0
	})),
	total: rows.reduce((n, [, u]) => n + u, 0)
});
const notice = { kind: 'chat' as const, conversation_id: 'c1', conversation: 1, message_seq: 1 };

async function flush() {
	for (let i = 0; i < 8; i++) await Promise.resolve();
}

function changeIdentity(authenticated = true) {
	h.state.epoch++;
	h.state.authenticated = authenticated;
	for (const fn of h.state.listeners) fn('previous-user');
}

beforeEach(async () => {
	vi.useFakeTimers();
	h.unread.mockReset();
	h.unread.mockResolvedValue(resp([[1, 2]]));
	Object.assign(h.state, { authenticated: true, epoch: 0, listeners: [] });
	vi.resetModules();
	store = (await import('./chat.svelte')).chatStore;
});

afterEach(() => {
	store.stop();
	vi.useRealTimers();
});

describe('chat store (TASK-4044)', () => {
	it('three hints in a burst give ONE fetch after the debounce', async () => {
		store.onHint(notice);
		await vi.advanceTimersByTimeAsync(CHAT_HINT_DEBOUNCE_MS - 100);
		store.onHint(notice);
		store.onHint(notice);
		await vi.advanceTimersByTimeAsync(CHAT_HINT_DEBOUNCE_MS - 1);
		expect(h.unread).not.toHaveBeenCalled();
		await vi.advanceTimersByTimeAsync(1);
		expect(h.unread).toHaveBeenCalledTimes(1);
	});

	it('takes the counts from the response, and never from the hint', async () => {
		h.unread.mockResolvedValue(resp([[1, 2], [4, 5]]));
		store.onHint({ ...notice, conversation: 9 });
		await vi.advanceTimersByTimeAsync(CHAT_HINT_DEBOUNCE_MS);
		await flush();
		expect(store.unreadTotal).toBe(7);
		expect(store.unreadByNumber).toEqual({ 1: 2, 4: 5 });
	});

	it('a hint during a fetch gives exactly one follow-up', async () => {
		let release!: () => void;
		h.unread.mockImplementationOnce(
			() => new Promise((res) => (release = () => res(resp([[1, 1]]))))
		);
		store.onHint(notice);
		await vi.advanceTimersByTimeAsync(CHAT_HINT_DEBOUNCE_MS);
		expect(h.unread).toHaveBeenCalledTimes(1);

		store.onHint(notice);
		await vi.advanceTimersByTimeAsync(CHAT_HINT_DEBOUNCE_MS);
		store.onHint(notice);
		await vi.advanceTimersByTimeAsync(CHAT_HINT_DEBOUNCE_MS);
		// Still one in flight: nothing concurrent.
		expect(h.unread).toHaveBeenCalledTimes(1);

		release();
		await flush();
		expect(h.unread).toHaveBeenCalledTimes(2);
		await vi.advanceTimersByTimeAsync(CHAT_HINT_DEBOUNCE_MS * 3);
		await flush();
		expect(h.unread).toHaveBeenCalledTimes(2);
		expect(store.unreadTotal).toBe(2);
	});

	it('resync fetches at once, without the debounce', async () => {
		store.resync();
		await flush();
		expect(h.unread).toHaveBeenCalledTimes(1);
		expect(store.unreadTotal).toBe(2);
	});

	it('a resync cancels the hint timer it supersedes', async () => {
		store.onHint(notice);
		store.resync();
		await flush();
		await vi.advanceTimersByTimeAsync(CHAT_HINT_DEBOUNCE_MS * 2);
		expect(h.unread).toHaveBeenCalledTimes(1);
	});

	function inFlightAnswer(rows: Array<[number, number]>) {
		let release!: () => void;
		h.unread.mockImplementationOnce(() => new Promise((res) => (release = () => res(resp(rows)))));
		return () => release();
	}

	it('an identity change clears the counts', async () => {
		store.resync();
		await flush();
		expect(store.unreadTotal).toBe(2);
		expect(store.unreadByNumber).toEqual({ 1: 2 });
		changeIdentity(false);
		expect(store.unreadTotal).toBe(0);
		expect(store.unreadByNumber).toEqual({});
	});

	it('an identity change cancels a pending hint fetch', async () => {
		store.onHint(notice);
		// Signed in as someone else: the timer armed for the previous account
		// must not fetch for this one.
		changeIdentity(true);
		await vi.advanceTimersByTimeAsync(CHAT_HINT_DEBOUNCE_MS * 2);
		expect(h.unread).not.toHaveBeenCalled();
	});

	it('drops the previous account answer that lands after an identity change', async () => {
		const release = inFlightAnswer([[7, 9]]);
		store.resync();
		await flush();
		changeIdentity(true);
		release();
		await flush();
		expect(store.unreadTotal).toBe(0);
		expect(store.unreadByNumber).toEqual({});
	});

	it('drops an answer whose identity fence broke, even with no listener call', async () => {
		const release = inFlightAnswer([[7, 9]]);
		store.resync();
		await flush();
		h.state.epoch++;
		release();
		await flush();
		expect(store.unreadTotal).toBe(0);
	});

	it('drops an answer outdated by the identity listener, even with the fence intact', async () => {
		const release = inFlightAnswer([[7, 9]]);
		store.resync();
		await flush();
		for (const fn of h.state.listeners) fn('previous-user');
		release();
		await flush();
		expect(store.unreadTotal).toBe(0);
	});

	it('keeps the last counts when a fetch fails', async () => {
		store.resync();
		await flush();
		h.unread.mockRejectedValueOnce(new Error('boom'));
		store.resync();
		await flush();
		expect(store.unreadTotal).toBe(2);
		// And the next one still works.
		h.unread.mockResolvedValue(resp([[1, 3]]));
		store.resync();
		await flush();
		expect(store.unreadTotal).toBe(3);
	});

	it('calls subscribers once per hint, until they unsubscribe', async () => {
		const fn = vi.fn();
		const off = store.subscribe(fn);
		store.onHint(notice);
		store.onHint({ ...notice, message_seq: 2 });
		expect(fn).toHaveBeenCalledTimes(2);
		expect(fn).toHaveBeenLastCalledWith({ ...notice, message_seq: 2 });
		off();
		store.onHint(notice);
		expect(fn).toHaveBeenCalledTimes(2);
	});

	it('a subscriber that throws does not cost later subscribers the hint', async () => {
		const err = vi.spyOn(console, 'error').mockImplementation(() => {});
		const bad = vi.fn(() => {
			throw new Error('boom');
		});
		const good = vi.fn();
		store.subscribe(bad);
		store.subscribe(good);
		expect(() => store.onHint(notice)).not.toThrow();
		expect(bad).toHaveBeenCalledTimes(1);
		expect(good).toHaveBeenCalledWith(notice);
		expect(err).toHaveBeenCalled();
		err.mockRestore();
	});

	it('start() fetches once however often it is called, and refetches on focus', async () => {
		store.start();
		store.start();
		store.start();
		await flush();
		expect(h.unread).toHaveBeenCalledTimes(1);
		window.dispatchEvent(new Event('focus'));
		await vi.advanceTimersByTimeAsync(CHAT_HINT_DEBOUNCE_MS);
		await flush();
		expect(h.unread).toHaveBeenCalledTimes(2);
	});

	it('fetches nothing, and notifies no one, before auth', async () => {
		h.state.authenticated = false;
		const fn = vi.fn();
		store.subscribe(fn);
		store.start();
		store.onHint(notice);
		store.resync();
		await vi.advanceTimersByTimeAsync(CHAT_HINT_DEBOUNCE_MS * 2);
		await flush();
		expect(h.unread).not.toHaveBeenCalled();
		expect(fn).not.toHaveBeenCalled();
	});
});
