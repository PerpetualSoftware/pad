// TASK-4315 PR 3b: the Chat surface mounted from the root layout. The real
// inbox/thread stores run against a faked API; `$app/state` is the reactive
// stand-in so the surface opens and closes by flipping page.shallow.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick, flushSync } from 'svelte';

const h = vi.hoisted(() => ({
	getConversation: vi.fn(),
	markRead: vi.fn(),
	listMessages: vi.fn(),
	listConversations: vi.fn(),
	sessions: vi.fn(),
	goto: vi.fn(),
	resync: vi.fn(),
	chatOn: true
}));

class NotFound extends Error {
	code = 'not_found';
}

vi.mock('$app/state', async () => await import('../../../test/mocks/app-state-reactive.svelte'));
vi.mock('$app/navigation', () => ({ goto: h.goto, beforeNavigate: () => {}, afterNavigate: () => {} }));
vi.mock('$lib/api/client', () => ({
	api: {
		chat: {
			getConversation: h.getConversation,
			markRead: h.markRead,
			listMessages: h.listMessages,
			listConversations: h.listConversations
		},
		sessions: { list: h.sessions },
		auth: { setChatEnabled: vi.fn() }
	},
	isNotFoundError: (e: unknown) => e instanceof NotFound,
	isReplyTargetDeletedError: () => false,
	isReplyTargetNotFoundError: () => false
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		userId: 'u1',
		emailVerified: true,
		authenticated: true,
		user: { chat_enabled: true },
		identityFence: () => () => true,
		onIdentityChange: () => () => {}
	}
}));
vi.mock('$lib/stores/chatSetting.svelte', () => ({
	chatSetting: {
		get enabled() {
			return live.chatOn;
		},
		set: vi.fn()
	}
}));
vi.mock('$lib/stores/chat.svelte', () => ({
	chatStore: {
		version: 0,
		unreadTotal: 0,
		resync: h.resync,
		unreadByNumber: {} as Record<number, number>
	}
}));
vi.mock('$lib/stores/confirmDialog.svelte', () => ({ confirmDialog: { request: vi.fn(async () => true) } }));

type IOCallback = (e: Array<{ isIntersecting: boolean }>) => void;
let lastIO: { cb: IOCallback } | null = null;
class FakeIO {
	constructor(cb: IOCallback) {
		lastIO = { cb };
	}
	observe() {}
	disconnect() {}
}
const live = $state({ chatOn: true });

import ChatHost from './ChatHost.svelte';
import { page } from '$app/state';
import { chatInbox } from '$lib/stores/chatInbox.svelte';
import { coveredPage, __resetCoveredPageForTests } from '$lib/stores/coveredPage.svelte';
import { pushEscapeHandler, ESCAPE_PRIORITY, _resetEscapeStackForTests } from '$lib/stores/escapeStack';
import { uiStore } from '$lib/stores/ui.svelte';
import { chatSurface, __resetChatSurfaceForTests } from '$lib/stores/chatSurface.svelte';

Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'visible' });

async function settle() {
	for (let i = 0; i < 10; i++) {
		await Promise.resolve();
		await tick();
		flushSync();
	}
}

function openAt(path: string, state: Record<string, unknown> = { chatDepth: 1 }) {
	page.state = state;
	page.shallow = { url: new URL(`http://localhost${path}`), route: null, params: null };
}

const conv = (n: number, over: Record<string, unknown> = {}) => ({
	id: `c${n}`,
	number: n,
	kind: 'direct',
	title: '',
	agent_key: `agent-${n}`,
	last_seq: 0,
	created_at: '',
	updated_at: '',
	read_seq: 0,
	unread: 0,
	...over
});
const msg = (seq: number) => ({
	id: `m${seq}`,
	conversation_id: 'c3',
	seq,
	author_agent_key: 'agent-3',
	author_is_human: false,
	mine: false,
	source: 'cli',
	body: `body ${seq}`,
	created_at: '2026-10-26T12:00:00.000000000Z',
	deleted: false
});
const pageOf = (messages: ReturnType<typeof msg>[]) => ({
	messages,
	oldest_seq: messages[0]?.seq ?? 0,
	newest_seq: messages.at(-1)?.seq ?? 0,
	last_seq: messages.at(-1)?.seq ?? 0,
	read_seq: 0,
	has_more: false,
	refs: [],
	refs_truncated: false
});

function setWidth(w: number) {
	Object.defineProperty(window, 'innerWidth', { configurable: true, writable: true, value: w });
	window.dispatchEvent(new Event('resize'));
}

beforeEach(() => {
	for (const f of [h.getConversation, h.markRead, h.listMessages, h.listConversations, h.sessions, h.goto, h.resync]) f.mockReset();
	live.chatOn = true;
	h.listConversations.mockResolvedValue({ conversations: [conv(3)], unread_total: 0 });
	h.getConversation.mockResolvedValue(conv(3));
	h.listMessages.mockResolvedValue(pageOf([]));
	h.sessions.mockResolvedValue({ sessions: [], count: 0 });
	page.url = new URL('http://localhost/alice/ws/tasks');
	page.shallow = null;
	page.state = {};
	chatInbox.conversations = [];
	chatInbox.loaded = false;
	chatInbox.failed = false;
	chatInbox.sessions = null;
	chatInbox.checkedAt = null;
	setWidth(1600);
	vi.stubGlobal('IntersectionObserver', FakeIO);
	lastIO = null;
	Element.prototype.scrollIntoView = function () {};
});

afterEach(() => {
	cleanup();
	__resetCoveredPageForTests();
	_resetEscapeStackForTests();
	__resetChatSurfaceForTests();
	chatInbox.stopPresence();
	vi.unstubAllGlobals();
	vi.restoreAllMocks();
});

describe('a closed host', () => {
	it('renders nothing and starts no thread, sentinel, poller or fetch', async () => {
		const start = vi.spyOn(chatInbox, 'start');
		const presence = vi.spyOn(chatInbox, 'startPresence');
		const { container } = render(ChatHost);
		await settle();
		expect(container.innerHTML.replace(/<!--.*?-->/g, '').trim()).toBe('');
		expect(start).not.toHaveBeenCalled();
		expect(presence).not.toHaveBeenCalled();
		for (const f of [h.listConversations, h.sessions, h.getConversation, h.listMessages, h.markRead]) {
			expect(f).not.toHaveBeenCalled();
		}
	});

	it('closing stops everything it started', async () => {
		openAt('/messages');
		const { container } = render(ChatHost);
		await settle();
		expect(h.sessions).toHaveBeenCalled();
		page.shallow = null;
		await settle();
		expect(container.querySelector('[role="complementary"], [role="dialog"]')).toBeNull();
		const calls = h.listConversations.mock.calls.length;
		await settle();
		expect(h.listConversations.mock.calls.length).toBe(calls);
	});
});

describe('the inbox in the host', () => {
	it('docks as a labelled complementary region when there is room', async () => {
		openAt('/messages');
		setWidth(1600);
		const { container } = render(ChatHost);
		await settle();
		const dock = container.querySelector('[role="complementary"]');
		expect(dock).not.toBeNull();
		expect(dock!.getAttribute('aria-label')).toBe('Chat');
		expect(container.querySelector('[role="dialog"]')).toBeNull();
		expect(document.body.style.getPropertyValue('--chat-dock-width')).toBe('440px');
		expect(container.textContent).toContain('Close Chat');
	});

	it('is a drawer, not a dock, when the room after the sidebar is under 440 + 560', async () => {
		openAt('/messages');
		setWidth(1200);
		const { container } = render(ChatHost);
		await settle();
		expect(container.querySelector('[role="dialog"]')).not.toBeNull();
		expect(container.querySelector('[role="complementary"]')).toBeNull();
		expect(document.body.style.getPropertyValue('--chat-dock-width')).toBe('');
	});

	it('the console has no sidebar to subtract', async () => {
		page.url = new URL('http://localhost/console');
		openAt('/messages');
		setWidth(1100);
		const { container } = render(ChatHost);
		await settle();
		expect(container.querySelector('[role="complementary"]')).not.toBeNull();
	});

	it('one presence owner and one inbox owner, across a dock to drawer swap', async () => {
		openAt('/messages');
		const start = vi.spyOn(chatInbox, 'start');
		const presence = vi.spyOn(chatInbox, 'startPresence');
		const stop = vi.spyOn(chatInbox, 'stopPresence');
		const { container } = render(ChatHost);
		await settle();
		setWidth(1100);
		await settle();
		expect(container.querySelector('[role="dialog"]')).not.toBeNull();
		setWidth(1600);
		await settle();
		expect(container.querySelector('[role="complementary"]')).not.toBeNull();
		expect(start).toHaveBeenCalledTimes(1);
		expect(presence).toHaveBeenCalledTimes(1);
		expect(stop).not.toHaveBeenCalled();
		expect(container.querySelectorAll('ul.conversation-list').length).toBe(1);
	});

	it('Close Chat asks the surface to close', async () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		openAt('/messages', { chatDepth: 1 });
		const { getByText } = render(ChatHost);
		await settle();
		getByText('Close Chat').click();
		expect(go).toHaveBeenCalledWith(-1);
	});

	it('says Chat is off, and mounts no view, when the setting is off', async () => {
		live.chatOn = false;
		openAt('/messages');
		const { container } = render(ChatHost);
		await settle();
		expect(container.textContent).toContain('Chat is off');
		expect(h.listConversations).not.toHaveBeenCalled();
		expect(h.sessions).not.toHaveBeenCalled();
	});
});

describe('a thread in the host', () => {
	beforeEach(() => {
		h.getConversation.mockResolvedValue(conv(3, { last_seq: 4, read_seq: 2, unread: 2 }));
		h.listMessages.mockResolvedValue(pageOf([msg(3), msg(4)]));
		h.markRead.mockResolvedValue({ read_seq: 4 });
	});

	it('shows exactly one thread, also across a dock to drawer swap', async () => {
		openAt('/messages/3');
		const { container } = render(ChatHost);
		await settle();
		expect(container.querySelectorAll('.scroller').length).toBe(1);
		setWidth(1100);
		await settle();
		expect(container.querySelectorAll('.scroller').length).toBe(1);
		expect(h.getConversation).toHaveBeenCalledTimes(1);
	});

	it('marks read from the bottom sentinel while shown', async () => {
		openAt('/messages/3');
		render(ChatHost);
		await settle();
		lastIO!.cb([{ isIntersecting: true }]);
		await settle();
		expect(h.markRead).toHaveBeenCalledWith(3, 4);
	});

	it('a thread covered by another overlay never marks read, and does once uncovered', async () => {
		openAt('/messages/3');
		render(ChatHost);
		await settle();
		const leave = coveredPage.enter({ keepBottomNav: false });
		await settle();
		lastIO!.cb([{ isIntersecting: true }]);
		await settle();
		expect(h.markRead).not.toHaveBeenCalled();
		leave();
		await settle();
		expect(h.markRead).toHaveBeenCalledWith(3, 4);
	});

	it('the drawer\'s own covering does not stop its thread marking read', async () => {
		openAt('/messages/3');
		setWidth(1100);
		const { container } = render(ChatHost);
		await settle();
		expect(container.querySelector('[role="dialog"]')).not.toBeNull();
		expect(coveredPage.active).toBe(true);
		lastIO!.cb([{ isIntersecting: true }]);
		await settle();
		expect(h.markRead).toHaveBeenCalledWith(3, 4);
	});

	it('closing the host marks nothing and leaves no thread behind', async () => {
		openAt('/messages/3');
		const { container } = render(ChatHost);
		await settle();
		page.shallow = null;
		await settle();
		expect(container.querySelector('.scroller')).toBeNull();
		expect(h.markRead).not.toHaveBeenCalled();
	});

	it('the All conversations link goes back to the inbox through the surface', async () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		openAt('/messages/3', { chatDepth: 2, chatFromInbox: true });
		const { getByText } = render(ChatHost);
		await settle();
		getByText('All conversations').click();
		expect(go).toHaveBeenCalledWith(-1);
	});
});

describe('Conversation not found', () => {
	it('is the same state for an invalid number and an inaccessible one', async () => {
		openAt('/messages/abc');
		const a = render(ChatHost);
		await settle();
		const invalid = a.container.querySelector('[role="complementary"]')!.innerHTML;
		expect(a.container.textContent).toContain('Conversation not found');
		expect(h.getConversation).not.toHaveBeenCalled();
		cleanup();

		h.getConversation.mockRejectedValue(new NotFound('nope'));
		openAt('/messages/99');
		const b = render(ChatHost);
		await settle();
		const missing = b.container.querySelector('[role="complementary"]')!.innerHTML;
		expect(b.container.textContent).toContain('Conversation not found');
		expect(missing).toBe(invalid);
	});
});

describe('Escape in the drawer', () => {
	const press = (init: KeyboardEventInit = {}, before?: (e: KeyboardEvent) => void) => {
		const e = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true, ...init });
		before?.(e);
		window.dispatchEvent(e);
		return e;
	};

	async function drawer() {
		setWidth(1100);
		openAt('/messages', { chatDepth: 1 });
		const r = render(ChatHost);
		await settle();
		expect(r.container.querySelector('[role="dialog"]')).not.toBeNull();
		return r;
	}

	it('closes the drawer on a plain Escape', async () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		await drawer();
		press();
		expect(go).toHaveBeenCalledWith(-1);
	});

	it('leaves the key to a nested layer that owns it (a portaled menu)', async () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		await drawer();
		const off = pushEscapeHandler(() => true, ESCAPE_PRIORITY.menu);
		press();
		expect(go).not.toHaveBeenCalled();
		off();
		press();
		expect(go).toHaveBeenCalledTimes(1);
	});

	it('ignores an Escape another handler already used', async () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		await drawer();
		press({}, (e) => e.preventDefault());
		expect(go).not.toHaveBeenCalled();
	});

	it('ignores the Escape that ends IME composition', async () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		await drawer();
		press({ isComposing: true });
		expect(go).not.toHaveBeenCalled();
	});
});

describe('Close right after a Back that has landed', () => {
	it('is not held by that Back when the surface returns to the state Back left', async () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		openAt('/messages/3', { chatDepth: 2, chatFromInbox: true });
		render(ChatHost);
		await settle();
		chatSurface.back();
		expect(go).toHaveBeenCalledTimes(1);
		openAt('/messages', { chatDepth: 1 }); // the popstate landed
		await settle();
		openAt('/messages/3', { chatDepth: 2, chatFromInbox: true }); // chosen again
		await settle();
		chatSurface.close();
		expect(go).toHaveBeenCalledTimes(2);
		expect(go).toHaveBeenLastCalledWith(-2);
	});
});

describe('focus in the host', () => {
	it('moves into the new view when the clicked link goes away (inbox to thread)', async () => {
		h.getConversation.mockResolvedValue(conv(3));
		openAt('/messages', { chatDepth: 1 });
		const { container } = render(ChatHost);
		await settle();
		const link = container.querySelector<HTMLAnchorElement>('a[href="/messages/3"]')!;
		link.focus();
		expect(document.activeElement).toBe(link);
		openAt('/messages/3', { chatDepth: 2, chatFromInbox: true });
		await settle();
		const panel = container.querySelector<HTMLElement>('[data-chat-host]')!;
		expect(link.isConnected).toBe(false);
		expect(panel.contains(document.activeElement)).toBe(true);
	});

	it('does not take focus from somewhere else on a view change', async () => {
		openAt('/messages', { chatDepth: 1 });
		const outside = document.createElement('button');
		document.body.append(outside);
		const { container } = render(ChatHost);
		await settle();
		outside.focus();
		openAt('/messages/3', { chatDepth: 2, chatFromInbox: true });
		await settle();
		expect(document.activeElement).toBe(outside);
		expect(container.querySelector('[data-chat-host]')).not.toBeNull();
		outside.remove();
	});

	it('returns focus to the opener on close, and records nothing when the opener is already gone', async () => {
		const opener = document.createElement('button');
		document.body.append(opener);
		opener.focus();
		openAt('/messages', { chatDepth: 1 });
		render(ChatHost);
		await settle();
		page.shallow = null;
		await settle();
		expect(document.activeElement).toBe(opener);
		opener.remove();
	});
});

describe('dock or drawer follows the sidebar', () => {
	it('closing and reopening the sidebar re-decides without a resize', async () => {
		setWidth(1200);
		openAt('/messages', { chatDepth: 1 });
		const { container } = render(ChatHost);
		await settle();
		const wasOpen = uiStore.sidebarOpen;
		if (!wasOpen) uiStore.openSidebar();
		await settle();
		expect(container.querySelector('[role="dialog"]')).not.toBeNull();
		uiStore.closeSidebar();
		await settle();
		expect(container.querySelector('[role="complementary"]')).not.toBeNull();
		uiStore.openSidebar();
		await settle();
		expect(container.querySelector('[role="dialog"]')).not.toBeNull();
		if (!wasOpen) uiStore.closeSidebar();
	});
});
