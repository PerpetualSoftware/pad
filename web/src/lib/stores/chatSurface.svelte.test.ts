// TASK-4315 PR 3b: the single owner of "is Chat open, and on what".
// jsdom cannot run shallow routing, so these drive `page.shallow` by hand and
// spy on goto and history.go; the real traversal is proven in e2e/chat-shell.spec.ts.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

const h = vi.hoisted(() => ({ mobile: false, userId: 'u1' as string | null, epoch: 0, fenceN: 0, throwAt: -1 }));
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
		},
		identityFence() {
			const at = h.epoch;
			const id = h.fenceN++;
			return () => {
				if (h.throwAt === id) throw new Error('fence boom');
				return h.epoch === at;
			};
		}
	}
}));

import { goto } from '$app/navigation';
import { page } from '$app/state';
import { chatSurface, navigated, STRIP_NEW_KEY, parseChatPath, parseThreadNumber, __resetChatSurfaceForTests, __checkChatSurfaceForTests } from './chatSurface.svelte';

const gotoMock = vi.mocked(goto);
const asShallow = (path: string) =>
	({ url: new URL(`http://localhost${path}`), route: null, params: null }) as never;

function click(init: MouseEventInit = {}): MouseEvent {
	return new MouseEvent('click', { bubbles: true, cancelable: true, button: 0, ...init });
}

beforeEach(() => {
	h.mobile = false;
	h.userId = 'u1';
	h.epoch = 0;
	h.fenceN = 0;
	h.throwAt = -1;
	page.url = new URL('http://localhost/alice/ws/tasks');
	page.state = {};
	(page as { shallow: unknown }).shallow = null;
	gotoMock.mockClear();
	__resetChatSurfaceForTests();
});
afterEach(() => {
	vi.useRealTimers();
	vi.restoreAllMocks();
});

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

	it.each(['/messagesfoo', '/messages/1/x', '/console', '/', '/needs', '/agenda', '/settings/account', '/alice/ws/messages/3'])(
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
		(page as { shallow: unknown }).shallow = asShallow('/');
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

	it('phone: opens as a shallow navigation with a chat depth, like the dock and drawer', async () => {
		h.mobile = true;
		await chatSurface.open('/messages/4');
		expect(gotoMock).toHaveBeenCalledTimes(1);
		expect(gotoMock).toHaveBeenCalledWith('/messages/4', {
			shallow: true,
			state: { chatDepth: 1, chatFromInbox: undefined }
		});
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
		await chatSurface.open('/settings/account');
		expect(gotoMock).toHaveBeenCalledWith('/settings/account');
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

describe('close and back are not re-entrant', () => {
	function openAtDepth(path: string, state: Record<string, unknown>) {
		(page as { shallow: unknown }).shallow = asShallow(path);
		page.state = state;
	}

	it('a second close before the first traversal lands does not pop past the opener', () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		openAtDepth('/messages', { chatDepth: 1 });
		chatSurface.close();
		chatSurface.close();
		expect(go).toHaveBeenCalledTimes(1);
		expect(go).toHaveBeenCalledWith(-1);
	});

	it('a second back from a depth-2 thread does not pop the inbox as well', () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		openAtDepth('/messages/4', { chatDepth: 2, chatFromInbox: true });
		chatSurface.back();
		chatSurface.back();
		expect(go).toHaveBeenCalledTimes(1);
	});

	it('a close after a back that is still travelling is also held', () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		openAtDepth('/messages/4', { chatDepth: 2, chatFromInbox: true });
		chatSurface.back();
		chatSurface.close();
		expect(go).toHaveBeenCalledTimes(1);
	});

	const popstate = () => window.dispatchEvent(new PopStateEvent('popstate'));

	it('releases when the traversal lands: its popstate, then the surface state moving', () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		openAtDepth('/messages/4', { chatDepth: 2, chatFromInbox: true });
		chatSurface.back();
		popstate();
		// the popstate landed: the inbox, one entry deep
		openAtDepth('/messages', { chatDepth: 1 });
		chatSurface.close();
		expect(go).toHaveBeenCalledTimes(2);
		expect(go).toHaveBeenLastCalledWith(-1);
	});

	it('a state change before any popstate (an open completing mid-traversal) does not release', () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		openAtDepth('/messages/4', { chatDepth: 2, chatFromInbox: true });
		chatSurface.back();
		openAtDepth('/messages/9', { chatDepth: 3, chatFromInbox: true });
		chatSurface.close();
		expect(go).toHaveBeenCalledTimes(1);
	});

	it('a popstate whose state has not been applied yet does not release', () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		openAtDepth('/messages', { chatDepth: 1 });
		chatSurface.close();
		popstate();
		chatSurface.close();
		expect(go).toHaveBeenCalledTimes(1);
	});

	it('reverse order: a closeThen continuation runs on the popstate, not the fallback', () => {
		vi.useFakeTimers();
		vi.spyOn(history, 'go').mockImplementation(() => {});
		(page as { shallow: unknown }).shallow = asShallow('/messages/4');
		page.state = { chatDepth: 2 };
		const run = vi.fn();
		expect(chatSurface.closeThen(run)).toBe(true);
		// Kit applied page.state first; the surface-key effect looked with popped still false.
		(page as { shallow: unknown }).shallow = null;
		page.state = {};
		__checkChatSurfaceForTests();
		expect(run).not.toHaveBeenCalled();
		// No read of the surface in between: only the popstate itself can release it,
		// and the fake clock has not moved, so the 3000ms fallback cannot have.
		popstate();
		expect(run).toHaveBeenCalledTimes(1);
	});

	it('a slow popstate: a second close at 1000ms with none landed issues no second go', () => {
		vi.useFakeTimers();
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		openAtDepth('/messages', { chatDepth: 1 });
		chatSurface.close();
		vi.advanceTimersByTime(1000);
		chatSurface.close();
		expect(go).toHaveBeenCalledTimes(1);
	});

	it('releases on a long fallback if no popstate ever arrives', () => {
		vi.useFakeTimers();
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		openAtDepth('/messages', { chatDepth: 1 });
		chatSurface.close();
		vi.advanceTimersByTime(3100);
		chatSurface.close();
		expect(go).toHaveBeenCalledTimes(2);
	});
});

describe('closeThen: close Chat, then run what needs the page underneath', () => {
	const popstate = () => window.dispatchEvent(new PopStateEvent('popstate'));

	it('with Chat closed it does nothing and returns false, so the caller runs', () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		const run = vi.fn();
		expect(chatSurface.closeThen(run)).toBe(false);
		expect(go).not.toHaveBeenCalled();
		expect(run).not.toHaveBeenCalled();
	});

	it('pops what Chat pushed and runs only after the traversal has landed', () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		(page as { shallow: unknown }).shallow = asShallow('/messages/4');
		page.state = { chatDepth: 2, paneDepth: 0, paneOwned: true };
		const run = vi.fn();
		expect(chatSurface.closeThen(run)).toBe(true);
		expect(go).toHaveBeenCalledWith(-2);
		expect(run).not.toHaveBeenCalled();
		popstate();
		(page as { shallow: unknown }).shallow = null;
		page.state = { paneDepth: 0, paneOwned: true };
		__checkChatSurfaceForTests(); // what the surface-key effect does when the state moves
		expect(run).toHaveBeenCalledTimes(1);
	});

	it('a gesture during a traversal is held, not queued behind it', () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		(page as { shallow: unknown }).shallow = asShallow('/messages');
		page.state = { chatDepth: 1 };
		chatSurface.close();
		const run = vi.fn();
		expect(chatSurface.closeThen(run)).toBe(true);
		expect(go).toHaveBeenCalledTimes(1);
		expect(run).not.toHaveBeenCalled();
	});
});

describe('deferPageWrite: a page-owned URL write waits for Chat to close', () => {
	const popstate = () => window.dispatchEvent(new PopStateEvent('popstate'));
	const setShallow = (path: string | null, state: Record<string, unknown>) => {
		(page as { shallow: unknown }).shallow = path === null ? null : asShallow(path);
		page.state = state;
	};
	/** Chat open at `depth` over /alice/ws/tasks. */
	const openAt = (depth: number) =>
		setShallow(depth === 1 ? '/messages' : '/messages/4', depth === 1 ? { chatDepth: 1 } : { chatDepth: 2, chatFromInbox: true });
	/** What Kit does when the traversal lands on the page underneath. */
	const land = () => {
		popstate();
		setShallow(null, {});
		__checkChatSurfaceForTests();
	};

	it('returns false and defers nothing when Chat is closed', () => {
		const run = vi.fn();
		expect(chatSurface.deferPageWrite('k', run)).toBe(false);
		__checkChatSurfaceForTests();
		expect(run).not.toHaveBeenCalled();
	});

	it('returns false in route mode (the page itself is /messages)', () => {
		page.url = new URL('http://localhost/messages/3');
		(page as { shallow: unknown }).shallow = asShallow('/messages/3');
		expect(chatSurface.deferPageWrite('k', vi.fn())).toBe(false);
	});

	it('returns false without a signed-in identity', () => {
		openAt(1);
		h.userId = null;
		expect(chatSurface.deferPageWrite('k', vi.fn())).toBe(false);
	});

	it('returns false on a phone while Chat is closed', () => {
		h.mobile = true;
		expect(chatSurface.deferPageWrite('k', vi.fn())).toBe(false);
	});

	it('holds the write on a phone while Chat is open (the sheet is a surface now)', () => {
		h.mobile = true;
		openAt(1);
		expect(chatSurface.deferPageWrite('k', vi.fn())).toBe(true);
	});

	it('holds the write while Chat is open and runs it once Chat has closed', () => {
		vi.spyOn(history, 'go').mockImplementation(() => {});
		openAt(1);
		const run = vi.fn();
		expect(chatSurface.deferPageWrite('k', run)).toBe(true);
		chatSurface.close();
		expect(run).not.toHaveBeenCalled();
		land();
		expect(run).toHaveBeenCalledTimes(1);
		__checkChatSurfaceForTests();
		expect(run).toHaveBeenCalledTimes(1);
	});

	it('last write wins per key; different keys all run, in insertion order', () => {
		vi.spyOn(history, 'go').mockImplementation(() => {});
		openAt(1);
		const order: string[] = [];
		chatSurface.deferPageWrite('a', () => void order.push('a1'));
		chatSurface.deferPageWrite('b', () => void order.push('b'));
		chatSurface.deferPageWrite('a', () => void order.push('a2'));
		chatSurface.close();
		land();
		expect(order).toEqual(['a2', 'b']);
	});

	it('Close with a closeThen continuation: the write runs first, the continuation after it settles', async () => {
		vi.spyOn(history, 'go').mockImplementation(() => {});
		openAt(2);
		const order: string[] = [];
		let finish!: () => void;
		chatSurface.deferPageWrite('k', () => {
			order.push('write-start');
			return new Promise<void>((r) => {
				finish = () => {
					order.push('write-done');
					r();
				};
			});
		});
		expect(chatSurface.closeThen(() => void order.push('continuation'))).toBe(true);
		land();
		expect(order).toEqual(['write-start']);
		finish();
		await Promise.resolve();
		await Promise.resolve();
		expect(order).toEqual(['write-start', 'write-done', 'continuation']);
	});

	it('closeThen with nothing deferred still runs the continuation synchronously', () => {
		vi.spyOn(history, 'go').mockImplementation(() => {});
		openAt(1);
		const run = vi.fn();
		chatSurface.closeThen(run);
		land();
		expect(run).toHaveBeenCalledTimes(1);
	});

	it('browser Back with no latch flushes (depth 1)', () => {
		openAt(1);
		const run = vi.fn();
		chatSurface.deferPageWrite('k', run);
		setShallow(null, {});
		__checkChatSurfaceForTests();
		expect(run).toHaveBeenCalledTimes(1);
	});

	it('browser Back at depth 2: the first Back leaves it pending, the second lands it', () => {
		openAt(2);
		const run = vi.fn();
		chatSurface.deferPageWrite('k', run);
		setShallow('/messages', { chatDepth: 1 });
		__checkChatSurfaceForTests();
		expect(run).not.toHaveBeenCalled();
		setShallow(null, {});
		__checkChatSurfaceForTests();
		expect(run).toHaveBeenCalledTimes(1);
	});

	it('a live latch owns the flush: the effect does not run it ahead of the continuation', () => {
		vi.spyOn(history, 'go').mockImplementation(() => {});
		openAt(1);
		const order: string[] = [];
		chatSurface.deferPageWrite('k', () => void order.push('write'));
		chatSurface.closeThen(() => void order.push('continuation'));
		// Kit moved the state first; the latch has not seen its popstate yet.
		setShallow(null, {});
		__checkChatSurfaceForTests();
		expect(order).toEqual([]);
		popstate();
		expect(order).toEqual(['write', 'continuation']);
	});

	it('dropDeferredExcept drops other-path entries and keeps same-path ones', () => {
		vi.spyOn(history, 'go').mockImplementation(() => {});
		openAt(1);
		const here = vi.fn();
		const elsewhere = vi.fn();
		chatSurface.deferPageWrite('k', here);
		page.url = new URL('http://localhost/alice/ws/other');
		chatSurface.deferPageWrite('k2', elsewhere);
		chatSurface.dropDeferredExcept('/alice/ws/other');
		setShallow(null, {});
		__checkChatSurfaceForTests();
		expect(here).not.toHaveBeenCalled();
		expect(elsewhere).toHaveBeenCalledTimes(1);
	});

	it('dropDeferredExcept(null) drops everything', () => {
		openAt(1);
		const run = vi.fn();
		chatSurface.deferPageWrite('k', run);
		chatSurface.dropDeferredExcept(null);
		setShallow(null, {});
		__checkChatSurfaceForTests();
		expect(run).not.toHaveBeenCalled();
	});

	it('an identity change before Chat closes drops the write', () => {
		openAt(1);
		const run = vi.fn();
		chatSurface.deferPageWrite('k', run);
		h.epoch++;
		setShallow(null, {});
		__checkChatSurfaceForTests();
		expect(run).not.toHaveBeenCalled();
	});

	it('a throwing write stops neither the others nor the continuation, and never runs twice', () => {
		vi.spyOn(history, 'go').mockImplementation(() => {});
		openAt(1);
		const second = vi.fn();
		const cont = vi.fn();
		chatSurface.deferPageWrite('a', () => {
			throw new Error('boom');
		});
		chatSurface.deferPageWrite('b', second);
		chatSurface.closeThen(cont);
		land();
		expect(second).toHaveBeenCalledTimes(1);
		expect(cont).toHaveBeenCalledTimes(1);
		__checkChatSurfaceForTests();
		popstate();
		expect(second).toHaveBeenCalledTimes(1);
	});

	it('the fallback release with the surface still open keeps the write for the real close', () => {
		vi.useFakeTimers();
		vi.spyOn(history, 'go').mockImplementation(() => {});
		openAt(1);
		const run = vi.fn();
		chatSurface.deferPageWrite('k', run);
		chatSurface.close();
		vi.advanceTimersByTime(3100);
		expect(run).not.toHaveBeenCalled();
		setShallow(null, {});
		__checkChatSurfaceForTests();
		expect(run).toHaveBeenCalledTimes(1);
	});
});

describe('runAfterClose: a heal that cannot wait is never lost', () => {
	const popstate = () => window.dispatchEvent(new PopStateEvent('popstate'));
	const openAt1 = () => {
		(page as { shallow: unknown }).shallow = asShallow('/messages');
		page.state = { chatDepth: 1 };
	};
	const land = () => {
		popstate();
		(page as { shallow: unknown }).shallow = null;
		page.state = {};
		__checkChatSurfaceForTests();
	};

	it('mid-Close (latch occupied, no continuation), the heal chains onto the Close in flight', () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		openAt1();
		chatSurface.close();
		const heal = vi.fn();
		chatSurface.runAfterClose(heal);
		expect(go).toHaveBeenCalledTimes(1);
		expect(heal).not.toHaveBeenCalled();
		land();
		expect(heal).toHaveBeenCalledTimes(1);
	});

	it('behind a pane continuation already parked: both run, the pane first', () => {
		vi.spyOn(history, 'go').mockImplementation(() => {});
		openAt1();
		const order: string[] = [];
		chatSurface.closeThen(() => void order.push('pane'));
		chatSurface.runAfterClose(() => void order.push('heal'));
		land();
		expect(order).toEqual(['pane', 'heal']);
	});

	it('several heals run in the order they arrived, once each', () => {
		vi.spyOn(history, 'go').mockImplementation(() => {});
		openAt1();
		const order: string[] = [];
		chatSurface.runAfterClose(() => void order.push('a'));
		chatSurface.runAfterClose(() => void order.push('b'));
		land();
		__checkChatSurfaceForTests();
		expect(order).toEqual(['a', 'b']);
	});

	it('the fallback with the surface still open runs the heal as a plain navigation (today)', () => {
		vi.useFakeTimers();
		vi.spyOn(history, 'go').mockImplementation(() => {});
		openAt1();
		const heal = vi.fn();
		chatSurface.runAfterClose(heal);
		vi.advanceTimersByTime(3100);
		expect(heal).toHaveBeenCalledTimes(1);
		// and it is not run a second time when the surface does close
		land();
		expect(heal).toHaveBeenCalledTimes(1);
	});

	it('a throwing heal does not stop the next one', () => {
		vi.spyOn(history, 'go').mockImplementation(() => {});
		openAt1();
		const next = vi.fn();
		chatSurface.runAfterClose(() => {
			throw new Error('boom');
		});
		chatSurface.runAfterClose(next);
		land();
		expect(next).toHaveBeenCalledTimes(1);
	});

	it('a heal supersedes pending page writes: they would land on the dead URL', () => {
		vi.spyOn(history, 'go').mockImplementation(() => {});
		openAt1();
		const write = vi.fn();
		const seen: boolean[] = [];
		chatSurface.deferPageWrite('k', write);
		chatSurface.runAfterClose(() => {
			seen.push(chatSurface.hasDeferredWrite('k'));
			return true;
		});
		land();
		// the heal could still read that a write was pending (to use live state), then it was dropped
		expect(seen).toEqual([true]);
		expect(write).not.toHaveBeenCalled();
		expect(chatSurface.hasDeferredWrite('k')).toBe(false);
	});

	it('runAfterClose runs now when Chat is closed', () => {
		const heal = vi.fn();
		chatSurface.runAfterClose(heal);
		expect(heal).toHaveBeenCalledTimes(1);
	});

	it('runAfterClose with Chat open closes first and runs after the landing', () => {
		const go = vi.spyOn(history, 'go').mockImplementation(() => {});
		openAt1();
		const heal = vi.fn();
		chatSurface.runAfterClose(heal);
		expect(go).toHaveBeenCalledWith(-1);
		expect(heal).not.toHaveBeenCalled();
		land();
		expect(heal).toHaveBeenCalledTimes(1);
	});
});

// Round 6. Helpers shared by the four blocks below.
const popstateEv = () => window.dispatchEvent(new PopStateEvent('popstate'));
const setSurface = (path: string | null, state: Record<string, unknown>) => {
	(page as { shallow: unknown }).shallow = path === null ? null : asShallow(path);
	page.state = state;
};
const landUnder = () => {
	popstateEv();
	setSurface(null, {});
	__checkChatSurfaceForTests();
};
const flush = () => new Promise<void>((r) => setTimeout(r, 0));

describe('deferred writes with different keys run one after another', () => {
	beforeEach(() => {
		vi.spyOn(history, 'go').mockImplementation(() => {});
	});

	/** A write the way activity's dropFromUrl is: read the LIVE page.url, then navigate (page.url moves a tick later). */
	const dropParam = (name: string) => async () => {
		const u = new URL(page.url.href);
		u.searchParams.delete(name);
		await Promise.resolve();
		await Promise.resolve();
		page.url = u;
	};

	it('two writes that each rebuild page.url both land in the final URL', async () => {
		page.url = new URL('http://localhost/alice/ws/activity?since=1&view=audit');
		setSurface('/messages', { chatDepth: 1 });
		chatSurface.deferPageWrite('p#drop-since', dropParam('since'));
		chatSurface.deferPageWrite('p#drop-view', dropParam('view'));
		chatSurface.close();
		landUnder();
		await flush();
		expect(page.url.search).toBe('');
	});

	it('a later write does not start until the earlier one has settled', async () => {
		setSurface('/messages', { chatDepth: 1 });
		const order: string[] = [];
		let finish!: () => void;
		chatSurface.deferPageWrite('a', () => {
			order.push('a-start');
			return new Promise<void>((r) => {
				finish = () => {
					order.push('a-done');
					r();
				};
			});
		});
		chatSurface.deferPageWrite('b', () => void order.push('b'));
		chatSurface.close();
		landUnder();
		expect(order).toEqual(['a-start']);
		finish();
		await flush();
		expect(order).toEqual(['a-start', 'a-done', 'b']);
	});

	it('a rejected write is logged and the next one still runs, then the continuation', async () => {
		const err = vi.spyOn(console, 'error').mockImplementation(() => {});
		setSurface('/messages', { chatDepth: 1 });
		const order: string[] = [];
		chatSurface.deferPageWrite('a', () => Promise.reject(new Error('nav failed')));
		chatSurface.deferPageWrite('b', () => void order.push('b'));
		expect(chatSurface.closeThen(() => void order.push('then'))).toBe(true);
		landUnder();
		await flush();
		expect(order).toEqual(['b', 'then']);
		expect(err).toHaveBeenCalled();
	});

	it('an identity change while an earlier write is in flight skips the later write', async () => {
		setSurface('/messages', { chatDepth: 1 });
		const b = vi.fn();
		chatSurface.deferPageWrite('a', async () => {
			await Promise.resolve();
			h.epoch++;
		});
		chatSurface.deferPageWrite('b', b);
		chatSurface.close();
		landUnder();
		await flush();
		expect(b).not.toHaveBeenCalled();
	});
});

describe('a heal waits for Chat to close on every settle path', () => {
	beforeEach(() => {
		vi.useFakeTimers();
	});

	/** How a traversal that leaves the INBOX open at depth 1 is begun. */
	const BEGIN: Array<[string, () => void]> = [
		[
			'close()',
			() => {
				setSurface('/messages/4', { chatDepth: 2, chatFromInbox: true });
				chatSurface.close();
			}
		],
		[
			'back()',
			() => {
				setSurface('/messages/4', { chatDepth: 2, chatFromInbox: true });
				chatSurface.back();
			}
		],
		[
			'closeThen()',
			() => {
				setSurface('/messages/4', { chatDepth: 2, chatFromInbox: true });
				expect(chatSurface.closeThen(() => {})).toBe(true);
			}
		]
	];
	/** How the settle is noticed. */
	const NOTICE: Array<[string, () => void]> = [
		['the popstate listener', popstateEv],
		['the surface-key effect', () => __checkChatSurfaceForTests()],
		['a traversing read', () => void chatSurface.traversing]
	];

	describe.each(BEGIN)('begun by %s', (_n, begin) => {
		it.each(NOTICE)('settled and noticed by %s with the inbox still open: the heal does not run plain; the close is re-issued', (_m, notice) => {
			const go = vi.spyOn(history, 'go').mockImplementation(() => {});
			begin();
			const heal = vi.fn();
			chatSurface.runAfterClose(heal);
			expect(go).toHaveBeenCalledTimes(1);
			// The first traversal landed on the inbox: the popstate fired and the state moved.
			popstateEv();
			setSurface('/messages', { chatDepth: 1 });
			notice();
			void chatSurface.traversing;
			expect(heal).not.toHaveBeenCalled();
			expect(go).toHaveBeenCalledTimes(2);
			expect(go).toHaveBeenLastCalledWith(-1);
			// ...and the heal runs once the second traversal has closed Chat.
			landUnder();
			expect(heal).toHaveBeenCalledTimes(1);
		});
	});

	it('only the 3s timer runs a heal plain with Chat still open', () => {
		vi.spyOn(history, 'go').mockImplementation(() => {});
		setSurface('/messages', { chatDepth: 1 });
		const heal = vi.fn();
		chatSurface.runAfterClose(heal);
		vi.advanceTimersByTime(2900);
		expect(heal).not.toHaveBeenCalled();
		vi.advanceTimersByTime(200);
		expect(heal).toHaveBeenCalledTimes(1);
	});

	it('a parked continuation is carried across the re-issued close and runs once, before the heal', () => {
		vi.spyOn(history, 'go').mockImplementation(() => {});
		setSurface('/messages/4', { chatDepth: 2, chatFromInbox: true });
		const order: string[] = [];
		chatSurface.closeThen(() => void order.push('then'));
		chatSurface.runAfterClose(() => void order.push('heal'));
		popstateEv();
		setSurface('/messages', { chatDepth: 1 });
		__checkChatSurfaceForTests();
		expect(order).toEqual([]);
		landUnder();
		expect(order).toEqual(['then', 'heal']);
	});
});

describe('a heal acts only for the page it was queued for', () => {
	beforeEach(() => {
		vi.spyOn(history, 'go').mockImplementation(() => {});
	});

	it('is skipped when the page is no longer the one it was queued on', () => {
		page.url = new URL('http://localhost/alice/ws/tasks');
		setSurface('/messages', { chatDepth: 1 });
		const heal = vi.fn();
		chatSurface.runAfterClose(heal);
		// the user's own navigation to another page committed before the close landed
		page.url = new URL('http://localhost/alice/ws/other');
		landUnder();
		expect(heal).not.toHaveBeenCalled();
	});

	it('runs on the same page even when the query changed meanwhile', () => {
		page.url = new URL('http://localhost/alice/ws/tasks');
		setSurface('/messages', { chatDepth: 1 });
		const heal = vi.fn();
		chatSurface.runAfterClose(heal);
		page.url = new URL('http://localhost/alice/ws/tasks?status=open');
		landUnder();
		expect(heal).toHaveBeenCalledTimes(1);
	});

	it('the 3s fallback skips it too once the page has moved', () => {
		vi.useFakeTimers();
		page.url = new URL('http://localhost/alice/ws/tasks');
		setSurface('/messages', { chatDepth: 1 });
		const heal = vi.fn();
		chatSurface.runAfterClose(heal);
		page.url = new URL('http://localhost/alice/ws/other');
		vi.advanceTimersByTime(3100);
		expect(heal).not.toHaveBeenCalled();
	});

	it('a real navigation away (dropDeferredExcept) drops a queued heal; a same-path one keeps it', () => {
		page.url = new URL('http://localhost/alice/ws/tasks');
		setSurface('/messages', { chatDepth: 1 });
		const heal = vi.fn();
		chatSurface.runAfterClose(heal);
		chatSurface.dropDeferredExcept('/alice/ws/tasks');
		chatSurface.dropDeferredExcept('/alice/ws/other');
		landUnder();
		expect(heal).not.toHaveBeenCalled();
	});
});

describe('a heal supersedes only the deferred writes it conflicts with', () => {
	beforeEach(() => {
		vi.spyOn(history, 'go').mockImplementation(() => {});
	});

	it('a heal that navigated drops the writes aimed at the page it left', () => {
		page.url = new URL('http://localhost/alice/ws/tasks');
		setSurface('/messages', { chatDepth: 1 });
		const write = vi.fn();
		chatSurface.deferPageWrite('k', write);
		chatSurface.runAfterClose(() => true);
		landUnder();
		expect(write).not.toHaveBeenCalled();
	});

	it('a heal that did nothing (returns false) leaves the write to run', () => {
		page.url = new URL('http://localhost/alice/ws/tasks');
		setSurface('/messages', { chatDepth: 1 });
		const write = vi.fn();
		chatSurface.deferPageWrite('item-detail#strip-new', write);
		chatSurface.runAfterClose(() => false);
		landUnder();
		expect(write).toHaveBeenCalledTimes(1);
	});

	it('a heal for a different path does not drop a write made on another path; the flush then skips it as stale', () => {
		page.url = new URL('http://localhost/alice/ws/jobs/ITEM-1');
		setSurface('/messages', { chatDepth: 1 });
		const strip = vi.fn();
		chatSurface.deferPageWrite('item-detail#strip-new', strip);
		page.url = new URL('http://localhost/alice/ws/tasks');
		chatSurface.runAfterClose(() => true);
		landUnder();
		// not dropped by the heal (other pathname), but not run on a page it was not made for
		expect(chatSurface.hasDeferredWrite('item-detail#strip-new')).toBe(false);
		expect(strip).not.toHaveBeenCalled();
	});

	it('writes that survive still run after the heals, in order', () => {
		page.url = new URL('http://localhost/alice/ws/tasks');
		setSurface('/messages', { chatDepth: 1 });
		const order: string[] = [];
		chatSurface.deferPageWrite('k', () => void order.push('write'));
		chatSurface.runAfterClose(() => {
			order.push('heal');
			return false;
		});
		landUnder();
		expect(order).toEqual(['heal', 'write']);
	});
});

describe('round 7: a serial flush follows the page, and the chain is total and bounded', () => {
	beforeEach(() => {
		vi.spyOn(history, 'go').mockImplementation(() => {});
	});

	it('a navigation while the first write is in flight aborts the writes still queued', async () => {
		setSurface('/messages', { chatDepth: 1 });
		let finish!: () => void;
		const b = vi.fn();
		chatSurface.deferPageWrite('a', () => new Promise<void>((r) => (finish = r)));
		chatSurface.deferPageWrite('b', b);
		chatSurface.close();
		landUnder();
		// ChatHost onNavigate for a real navigation to another page
		// page.url stays on the SAME path: only the pruning of the running queue can stop b
		chatSurface.dropDeferredExcept('/alice/ws/other');
		finish();
		await flush();
		expect(b).not.toHaveBeenCalled();
	});

	it('a write for another pathname is skipped when it comes up even with no drop', async () => {
		setSurface('/messages', { chatDepth: 1 });
		let finish!: () => void;
		const b = vi.fn();
		chatSurface.deferPageWrite('a', () => new Promise<void>((r) => (finish = r)));
		chatSurface.deferPageWrite('b', b);
		chatSurface.close();
		landUnder();
		page.url = new URL('http://localhost/alice/ws/other');
		finish();
		await flush();
		expect(b).not.toHaveBeenCalled();
	});

	it('a same-path drop leaves the queued same-path write running', async () => {
		setSurface('/messages', { chatDepth: 1 });
		let finish!: () => void;
		const b = vi.fn();
		chatSurface.deferPageWrite('a', () => new Promise<void>((r) => (finish = r)));
		chatSurface.deferPageWrite('b', b);
		chatSurface.close();
		landUnder();
		chatSurface.dropDeferredExcept('/alice/ws/tasks');
		finish();
		await flush();
		expect(b).toHaveBeenCalledTimes(1);
	});

	it('a throwing fence is caught: the next write, the continuation and the heals still run', async () => {
		const err = vi.spyOn(console, 'error').mockImplementation(() => {});
		setSurface('/messages', { chatDepth: 1 });
		const order: string[] = [];
		chatSurface.deferPageWrite('a', () => void order.push('a'));
		h.throwAt = h.fenceN;
		chatSurface.deferPageWrite('b', () => void order.push('b'));
		chatSurface.deferPageWrite('c', () => void order.push('c'));
		expect(chatSurface.closeThen(() => void order.push('then'))).toBe(true);
		landUnder();
		await flush();
		expect(order).toEqual(['a', 'c', 'then']);
		expect(err).toHaveBeenCalled();
	});

	it('a write that never settles is left after 3s: the next write and the continuation run', async () => {
		vi.useFakeTimers();
		setSurface('/messages', { chatDepth: 1 });
		const order: string[] = [];
		chatSurface.deferPageWrite('a', () => new Promise<void>(() => {}));
		chatSurface.deferPageWrite('b', () => void order.push('b'));
		expect(chatSurface.closeThen(() => void order.push('then'))).toBe(true);
		landUnder();
		await vi.advanceTimersByTimeAsync(2900);
		expect(order).toEqual([]);
		await vi.advanceTimersByTimeAsync(200);
		expect(order).toEqual(['b', 'then']);
	});
});

describe('round 7: a heal drops writes only when it reports that it navigated', () => {
	beforeEach(() => {
		vi.spyOn(history, 'go').mockImplementation(() => {});
		page.url = new URL('http://localhost/alice/ws/tasks');
	});
	const run = async (heal: () => unknown) => {
		setSurface('/messages', { chatDepth: 1 });
		const write = vi.fn();
		chatSurface.deferPageWrite('k', write);
		chatSurface.runAfterClose(heal as never);
		landUnder();
		await flush();
		return write;
	};

	it('true drops the write', async () => expect(await run(() => true)).not.toHaveBeenCalled());
	it('undefined (unknown) keeps the write', async () => expect(await run(() => undefined)).toHaveBeenCalledTimes(1));
	it('false keeps the write', async () => expect(await run(() => false)).toHaveBeenCalledTimes(1));
	it('a promise resolving true drops the write, only after it resolved', async () => {
		let done!: () => void;
		setSurface('/messages', { chatDepth: 1 });
		const write = vi.fn();
		const later = vi.fn();
		chatSurface.deferPageWrite('k', write);
		chatSurface.runAfterClose((() => new Promise<boolean>((r) => (done = () => r(true)))) as never);
		expect(chatSurface.hasDeferredWrite('k')).toBe(true);
		landUnder();
		await flush();
		// still held while the heal's navigation is pending
		expect(chatSurface.hasDeferredWrite('k')).toBe(true);
		expect(write).not.toHaveBeenCalled();
		done();
		await flush();
		expect(chatSurface.hasDeferredWrite('k')).toBe(false);
		expect(write).not.toHaveBeenCalled();
		// a later flush has nothing of it to run
		setSurface('/messages', { chatDepth: 1 });
		chatSurface.deferPageWrite('later', later);
		chatSurface.close();
		landUnder();
		await flush();
		expect(later).toHaveBeenCalledTimes(1);
		expect(write).not.toHaveBeenCalled();
	});
	it('a promise resolving false keeps the write', async () =>
		expect(await run(() => Promise.resolve(false))).toHaveBeenCalledTimes(1));
	it('a rejected navigation keeps the write', async () => {
		vi.spyOn(console, 'error').mockImplementation(() => {});
		expect(await run(() => Promise.reject(new Error('nav failed')))).toHaveBeenCalledTimes(1);
	});
	it('a throwing heal keeps the write', async () => {
		vi.spyOn(console, 'error').mockImplementation(() => {});
		expect(
			await run(() => {
				throw new Error('boom');
			})
		).toHaveBeenCalledTimes(1);
	});
	it('a pending heal navigation holds the surviving writes until it settles', async () => {
		let done!: () => void;
		setSurface('/messages', { chatDepth: 1 });
		const write = vi.fn();
		chatSurface.deferPageWrite('k', write);
		chatSurface.runAfterClose((() => new Promise<boolean>((r) => (done = () => r(false)))) as never);
		landUnder();
		await flush();
		expect(write).not.toHaveBeenCalled();
		done();
		await flush();
		expect(write).toHaveBeenCalledTimes(1);
	});
});

describe('round 7: a heal carries a held ?new=1 strip instead of leaving it behind', () => {
	beforeEach(() => {
		vi.spyOn(history, 'go').mockImplementation(() => {});
	});

	it('searchForHeal removes new= only while the strip is held', () => {
		page.url = new URL('http://localhost/alice/ws/tasks/ITEM-1?new=1&view=x');
		expect(chatSurface.searchForHeal('?new=1&view=x')).toBe('?new=1&view=x');
		setSurface('/messages', { chatDepth: 1 });
		chatSurface.deferPageWrite(STRIP_NEW_KEY, vi.fn());
		expect(chatSurface.searchForHeal('?new=1&view=x')).toBe('?view=x');
		expect(chatSurface.searchForHeal('?new=1')).toBe('');
		expect(chatSurface.searchForHeal('')).toBe('');
	});

	it('a heal built on it navigates without new= and the strip, dropped, is not needed', async () => {
		page.url = new URL('http://localhost/alice/ws/tasks/ITEM-1?new=1');
		setSurface('/messages', { chatDepth: 1 });
		const strip = vi.fn();
		chatSurface.deferPageWrite(STRIP_NEW_KEY, strip);
		let target = '';
		chatSurface.runAfterClose(() => {
			target = `/alice/ws/jobs/ITEM-1${chatSurface.searchForHeal(page.url.search)}`;
			return true;
		});
		landUnder();
		await flush();
		expect(target).toBe('/alice/ws/jobs/ITEM-1');
		expect(strip).not.toHaveBeenCalled();
	});

	it('the board-filter case: filters are carried by the heal, so the held filter write may drop', async () => {
		page.url = new URL('http://localhost/alice/ws/tasks?status=open');
		setSurface('/messages', { chatDepth: 1 });
		const filters = vi.fn();
		chatSurface.deferPageWrite('filters', filters);
		chatSurface.runAfterClose(() => true);
		landUnder();
		await flush();
		expect(filters).not.toHaveBeenCalled();
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

describe('round 8: a heal reports a navigation only when it committed', () => {
	beforeEach(() => {
		vi.spyOn(history, 'go').mockImplementation(() => {});
		page.url = new URL('http://localhost/alice/ws/tasks');
	});
	const runNav = async (nav: () => Promise<unknown>) => {
		setSurface('/messages', { chatDepth: 1 });
		const write = vi.fn();
		chatSurface.deferPageWrite('k', write);
		chatSurface.runAfterClose(() => navigated(nav()));
		landUnder();
		await flush();
		return write;
	};

	it('a goto a guard cancelled (resolves, URL unchanged) keeps the held write', async () => {
		expect(await runNav(async () => {})).toHaveBeenCalledTimes(1);
	});
	it('a goto that committed (URL moved) drops the held write', async () => {
		expect(
			await runNav(async () => {
				await Promise.resolve(); // goto commits after it was called, never synchronously
				page.url = new URL('http://localhost/alice/ws/tasks?new=0');
			})
		).not.toHaveBeenCalled();
	});
	it('a rejected goto keeps the held write', async () => {
		vi.spyOn(console, 'error').mockImplementation(() => {});
		expect(await runNav(() => Promise.reject(new Error('nav failed')))).toHaveBeenCalledTimes(1);
	});
});

describe('round 8: a heal that never settles cannot hold the flush', () => {
	beforeEach(() => {
		vi.spyOn(history, 'go').mockImplementation(() => {});
		page.url = new URL('http://localhost/alice/ws/tasks');
	});

	// Round 11: past the bound the wait opens for everything else, but the page's own
	// writes stay held (a late heal may still navigate); a real navigation drops them.
	it('after 3s the wait opens, the page writes stay held, and a real navigation drops them', async () => {
		vi.useFakeTimers();
		setSurface('/messages', { chatDepth: 1 });
		const write = vi.fn();
		chatSurface.deferPageWrite('k', write);
		chatSurface.runAfterClose((() => new Promise<boolean>(() => {})) as never);
		landUnder();
		await vi.advanceTimersByTimeAsync(3100);
		expect(write).not.toHaveBeenCalled();
		expect(chatSurface.hasDeferredWrite('k')).toBe(true);
		chatSurface.dropDeferredExcept('/alice/ws/other');
		expect(chatSurface.hasDeferredWrite('k')).toBe(false);
	});
});

// Round 11. A held write waits for its page's heal; a parked continuation cannot fire
// after a newer gesture; a throwing continuation strands nothing; a flush never runs
// under a reopened Chat.
const controlled = <T>() => {
	let resolve!: (v: T) => void;
	const p = new Promise<T>((r) => (resolve = r));
	return { p, resolve };
};
const DEAD = 'http://localhost/alice/ws/tasks';
const LIVE = 'http://localhost/alice/ws/tasks-new';

describe('round 11: a held write waits for its page heal to settle', () => {
	beforeEach(() => {
		vi.useFakeTimers();
		vi.spyOn(history, 'go').mockImplementation(() => {});
		page.url = new URL(DEAD);
	});

	/** Chat open on the dead slug with a held filter write; a caller-style heal whose goto the test controls. */
	const setup = (beforeLand: () => void = () => {}) => {
		setSurface('/messages', { chatDepth: 1 });
		beforeLand();
		// the write rebuilds the URL from the dead slug, as a board filter does
		const write = vi.fn(() => {
			page.url = new URL(`${DEAD}?f=1`);
		});
		chatSurface.deferPageWrite('k', write);
		const nav = controlled<void>();
		const state = { renameNav: 'tasks-new' as string | null, ok: undefined as boolean | undefined };
		chatSurface.runAfterClose(() =>
			navigated(nav.p).then((ok) => {
				if (!ok) state.renameNav = null; // the caller's poisoned-tracker reset
				state.ok = ok;
				return ok;
			})
		);
		landUnder();
		return { write, nav, state };
	};

	it('a heal load past 3s: the held write does not run at the bound; the heal then commits and the write is dropped', async () => {
		const { write, nav, state } = setup();
		await vi.advanceTimersByTimeAsync(3100);
		expect(write).not.toHaveBeenCalled();
		page.url = new URL(LIVE);
		nav.resolve();
		await vi.advanceTimersByTimeAsync(10);
		expect(state.ok).toBe(true);
		expect(page.url.pathname).toBe('/alice/ws/tasks-new');
		expect(page.url.search).toBe('');
		expect(state.renameNav).toBe('tasks-new');
		expect(write).not.toHaveBeenCalled();
		expect(chatSurface.hasDeferredWrite('k')).toBe(false);
	});

	it('the same heal settling late as NOT navigated runs the held write', async () => {
		const { write, nav, state } = setup();
		await vi.advanceTimersByTimeAsync(3100);
		expect(write).not.toHaveBeenCalled();
		nav.resolve(); // a guard cancelled it: resolved, URL unchanged
		await vi.advanceTimersByTimeAsync(10);
		expect(state.ok).toBe(false);
		expect(write).toHaveBeenCalledTimes(1);
	});

	it('healWait is released at the bound: another page state flushes, this page writes stay', async () => {
		const { write } = setup(() => {
			page.url = new URL('http://localhost/alice/ws/other');
			chatSurface.deferPageWrite('o', vi.fn()); // held on another page
			page.url = new URL(DEAD);
		});
		await vi.advanceTimersByTimeAsync(2900);
		expect(chatSurface.hasDeferredWrite('o')).toBe(true);
		await vi.advanceTimersByTimeAsync(200);
		expect(chatSurface.hasDeferredWrite('o')).toBe(false);
		expect(chatSurface.hasDeferredWrite('k')).toBe(true);
		expect(write).not.toHaveBeenCalled();
	});

	it('a heal that never settles leaves the writes held; a real navigation drops them', async () => {
		const { write } = setup();
		await vi.advanceTimersByTimeAsync(10000);
		expect(write).not.toHaveBeenCalled();
		expect(chatSurface.hasDeferredWrite('k')).toBe(true);
		chatSurface.dropDeferredExcept('/alice/ws/other');
		expect(chatSurface.hasDeferredWrite('k')).toBe(false);
		// the mark went with it: a later write on the page is not held by a heal that no longer applies
		page.url = new URL(DEAD);
		setSurface('/messages', { chatDepth: 1 });
		const later = vi.fn();
		chatSurface.deferPageWrite('n', later);
		chatSurface.close();
		landUnder();
		await vi.advanceTimersByTimeAsync(10);
		expect(later).toHaveBeenCalledTimes(1);
		expect(write).not.toHaveBeenCalled();
	});
});

describe('round 11: a parked continuation fires only if no newer gesture proceeded', () => {
	beforeEach(() => {
		vi.useFakeTimers();
		vi.spyOn(history, 'go').mockImplementation(() => {});
		page.url = new URL(DEAD);
	});

	/** Row click 1: parks `run`; the release finds a write in flight, so `run` lives only in the write chain. */
	const park = () => {
		setSurface('/messages', { chatDepth: 1 });
		const slow = controlled<void>();
		chatSurface.deferPageWrite('k', () => slow.p);
		const run = vi.fn();
		expect(chatSurface.closeThen(run)).toBe(true);
		landUnder();
		expect(run).not.toHaveBeenCalled();
		return { run, slow };
	};
	const settle = async (slow: { resolve: (v: void) => void }) => {
		slow.resolve();
		await vi.advanceTimersByTimeAsync(10);
	};

	it('(control) no newer gesture: it fires once', async () => {
		const { run, slow } = park();
		await settle(slow);
		expect(run).toHaveBeenCalledTimes(1);
	});

	it('(a) a second row click on the same page after the release does not let it fire', async () => {
		const { run, slow } = park();
		expect(chatSurface.traversing).toBe(false); // what paneNavInFlight reads: idle, the gesture proceeds
		await settle(slow);
		expect(run).not.toHaveBeenCalled();
	});

	it('(a2) a gesture that finds Chat closed (closeThen false) cancels it too', async () => {
		const { run, slow } = park();
		expect(chatSurface.closeThen(vi.fn())).toBe(false);
		await settle(slow);
		expect(run).not.toHaveBeenCalled();
	});

	it('(b) a sidebar navigation (another pathname) does not let it fire', async () => {
		const { run, slow } = park();
		chatSurface.dropDeferredExcept('/alice/ws/other');
		page.url = new URL('http://localhost/alice/ws/other');
		await settle(slow);
		expect(run).not.toHaveBeenCalled();
	});

	it('(b2) a pathname change alone does not let it fire', async () => {
		const { run, slow } = park();
		page.url = new URL('http://localhost/alice/ws/other');
		await settle(slow);
		expect(run).not.toHaveBeenCalled();
	});

	it('(d) a second click during the traversal is held and does not cancel the first', async () => {
		setSurface('/messages', { chatDepth: 1 });
		const slow = controlled<void>();
		chatSurface.deferPageWrite('k', () => slow.p);
		const first = vi.fn();
		const second = vi.fn();
		expect(chatSurface.closeThen(first)).toBe(true);
		expect(chatSurface.traversing).toBe(true); // a pane gesture is held here
		expect(chatSurface.closeThen(second)).toBe(true);
		landUnder();
		await settle(slow);
		expect(first).toHaveBeenCalledTimes(1);
		expect(second).not.toHaveBeenCalled();
	});
});

describe('round 11: a throwing continuation strands neither the heals nor the flush', () => {
	beforeEach(() => {
		vi.useFakeTimers();
		vi.spyOn(history, 'go').mockImplementation(() => {});
		page.url = new URL(DEAD);
	});

	it('heals arm: the heal and the held write still run', async () => {
		const err = vi.spyOn(console, 'error').mockImplementation(() => {});
		setSurface('/messages', { chatDepth: 1 });
		const write = vi.fn();
		chatSurface.deferPageWrite('k', write);
		const heal = vi.fn(() => false);
		chatSurface.closeThen(() => {
			throw new Error('then boom');
		});
		chatSurface.runAfterClose(heal);
		expect(() => landUnder()).not.toThrow();
		await vi.advanceTimersByTimeAsync(10);
		expect(heal).toHaveBeenCalledTimes(1);
		expect(write).toHaveBeenCalledTimes(1);
		expect(err).toHaveBeenCalled();
	});

	it('no-heals arm: the writes run first and a throw after them is logged, sync and after a promise', async () => {
		const err = vi.spyOn(console, 'error').mockImplementation(() => {});
		for (const slowWrite of [false, true]) {
			err.mockClear();
			__resetChatSurfaceForTests();
			setSurface('/messages', { chatDepth: 1 });
			const write = vi.fn(() => (slowWrite ? Promise.resolve() : undefined));
			chatSurface.deferPageWrite('k', write);
			chatSurface.closeThen(() => {
				throw new Error('then boom');
			});
			expect(() => landUnder()).not.toThrow();
			await vi.advanceTimersByTimeAsync(10);
			expect(write).toHaveBeenCalledTimes(1);
			expect(err).toHaveBeenCalled();
		}
	});
});

describe('round 11: a heal that settles does not flush under a reopened Chat', () => {
	beforeEach(() => {
		vi.useFakeTimers();
		vi.spyOn(history, 'go').mockImplementation(() => {});
		page.url = new URL(DEAD);
	});

	it('nothing flushes while Chat is open again; the next real close flushes', async () => {
		setSurface('/messages', { chatDepth: 1 });
		const write = vi.fn();
		chatSurface.deferPageWrite('k', write);
		const nav = controlled<boolean>();
		chatSurface.runAfterClose(() => nav.p);
		landUnder();
		setSurface('/messages', { chatDepth: 1 }); // Chat reopened during the heal
		nav.resolve(false);
		await vi.advanceTimersByTimeAsync(10);
		expect(write).not.toHaveBeenCalled();
		expect(chatSurface.hasDeferredWrite('k')).toBe(true);
		chatSurface.close();
		landUnder();
		await vi.advanceTimersByTimeAsync(10);
		expect(write).toHaveBeenCalledTimes(1);
	});
});
