// TASK-4044: the chat inbox list store. It runs against the REAL chatStore, so
// the reload-from-version wiring is what is under test. A fresh module pair per
// test (both are singletons that register identity listeners).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';

const h = vi.hoisted(() => ({
	unread: vi.fn(),
	list: vi.fn(),
	state: { authenticated: true, epoch: 0, listeners: [] as Array<(prev: string) => void> }
}));

vi.mock('$lib/api/client', () => ({
	api: { chat: { unread: h.unread, listConversations: h.list } }
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

import { CHAT_HINT_DEBOUNCE_MS } from './chat.svelte';

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

function changeIdentity() {
	h.state.epoch++;
	for (const fn of h.state.listeners) fn('previous-user');
}

beforeEach(async () => {
	vi.useFakeTimers();
	h.unread.mockReset();
	h.list.mockReset();
	h.unread.mockResolvedValue({ conversations: [], total: 0 });
	h.list.mockResolvedValue(listResp(1));
	Object.assign(h.state, { authenticated: true, epoch: 0, listeners: [] });
	vi.resetModules();
	chat = (await import('./chat.svelte')).chatStore;
	inbox = (await import('./chatInbox.svelte')).chatInbox;
});

afterEach(() => {
	inbox.stop();
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
});
