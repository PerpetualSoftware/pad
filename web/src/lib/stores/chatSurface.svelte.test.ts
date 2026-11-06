// TASK-4315 PR 3b: the single owner of "is Chat open, and on what".
// jsdom cannot run shallow routing, so these drive `page.shallow` by hand and
// spy on goto and history.go; the real traversal is proven in e2e/chat-shell.spec.ts.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

const h = vi.hoisted(() => ({ mobile: false, userId: 'u1' as string | null }));
vi.mock('$app/navigation', () => ({ goto: vi.fn(async () => {}) }));
vi.mock('$lib/stores/breakpoint.svelte', () => ({
	viewport: {
		get isMobile() {
			return h.mobile;
		}
	}
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get userId() {
			return h.userId;
		}
	}
}));

import { goto } from '$app/navigation';
import { page } from '$app/state';
import { chatSurface, parseChatPath, parseThreadNumber } from './chatSurface.svelte';

const gotoMock = vi.mocked(goto);
const asShallow = (path: string) =>
	({ url: new URL(`http://localhost${path}`), route: null, params: null }) as never;

function click(init: MouseEventInit = {}): MouseEvent {
	return new MouseEvent('click', { bubbles: true, cancelable: true, button: 0, ...init });
}

beforeEach(() => {
	h.mobile = false;
	h.userId = 'u1';
	page.url = new URL('http://localhost/alice/ws/tasks');
	page.state = {};
	(page as { shallow: unknown }).shallow = null;
	gotoMock.mockClear();
});
afterEach(() => vi.restoreAllMocks());

describe('parseChatPath: the /messages route\'s own rule', () => {
	it.each([
		['/messages', { kind: 'inbox' }],
		['/messages/', { kind: 'inbox' }],
		['/messages/5', { kind: 'thread', n: 5 }],
		['/messages/120', { kind: 'thread', n: 120 }],
		['/messages/abc', { kind: 'notFound' }],
		['/messages/0', { kind: 'notFound' }],
		['/messages/007', { kind: 'notFound' }],
		['/messages/-3', { kind: 'notFound' }]
	])('%s', (path, want) => {
		expect(parseChatPath(path)).toEqual(want);
	});

	it.each(['/messagesfoo', '/messages/1/x', '/console', '/', '/alice/ws/messages/3'])(
		'%s is not Chat',
		(path) => {
			expect(parseChatPath(path)).toBeNull();
		}
	);

	it('parseThreadNumber is the regex the [n] route uses', () => {
		expect(parseThreadNumber('12')).toBe(12);
		expect(parseThreadNumber('012')).toBeNull();
		expect(parseThreadNumber('')).toBeNull();
	});
});

describe('target', () => {
	it('is null with no shallow navigation', () => {
		expect(chatSurface.target).toBeNull();
		expect(chatSurface.isOpen).toBe(false);
	});

	it('reads the shallow URL, not the underlying route', () => {
		(page as { shallow: unknown }).shallow = asShallow('/messages/7');
		expect(chatSurface.target).toEqual({ kind: 'thread', n: 7 });
		expect(chatSurface.isOpen).toBe(true);
	});

	it('a shallow URL that is not Chat is not an open Chat', () => {
		(page as { shallow: unknown }).shallow = asShallow('/console');
		expect(chatSurface.target).toBeNull();
	});

	it('route mode: the underlying URL is /messages*', () => {
		expect(chatSurface.routeMode).toBe(false);
		page.url = new URL('http://localhost/messages/3');
		expect(chatSurface.routeMode).toBe(true);
	});
});

describe('open', () => {
	it('desktop: a shallow goto that keeps the pane state and counts one entry', async () => {
		page.state = { paneDepth: 2, paneOwned: true };
		await chatSurface.open('/messages');
		expect(gotoMock).toHaveBeenCalledWith('/messages', {
			shallow: true,
			state: { paneDepth: 2, paneOwned: true, chatDepth: 1, chatFromInbox: undefined }
		});
	});

	it('choosing a conversation from the open inbox adds an entry and remembers the inbox', async () => {
		(page as { shallow: unknown }).shallow = asShallow('/messages');
		page.state = { paneDepth: 1, chatDepth: 1 };
		await chatSurface.open('/messages/4');
		expect(gotoMock).toHaveBeenCalledWith('/messages/4', {
			shallow: true,
			state: { paneDepth: 1, chatDepth: 2, chatFromInbox: true }
		});
	});

	it('phone: a plain goto, no shallow, no state', async () => {
		h.mobile = true;
		await chatSurface.open('/messages/4');
		expect(gotoMock).toHaveBeenCalledTimes(1);
		expect(gotoMock).toHaveBeenCalledWith('/messages/4');
	});

	it('already in route mode: a plain goto', async () => {
		page.url = new URL('http://localhost/messages');
		await chatSurface.open('/messages/4');
		expect(gotoMock).toHaveBeenCalledWith('/messages/4');
	});

	it('without an identity: a plain goto', async () => {
		h.userId = null;
		await chatSurface.open('/messages');
		expect(gotoMock).toHaveBeenCalledWith('/messages');
	});

	it('opening what is already showing does nothing', async () => {
		(page as { shallow: unknown }).shallow = asShallow('/messages/4');
		page.state = { chatDepth: 1 };
		await chatSurface.open('/messages/4');
		expect(gotoMock).not.toHaveBeenCalled();
	});

	it('a non-Chat href is a plain goto', async () => {
		await chatSurface.open('/console/settings');
		expect(gotoMock).toHaveBeenCalledWith('/console/settings');
	});
});

describe('close and back', () => {
	it('close pops exactly the entries Chat pushed', () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		(page as { shallow: unknown }).shallow = asShallow('/messages/4');
		page.state = { chatDepth: 2, chatFromInbox: true };
		chatSurface.close();
		expect(go).toHaveBeenCalledWith(-2);
	});

	it('close with no recorded depth replaces the entry with the underlying page', () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		(page as { shallow: unknown }).shallow = asShallow('/messages');
		page.state = {};
		chatSurface.close();
		expect(go).not.toHaveBeenCalled();
		expect(gotoMock).toHaveBeenCalledWith('/alice/ws/tasks', { replaceState: true });
	});

	it('close with nothing open does nothing', () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		chatSurface.close();
		expect(go).not.toHaveBeenCalled();
		expect(gotoMock).not.toHaveBeenCalled();
	});

	it('back from a thread opened from the inbox pops one entry', () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		(page as { shallow: unknown }).shallow = asShallow('/messages/4');
		page.state = { chatDepth: 2, chatFromInbox: true };
		chatSurface.back();
		expect(go).toHaveBeenCalledWith(-1);
	});

	it('back from a thread opened directly replaces it with the inbox', () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		(page as { shallow: unknown }).shallow = asShallow('/messages/4');
		page.state = { chatDepth: 1 };
		chatSurface.back();
		expect(go).not.toHaveBeenCalled();
		expect(gotoMock).toHaveBeenCalledWith('/messages', {
			shallow: true,
			replaceState: true,
			state: { chatDepth: 1 }
		});
	});
});

describe('handleLinkClick', () => {
	it('a plain left click opens through the surface and cancels the href', () => {
		const e = click();
		chatSurface.handleLinkClick(e, '/messages/9');
		expect(e.defaultPrevented).toBe(true);
		expect(gotoMock).toHaveBeenCalledWith('/messages/9', expect.objectContaining({ shallow: true }));
	});

	it.each([
		['ctrl', { ctrlKey: true }],
		['meta', { metaKey: true }],
		['shift', { shiftKey: true }],
		['alt', { altKey: true }],
		['middle', { button: 1 }]
	])('a %s click keeps the plain href (direct entry)', (_n, init) => {
		const e = click(init as MouseEventInit);
		chatSurface.handleLinkClick(e, '/messages/9');
		expect(e.defaultPrevented).toBe(false);
		expect(gotoMock).not.toHaveBeenCalled();
	});

	it('an already-handled click is left alone', () => {
		const e = click();
		e.preventDefault();
		chatSurface.handleLinkClick(e, '/messages/9');
		expect(gotoMock).not.toHaveBeenCalled();
	});
});
