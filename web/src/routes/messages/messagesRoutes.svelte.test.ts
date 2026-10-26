// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// TASK-4044: the /messages routes. The layout redirects the signed-out and does
// not redirect a guest; the list page renders the inbox; the /messages/[n]
// thread (PR 3a) says "not found" for a bad number or a 404, shows the window,
// the "N new" divider and the Row H states, and marks read only from its bottom
// sentinel.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { createRawSnippet, tick, flushSync } from 'svelte';

const h = vi.hoisted(() => ({
	getConversation: vi.fn(),
	markRead: vi.fn(),
	listMessages: vi.fn(),
	listConversations: vi.fn(),
	sessions: vi.fn(),
	ensure: vi.fn(),
	authenticated: true,
	listeners: [] as Array<(prev: string) => void>,
	load: vi.fn(),
	goto: vi.fn(),
	// Bumped by a test to stand for an identity change: fences taken earlier fail.
	identityGen: 0,
	chat: { unreadByNumber: {} as Record<number, number> },
	resync: vi.fn()
}));

class NotFound extends Error {
	code = 'not_found';
}

vi.mock('$lib/api/client', () => ({
	api: {
		chat: {
			getConversation: h.getConversation,
			markRead: h.markRead,
			listMessages: h.listMessages,
			listConversations: h.listConversations,
			ensureConversation: h.ensure
		},
		sessions: { list: h.sessions }
	},
	isNotFoundError: (e: unknown) => e instanceof NotFound
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		load: h.load,
		get authenticated() {
			return h.authenticated;
		},
		identityFence: () => {
			const gen = h.identityGen;
			return () => h.identityGen === gen;
		},
		onIdentityChange: (fn: (prev: string) => void) => {
			h.listeners.push(fn);
			return () => {
				const i = h.listeners.indexOf(fn);
				if (i >= 0) h.listeners.splice(i, 1);
			};
		}
	}
}));
vi.mock('$lib/stores/chat.svelte', () => ({
	chatStore: {
		get version() {
			return live.version;
		},
		resync: h.resync,
		get unreadByNumber() {
			return h.chat.unreadByNumber;
		}
	}
}));
vi.mock('$app/navigation', () => ({ goto: h.goto }));

// An IntersectionObserver double: the test fires the callback by hand.
type IOCallback = (e: Array<{ isIntersecting: boolean }>) => void;
let lastIO: { cb: IOCallback } | null = null;
class FakeIO {
	constructor(cb: IOCallback) {
		lastIO = { cb };
	}
	observe() {}
	disconnect() {}
}

// chatStore.version as a rune, so a bump re-runs the thread's refresh effect.
// Read lazily by the mock above, after this line has run.
const live = $state({ version: 0 });

import MessagesLayout from './+layout.svelte';
import MessagesPage from './+page.svelte';
import ThreadPage from './[n]/+page.svelte';
import { page } from '$app/state';
import { lastWorkspace } from '$lib/stores/lastWorkspace.svelte';
import { chatInbox } from '$lib/stores/chatInbox.svelte';

// jsdom reports the document as not visible; the presence poll only runs while it is.
Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'visible' });

const child = createRawSnippet(() => ({ render: () => `<div data-testid="child">child</div>` }));

async function settle() {
	for (let i = 0; i < 10; i++) {
		await Promise.resolve();
		await tick();
		flushSync();
	}
}

// The shape the server sends; mine is false for an agent (X-Pad-Agent), true for the human.
const message = (seq: number, over: Record<string, unknown> = {}) => ({
	id: `m${seq}`,
	conversation_id: 'c3',
	seq,
	author_agent_key: 'agent-3',
	author_is_human: false,
	mine: false,
	source: 'cli',
	body: `body ${seq}`,
	created_at: '2026-10-26T12:00:00.000000000Z',
	deleted: false,
	...over
});
const pageOf = (messages: ReturnType<typeof message>[], over: Record<string, unknown> = {}) => ({
	messages,
	oldest_seq: messages[0]?.seq ?? 0,
	newest_seq: messages.at(-1)?.seq ?? 0,
	last_seq: messages.at(-1)?.seq ?? 0,
	read_seq: 0,
	has_more: false,
	refs: [],
	refs_truncated: false,
	...over
});

const conv = (n: number) => ({
	id: `c${n}`,
	number: n,
	kind: 'direct',
	title: '',
	agent_key: `agent-${n}`,
	last_seq: 0,
	created_at: '',
	updated_at: '',
	read_seq: 0,
	unread: 0
});

beforeEach(() => {
	for (const f of Object.values(h)) if (typeof f === 'function' && 'mockReset' in f) (f as ReturnType<typeof vi.fn>).mockReset();
	h.authenticated = true;
	h.chat.unreadByNumber = {};
	h.load.mockResolvedValue({ authenticated: true });
	h.listConversations.mockResolvedValue({ conversations: [conv(3)], unread_total: 0 });
	h.getConversation.mockResolvedValue(conv(3));
	h.listMessages.mockResolvedValue(pageOf([]));
	h.sessions.mockResolvedValue({ sessions: [], count: 0 });
	page.params = {};
	lastWorkspace.clear();
	// The inbox is a singleton: start each test from a cold one.
	chatInbox.conversations = [];
	chatInbox.loaded = false;
	chatInbox.failed = false;
	chatInbox.sessions = null;
	chatInbox.checkedAt = null;
	chatInbox.ensuring = new Set();
	chatInbox.rateLimited = {};
	chatInbox.ensureFailed = {};
});

afterEach(() => {
	cleanup();
	// cleanup() unmounts every page, which releases its presence owner; an armed
	// poll after it is a leaked owner (fake timers only: real ones cannot be counted).
	if (vi.isFakeTimers()) expect(vi.getTimerCount(), 'a page left a presence owner started').toBe(0);
	chatInbox.stopPresence();
	vi.useRealTimers();
	page.params = {};
});

describe('/messages layout (TASK-4044)', () => {
	it('renders its children once signed in', async () => {
		const { queryByTestId } = render(MessagesLayout, { props: { children: child } });
		await settle();
		expect(queryByTestId('child')).not.toBeNull();
		expect(h.goto).not.toHaveBeenCalled();
	});

	it('sends a signed-out visitor to /login and renders nothing', async () => {
		h.load.mockResolvedValue({ authenticated: false });
		const { queryByTestId } = render(MessagesLayout, { props: { children: child } });
		await settle();
		expect(h.goto).toHaveBeenCalledWith('/login', { replaceState: true });
		expect(queryByTestId('child')).toBeNull();
	});

	it('sends the visitor to /login when the session probe fails', async () => {
		h.load.mockRejectedValue(new Error('down'));
		const { queryByTestId } = render(MessagesLayout, { props: { children: child } });
		await settle();
		expect(h.goto).toHaveBeenCalledWith('/login', { replaceState: true });
		expect(queryByTestId('child')).toBeNull();
	});

	it('does NOT redirect a guest: only the signed-out are sent away', async () => {
		h.load.mockResolvedValue({ authenticated: true, user: { is_guest: true } });
		const { queryByTestId } = render(MessagesLayout, { props: { children: child } });
		await settle();
		expect(h.goto).not.toHaveBeenCalled();
		expect(queryByTestId('child')).not.toBeNull();
	});
});

describe('/messages page (TASK-4044)', () => {
	it('lists the conversations', async () => {
		const { container, getByText } = render(MessagesPage);
		await settle();
		expect(getByText('Chat')).toBeTruthy();
		expect(container.querySelector('a[href="/messages/3"]')).not.toBeNull();
	});

	it('says so when the first load fails', async () => {
		h.listConversations.mockRejectedValue(new Error('boom'));
		const { getByText } = render(MessagesPage);
		await settle();
		expect(getByText('Could not load your conversations.')).toBeTruthy();
	});

	// Nothing else retries a failed list while the unread fetch is fine (r1).
	it('Try again reloads the list once and shows it', async () => {
		h.listConversations.mockRejectedValueOnce(new Error('boom'));
		const { container, getByText } = render(MessagesPage);
		await settle();
		expect(h.listConversations).toHaveBeenCalledTimes(1);
		getByText('Try again').click();
		await settle();
		expect(h.listConversations).toHaveBeenCalledTimes(2);
		expect(container.querySelector('a[href="/messages/3"]')).not.toBeNull();
		expect(container.textContent).not.toContain('Could not load');
	});

	// A failed reload keeps the last list, and must say so (r2).
	it('a failed reload keeps the list, says so, and Try again clears it', async () => {
		const { container, getByText } = render(MessagesPage);
		await settle();
		h.listConversations.mockRejectedValueOnce(new Error('boom'));
		await chatInbox.load();
		await settle();
		expect(container.querySelector('a[href="/messages/3"]')).not.toBeNull();
		expect(container.textContent).toContain("Couldn't refresh your conversations.");
		getByText('Try again').click();
		await settle();
		expect(container.querySelector('a[href="/messages/3"]')).not.toBeNull();
		expect(container.textContent).not.toContain("Couldn't refresh");
	});

	it('Back goes to /console when no workspace is remembered', async () => {
		const { getByText } = render(MessagesPage);
		await settle();
		expect(getByText('Back').getAttribute('href')).toBe('/console');
	});

	it('Back returns to the remembered workspace', async () => {
		lastWorkspace.note('alice', 'proj');
		const { getByText } = render(MessagesPage);
		await settle();
		const a = getByText('Back to proj');
		expect(a.getAttribute('href')).toBe('/alice/proj');
	});
});

// Row J (TASK-4044 2b-ii): an identity change to signed-out leaves the page.
describe('/messages layout identity change (TASK-4044 2b-ii)', () => {
	async function mountAndChange(authenticated: boolean) {
		render(MessagesLayout, { props: { children: child } });
		await settle();
		h.goto.mockClear();
		h.authenticated = authenticated;
		for (const fn of [...h.listeners]) fn('previous-user');
	}

	it('redirects to /login when the new identity is signed out', async () => {
		await mountAndChange(false);
		expect(h.goto).toHaveBeenCalledWith('/login', { replaceState: true });
	});

	it('does not redirect on a swap to another signed-in user', async () => {
		await mountAndChange(true);
		expect(h.goto).not.toHaveBeenCalled();
	});

	it('stops listening when the layout unmounts', async () => {
		const base = h.listeners.length;
		const { unmount } = render(MessagesLayout, { props: { children: child } });
		await settle();
		expect(h.listeners.length).toBe(base + 1);
		unmount();
		expect(h.listeners.length).toBe(base);
	});
});

describe('/messages roster (TASK-4044 2b-ii)', () => {
	const live = (agent: string) => ({ id: `s-${agent}`, agent, state: 'online', armed: true, connected_at: '' });
	const ghostButton = (c: HTMLElement) => [...c.querySelectorAll('button')].find((b) => b.textContent?.includes('ghost')) as HTMLButtonElement;

	beforeEach(() => {
		h.sessions.mockResolvedValue({ sessions: [live('ghost')], count: 1 });
	});

	it('shows a link with the store unread badge for a DM, and a Start chat button for a session-only agent', async () => {
		h.chat.unreadByNumber = { 3: 5 };
		const { container } = render(MessagesPage);
		await settle();
		const link = container.querySelector('ul[aria-label="Agents"] a[href="/messages/3"]')!;
		expect(link.querySelector('.unread-badge')!.textContent).toBe('5');
		expect(link.querySelector('.presence-dot')).not.toBeNull();
		const btn = ghostButton(container);
		expect(btn.textContent).toContain('Start chat');
		expect(btn.disabled).toBe(false);
	});

	it('goes to the new chat only after the POST resolves, and not on a failure', async () => {
		let resolve!: (v: unknown) => void;
		h.ensure.mockReturnValue(new Promise((r) => (resolve = r)));
		const { container } = render(MessagesPage);
		await settle();
		ghostButton(container).click();
		await settle();
		expect(h.goto).not.toHaveBeenCalled();
		resolve({ conversation: conv(7), created: true });
		await settle();
		expect(h.goto).toHaveBeenCalledWith('/messages/7');
	});

	it('does not navigate when the page was left before the POST resolved', async () => {
		let resolve!: (v: unknown) => void;
		h.ensure.mockReturnValue(new Promise((r) => (resolve = r)));
		const { container, unmount } = render(MessagesPage);
		await settle();
		ghostButton(container).click();
		await settle();
		unmount();
		resolve({ conversation: conv(7), created: true });
		await settle();
		expect(h.goto).not.toHaveBeenCalled();
	});

	it('does not navigate when the ensure fails', async () => {
		h.ensure.mockRejectedValue(new Error('500'));
		const { container } = render(MessagesPage);
		await settle();
		ghostButton(container).click();
		await settle();
		expect(h.goto).not.toHaveBeenCalled();
		expect(container.textContent).toContain("Couldn't start the chat. Try again.");
		expect(ghostButton(container).disabled).toBe(false);
	});

	it('a double click posts once and the button is disabled meanwhile', async () => {
		h.ensure.mockReturnValue(new Promise(() => {}));
		const { container } = render(MessagesPage);
		await settle();
		const btn = ghostButton(container);
		btn.click();
		await settle();
		expect(btn.disabled).toBe(true);
		btn.click();
		await settle();
		expect(h.ensure).toHaveBeenCalledTimes(1);
	});

	// K3: the button must come back in the DOM once the ensure settles.
	it('re-enables the button in the DOM after a settled ensure', async () => {
		let resolve!: (v: unknown) => void;
		h.ensure.mockReturnValue(new Promise((r) => (resolve = r)));
		const { container } = render(MessagesPage);
		await settle();
		ghostButton(container).click();
		await settle();
		expect(ghostButton(container).disabled).toBe(true);
		h.ensure.mockReset();
		resolve({ conversation: conv(7), created: true });
		await settle();
		expect(ghostButton(container).disabled).toBe(false);
	});

	it('a 429 shows the copy and keeps the button off until the cool-down ends', async () => {
		vi.useFakeTimers();
		h.ensure.mockRejectedValue(Object.assign(new Error('rl'), { code: 'rate_limited' }));
		const { container } = render(MessagesPage);
		await settle();
		ghostButton(container).click();
		await settle();
		expect(container.textContent).toContain('Too many new chats right now. Try again in a few seconds.');
		expect(ghostButton(container).disabled).toBe(true);
		await vi.advanceTimersByTimeAsync(5000);
		await settle();
		expect(ghostButton(container).disabled).toBe(false);
		expect(container.textContent).not.toContain('Too many new chats');
	});
});

describe('presence ref count across a navigation (TASK-4044 2b-ii)', () => {
	it.each([
		['start before stop', true],
		['stop before start', false]
	])('/messages to /messages/[n], %s: one timer, one poll per tick', async (_n, startFirst) => {
		vi.useFakeTimers();
		page.params = { n: '3' };
		const list = render(MessagesPage);
		await settle();
		expect(vi.getTimerCount()).toBe(1);
		let thread!: ReturnType<typeof render>;
		if (startFirst) {
			thread = render(ThreadPage);
			list.unmount();
		} else {
			list.unmount();
			thread = render(ThreadPage);
		}
		await settle();
		expect(vi.getTimerCount()).toBe(1);
		const before = h.sessions.mock.calls.length;
		await vi.advanceTimersByTimeAsync(10_000);
		expect(h.sessions.mock.calls.length - before).toBe(1);
		thread.unmount();
		expect(vi.getTimerCount()).toBe(0);
	});
});

describe('/messages/[n] thread (TASK-4044 PR 3a)', () => {
	it('names the agent and shows the window in seq order', async () => {
		page.params = { n: '3' };
		h.getConversation.mockResolvedValue({ ...conv(3), last_seq: 2, read_seq: 2, unread: 0 });
		h.listMessages.mockResolvedValue(
			pageOf([message(1), message(2, { mine: true, author_agent_key: '', author_is_human: true })], {
				read_seq: 2
			})
		);
		const { container } = render(ThreadPage);
		await settle();
		expect(h.getConversation).toHaveBeenCalledWith(3);
		expect(container.textContent).toContain('agent-3');
		const rows = [...container.querySelectorAll('article')];
		expect(rows.map((r) => r.getAttribute('data-seq'))).toEqual(['1', '2']);
		expect(rows[1].classList.contains('mine')).toBe(true);
		expect(rows[1].textContent).toContain('You');
		expect(container.querySelector('#unread-divider')).toBeNull();
		expect(container.textContent).not.toContain('Jump to unread');
	});

	it.each(['abc', '0', '-1', '1.5', '3x', ''])('"%s" is not a conversation number: not found, no request', async (raw) => {
		page.params = { n: raw };
		const { container } = render(ThreadPage);
		await settle();
		expect(container.textContent).toContain('Conversation not found');
		expect(h.getConversation).not.toHaveBeenCalled();
	});

	it('a 404 shows not found', async () => {
		page.params = { n: '99' };
		h.getConversation.mockRejectedValue(new NotFound('nope'));
		const { container } = render(ThreadPage);
		await settle();
		expect(container.textContent).toContain('Conversation not found');
		
	});

	it('another failure is not reported as not found', async () => {
		page.params = { n: '3' };
		h.getConversation.mockRejectedValue(new Error('500'));
		const { container } = render(ThreadPage);
		await settle();
		expect(container.textContent).toContain('Could not load this conversation');
		expect(container.textContent).not.toContain('Conversation not found');
		expect(container.textContent).toContain('Try again');
	});

	it('shows a presence dot for the agent in the header', async () => {
		page.params = { n: '3' };
		h.sessions.mockResolvedValue({
			sessions: [{ id: 's', agent: 'agent-3', state: 'online', armed: true, connected_at: '' }],
			count: 1
		});
		const { container } = render(ThreadPage);
		await settle();
		const dot = container.querySelector('header .presence-dot');
		expect(dot).not.toBeNull();
		expect(dot!.getAttribute('aria-label')).toBe('Online');
	});

	it('the dot reads unknown until the first sessions read lands', async () => {
		page.params = { n: '3' };
		h.sessions.mockReturnValue(new Promise(() => {}));
		const { container } = render(ThreadPage);
		await settle();
		expect(container.querySelector('header .presence-dot')!.getAttribute('aria-label')).toBe('Status unknown');
	});

	// Row H (TASK-4044 2b-i): a failed load needs a way back that is not a reload.
	it('Try again refetches once after a failure and shows the conversation', async () => {
		page.params = { n: '3' };
		h.getConversation.mockRejectedValueOnce(new Error('500'));
		const { container, getByText } = render(ThreadPage);
		await settle();
		expect(container.textContent).toContain('Could not load this conversation');
		expect(h.getConversation).toHaveBeenCalledTimes(1);
		getByText('Try again').click();
		await settle();
		expect(h.getConversation).toHaveBeenCalledTimes(2);
		expect(container.textContent).not.toContain('Could not load');
		expect(container.textContent).toContain('agent-3');
	});

	it('a Try again answer that outlives its identity is dropped', async () => {
		page.params = { n: '3' };
		h.getConversation.mockRejectedValueOnce(new Error('500'));
		const { container, getByText } = render(ThreadPage);
		await settle();
		let resolve!: (c: ReturnType<typeof conv>) => void;
		h.getConversation.mockReturnValue(new Promise((r) => (resolve = r)));
		getByText('Try again').click();
		await settle();
		h.identityGen++;
		resolve(conv(3));
		await settle();
		expect(container.textContent).not.toContain('agent-3');
	});

	it('leaving the page stops the presence poll', async () => {
		page.params = { n: '3' };
		const { unmount } = render(ThreadPage);
		await settle();
		expect(h.sessions).toHaveBeenCalledTimes(1);
		unmount();
		document.dispatchEvent(new Event('visibilitychange'));
		await settle();
		expect(h.sessions).toHaveBeenCalledTimes(1);
	});

	it('drops an answer that outlives its identity', async () => {
		page.params = { n: '3' };
		let resolve!: (c: ReturnType<typeof conv>) => void;
		h.getConversation.mockReturnValue(new Promise((r) => (resolve = r)));
		const { container } = render(ThreadPage);
		await settle();
		h.identityGen++;
		resolve(conv(3));
		await settle();
		expect(container.textContent).not.toContain('agent-3');
	});

	// U8: the divider is where the reader left off, and the chip scrolls to it.
	describe('with unread', () => {
		let scrolled: Element[];
		beforeEach(() => {
			scrolled = [];
			lastIO = null;
			Element.prototype.scrollIntoView = function (this: Element) {
				scrolled.push(this);
			};
			vi.stubGlobal('IntersectionObserver', FakeIO);
			page.params = { n: '3' };
			h.getConversation.mockResolvedValue({ ...conv(3), last_seq: 4, read_seq: 2, unread: 2 });
			h.listMessages.mockResolvedValue(pageOf([message(3), message(4)], { read_seq: 2 }));
			h.markRead.mockResolvedValue({ read_seq: 4 });
		});
		afterEach(() => {
			vi.unstubAllGlobals();
			delete (Element.prototype as { scrollIntoView?: unknown }).scrollIntoView;
		});

		it('shows "2 new" before the first unread row and scrolls to it on open and on Jump to unread', async () => {
			const { container, getByText } = render(ThreadPage);
			await settle();
			expect(h.listMessages).toHaveBeenCalledWith(3, { after_seq: 2, limit: 200 });
			const divider = container.querySelector('#unread-divider')!;
			expect(divider.textContent).toContain('2 new');
			expect(divider.nextElementSibling!.getAttribute('data-seq')).toBe('3');
			expect(scrolled).toContain(divider);
			scrolled.length = 0;
			getByText('Jump to unread').click();
			expect(scrolled).toEqual([divider]);
		});

		it('marks read only once the bottom sentinel is in view, with the displayed seq', async () => {
			render(ThreadPage);
			await settle();
			expect(h.markRead).not.toHaveBeenCalled();
			lastIO!.cb([{ isIntersecting: true }]);
			await settle();
			expect(h.markRead).toHaveBeenCalledWith(3, 4);
			expect(h.resync).toHaveBeenCalledTimes(1);
		});

		it('keeps the divider after the read marker advances', async () => {
			const { container } = render(ThreadPage);
			await settle();
			lastIO!.cb([{ isIntersecting: true }]);
			await settle();
			expect(container.querySelector('#unread-divider')!.textContent).toContain('2 new');
		});
	});

	it('Load earlier appears when older rows exist and a failure keeps the window with a retry', async () => {
		page.params = { n: '3' };
		h.getConversation.mockResolvedValue({ ...conv(3), last_seq: 60, read_seq: 60, unread: 0 });
		h.listMessages.mockResolvedValueOnce(pageOf([message(59), message(60)], { read_seq: 60, last_seq: 60 }));
		const { container, getByText } = render(ThreadPage);
		await settle();
		h.listMessages.mockRejectedValueOnce(new Error('500'));
		getByText('Load earlier').click();
		await settle();
		expect(container.querySelectorAll('article')).toHaveLength(2);
		expect(container.textContent).toContain("Couldn't load earlier messages");
		h.listMessages.mockResolvedValueOnce(pageOf([message(58)], { read_seq: 60, last_seq: 60 }));
		getByText('Load earlier').click();
		await settle();
		expect(container.querySelectorAll('article')).toHaveLength(3);
		expect(container.textContent).not.toContain("Couldn't load earlier");
	});

	it('a failed refresh keeps the rows and shows Try again', async () => {
		page.params = { n: '3' };
		h.getConversation.mockResolvedValue({ ...conv(3), last_seq: 1, read_seq: 1, unread: 0 });
		h.listMessages.mockResolvedValueOnce(pageOf([message(1)], { read_seq: 1 }));
		const { container, getByText } = render(ThreadPage);
		await settle();
		h.listMessages.mockRejectedValueOnce(new Error('500'));
		live.version++;
		await settle();
		expect(container.textContent).toContain("Couldn't refresh this conversation");
		expect(container.querySelectorAll('article')).toHaveLength(1);
		h.listMessages.mockResolvedValue(pageOf([message(1)], { read_seq: 1 }));
		getByText('Try again').click();
		await settle();
		expect(container.textContent).not.toContain("Couldn't refresh");
	});

	it('a 404 offers no Try again', async () => {
		page.params = { n: '99' };
		h.getConversation.mockRejectedValue(new NotFound('nope'));
		const { container } = render(ThreadPage);
		await settle();
		expect(container.textContent).toContain('Conversation not found');
		expect([...container.querySelectorAll('button')].some((b) => b.textContent === 'Try again')).toBe(false);
	});

	// U19: busy is never certain.
	it('a busy agent reads "May be working", never "Online"', async () => {
		page.params = { n: '3' };
		h.sessions.mockResolvedValue({
			sessions: [{ id: 's', agent: 'agent-3', state: 'busy', armed: false, connected_at: '' }],
			count: 1
		});
		const { container } = render(ThreadPage);
		await settle();
		expect(container.querySelector('header .presence-dot')!.getAttribute('aria-label')).toBe('May be working');
	});
});
