// TASK-4315 PR 4: the phone Chat surface. ChatHost renders the same inbox / thread /
// off body inside a full-height DockedSheet above the bottom bar when uiStore.isMobile.
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
	onNavigate: vi.fn(),
	chatOn: true
}));

class NotFound extends Error {
	code = 'not_found';
}

vi.mock('$app/state', async () => await import('../../../test/mocks/app-state-reactive.svelte'));
vi.mock('$app/navigation', () => ({
	goto: h.goto,
	beforeNavigate: () => {},
	afterNavigate: () => {},
	onNavigate: h.onNavigate
}));
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
// `lastIO` is the bottom sentinel's observer; the unread divider has its own.
class FakeIO {
	cb: IOCallback;
	constructor(cb: IOCallback) {
		this.cb = cb;
	}
	observe(el: Element) {
		if (el.classList.contains('sentinel')) lastIO = { cb: this.cb };
	}
	disconnect() {}
}
const live = $state({ chatOn: true });

import { kb } from '$lib/chat/uiKeyboard.fixture.svelte';
vi.mock('$lib/stores/ui.svelte', async (orig) => {
	const actual = await orig<typeof import('$lib/stores/ui.svelte')>();
	return {
		...actual,
		uiStore: new Proxy(actual.uiStore, {
			get: (t, k) =>
				k === 'isMobile' ? kb.mobile : k === 'keyboardVisible' ? kb.visible : Reflect.get(t, k)
		})
	};
});

import ChatHost from './ChatHost.svelte';
import { page } from '$app/state';
import { chatInbox } from '$lib/stores/chatInbox.svelte';
import { coveredPage, __resetCoveredPageForTests } from '$lib/stores/coveredPage.svelte';
import { pushEscapeHandler, ESCAPE_PRIORITY, _resetEscapeStackForTests } from '$lib/stores/escapeStack';
import { uiStore } from '$lib/stores/ui.svelte';
import { chatSurface, __resetChatSurfaceForTests, __checkChatSurfaceForTests } from '$lib/stores/chatSurface.svelte';

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
	for (const f of [h.getConversation, h.markRead, h.listMessages, h.listConversations, h.sessions, h.goto, h.resync, h.onNavigate]) f.mockReset();
	live.chatOn = true;
	kb.mobile = true;
	kb.visible = false;
	h.listConversations.mockResolvedValue({ conversations: [conv(3)], unread_total: 0 });
	h.getConversation.mockResolvedValue(conv(3, { last_seq: 4, read_seq: 2, unread: 2 }));
	h.listMessages.mockResolvedValue(pageOf([msg(3), msg(4)]));
	h.markRead.mockResolvedValue({ read_seq: 4 });
	h.sessions.mockResolvedValue({ sessions: [], count: 0 });
	page.url = new URL('http://localhost/alice/ws/tasks');
	page.shallow = null;
	page.state = {};
	chatInbox.conversations = [];
	chatInbox.loaded = false;
	chatInbox.failed = false;
	chatInbox.sessions = null;
	chatInbox.checkedAt = null;
	setWidth(390);
	vi.stubGlobal('IntersectionObserver', FakeIO);
	lastIO = null;
	Element.prototype.scrollIntoView = function () {};
});

afterEach(() => {
	cleanup();
	kb.mobile = false;
	kb.visible = false;
	__resetCoveredPageForTests();
	_resetEscapeStackForTests();
	__resetChatSurfaceForTests();
	chatInbox.stopPresence();
	vi.unstubAllGlobals();
	vi.restoreAllMocks();
	document.body.style.removeProperty('--chat-dock-width');
});

const sheet = (c: HTMLElement) => c.querySelector<HTMLElement>('.ds-panel');

describe('the phone Chat sheet', () => {
	it('is a DockedSheet labelled Chat holding exactly one view, with no dock or drawer geometry', async () => {
		openAt('/messages');
		const { container } = render(ChatHost);
		await settle();
		const p = sheet(container)!;
		expect(p.getAttribute('role')).toBe('dialog');
		expect(p.getAttribute('aria-label')).toBe('Chat');
		expect(p.classList.contains('ds-fill')).toBe(true);
		expect(container.querySelector('.ds-content.ds-own-scroller')).not.toBeNull();
		expect(container.querySelector('[data-chat-host]')).toBeNull();
		expect(container.querySelector('.chat-backdrop')).toBeNull();
		expect(container.querySelectorAll('ul.conversation-list').length).toBe(1);
		expect(document.body.style.getPropertyValue('--chat-dock-width')).toBe('');
	});

	it('does not dock on a phone, however wide the window reports', async () => {
		setWidth(1600);
		openAt('/messages');
		const { container } = render(ChatHost);
		await settle();
		expect(container.querySelector('[role="complementary"]')).toBeNull();
		expect(sheet(container)).not.toBeNull();
		expect(document.body.style.getPropertyValue('--chat-dock-width')).toBe('');
	});

	it('a closed host still renders nothing', async () => {
		const { container } = render(ChatHost);
		await settle();
		expect(container.innerHTML.replace(/<!--.*?-->/g, '').trim()).toBe('');
	});

	it('says Chat is off, with a Close Chat button, when the setting is off', async () => {
		live.chatOn = false;
		openAt('/messages');
		const { container } = render(ChatHost);
		await settle();
		expect(container.textContent).toContain('Close Chat');
		expect(container.querySelector('ul.conversation-list')).toBeNull();
	});

	it('the sheet backdrop asks the surface to close', async () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		openAt('/messages');
		const { container } = render(ChatHost);
		await settle();
		container.querySelector<HTMLElement>('.ds-backdrop')!.click();
		expect(go).toHaveBeenCalledWith(-1);
	});

	it('a thread marks read under the sheet\'s own cover', async () => {
		openAt('/messages/3');
		render(ChatHost);
		await settle();
		expect(coveredPage.count).toBe(1);
		lastIO!.cb([{ isIntersecting: true }]);
		await settle();
		expect(h.markRead).toHaveBeenCalledWith(3, 4);
	});

	it('a thread under a second overlay never marks read, and does once uncovered', async () => {
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

	it('the sheet covers the page but keeps the bottom nav live', async () => {
		openAt('/messages');
		render(ChatHost);
		await settle();
		expect(coveredPage.active).toBe(true);
		expect(coveredPage.keepsBottomNav).toBe(true);
		page.shallow = null;
		await settle();
		expect(coveredPage.active).toBe(false);
	});

	it('keeps the draft when the window crosses to a wider layout and back', async () => {
		openAt('/messages/3');
		const { container } = render(ChatHost);
		await settle();
		const box = container.querySelector<HTMLTextAreaElement>('textarea')!;
		box.value = 'half written';
		box.dispatchEvent(new Event('input', { bubbles: true }));
		await settle();
		kb.mobile = false;
		setWidth(1600);
		await settle();
		expect(container.querySelector('.ds-panel')).toBeNull();
		expect(container.querySelector<HTMLTextAreaElement>('textarea')!.value).toBe('half written');
		kb.mobile = true;
		setWidth(390);
		await settle();
		expect(container.querySelector<HTMLTextAreaElement>('textarea')!.value).toBe('half written');
	});

	it('fits the visual viewport while the keyboard is up', async () => {
		const vv = Object.assign(new EventTarget(), { height: 800, offsetTop: 0 });
		Object.defineProperty(window, 'visualViewport', { configurable: true, value: vv });
		openAt('/messages/3');
		const { container } = render(ChatHost);
		await settle();
		expect(sheet(container)!.style.height).toBe('');
		vv.height = 450;
		kb.visible = true;
		await settle();
		expect(sheet(container)!.style.height).toBe('450px');
		expect(sheet(container)!.style.bottom).toBe('auto');
		kb.visible = false;
		await settle();
		expect(sheet(container)!.style.height).toBe('');
		Reflect.deleteProperty(window, 'visualViewport');
	});
});

describe('Escape in the phone sheet', () => {
	const press = (init: KeyboardEventInit = {}, before?: (e: KeyboardEvent) => void) => {
		const e = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true, ...init });
		before?.(e);
		window.dispatchEvent(e);
		return e;
	};
	async function phone() {
		openAt('/messages', { chatDepth: 1 });
		const r = render(ChatHost);
		await settle();
		return r;
	}

	it('closes on a plain Escape', async () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		await phone();
		press();
		expect(go).toHaveBeenCalledTimes(1);
		expect(go).toHaveBeenCalledWith(-1);
	});

	it('leaves the key to a nested layer that owns it (the message menu)', async () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		await phone();
		const off = pushEscapeHandler(() => true, ESCAPE_PRIORITY.menu);
		press();
		expect(go).not.toHaveBeenCalled();
		off();
		press();
		expect(go).toHaveBeenCalledTimes(1);
	});

	it('leaves the key to an open bottom sheet (the message menu on a phone owns it there)', async () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		await phone();
		const menu = document.createElement('div');
		menu.className = 'bs-sheet';
		document.body.append(menu);
		press();
		expect(go).not.toHaveBeenCalled();
		menu.remove();
		press();
		expect(go).toHaveBeenCalledTimes(1);
	});

	it('ignores an Escape another handler already used', async () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		await phone();
		press({}, (e) => e.preventDefault());
		expect(go).not.toHaveBeenCalled();
	});

	it('ignores the Escape that ends IME composition', async () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		await phone();
		press({ isComposing: true });
		expect(go).not.toHaveBeenCalled();
	});
});

describe('focus around the phone sheet', () => {
	it('returns focus to the opener on close', async () => {
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

	it('falls back to the Chat slot when the opener is gone', async () => {
		const opener = document.createElement('button');
		document.body.append(opener);
		const slot = document.createElement('button');
		slot.setAttribute('data-chat-slot', '');
		document.body.append(slot);
		opener.focus();
		openAt('/messages', { chatDepth: 1 });
		render(ChatHost);
		await settle();
		opener.remove();
		page.shallow = null;
		await settle();
		expect(document.activeElement).toBe(slot);
		slot.remove();
	});

	it('does not steal focus from somewhere else the reader put it', async () => {
		const other = document.createElement('button');
		const slot = document.createElement('button');
		slot.setAttribute('data-chat-slot', '');
		document.body.append(other, slot);
		openAt('/messages', { chatDepth: 1 });
		render(ChatHost);
		await settle();
		page.shallow = null;
		other.focus();
		await settle();
		expect(document.activeElement).toBe(other);
		other.remove();
		slot.remove();
	});
});
