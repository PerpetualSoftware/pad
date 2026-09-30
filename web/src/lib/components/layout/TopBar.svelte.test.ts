// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// TASK-3274 (PLAN-3002 U3): the desktop TopBar renders the caller's open set
// of workspace tabs from `tabsStore`, in one zone. These tests drive the REAL
// tabs store over a mocked API, so what the bar shows is what a committed
// server answer says, and a close or pin goes through the store's own door.
//
// This file used to pin the workspace OVERFLOW MENU's window keydown handler
// (TASK-2430: it must defer Escape and the arrows to a frontmost viewer). The
// menu and its handler are deleted by this unit, so that defect class no
// longer has a member here; the last test below pins that the bar registers
// no window key owner at all, which is what makes the deference moot.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick, flushSync } from 'svelte';

type Tab = { slug: string; name: string; ephemeral?: boolean; is_guest?: boolean; last_route?: string };

const mocks = vi.hoisted(() => ({
	goto: vi.fn(async () => {}),
	current: { slug: 'beta', name: 'Beta', owner_username: 'u' } as { slug: string; name: string; owner_username: string } | null,
	tabs: {
		list: vi.fn(),
		open: vi.fn(),
		close: vi.fn(),
		reorder: vi.fn(),
		update: vi.fn(),
	},
}));

vi.mock('$app/navigation', () => ({ goto: mocks.goto }));

vi.mock('$lib/api/client', () => ({
	PadApiError: class extends Error {},
	api: { workspaces: { tabs: mocks.tabs, listDeleted: vi.fn(async () => []) } },
}));

vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: {
		get current() {
			return mocks.current;
		},
		workspaces: [],
	},
}));

import { page } from '$app/state';
import TopBar from './TopBar.svelte';
import { tabsStore } from '$lib/stores/tabs.svelte';
import { uiStore } from '$lib/stores/ui.svelte';

// Each answer is numbered after the last, as a server processing the
// requests in the order the test answers them would (BUG-3285).
let revision = 0;
function answer(tabs: Tab[]) {
	return {
		revision: ++revision,
		tabs: tabs.map((t, i) => ({
			owner_username: 'u',
			is_guest: false,
			ephemeral: false,
			created_at: '',
			updated_at: '',
			...t,
			position: i,
		})),
	};
}

const ALPHA = { slug: 'alpha', name: 'Alpha' };
const BETA = { slug: 'beta', name: 'Beta' };
const GAMMA = { slug: 'gamma', name: 'Gamma' };

async function mountWith(tabs: Tab[]) {
	mocks.tabs.list.mockResolvedValueOnce(answer(tabs));
	await tabsStore.load();
	render(TopBar, { props: {} });
	await tick();
	flushSync();
}

const tabEls = () => Array.from(document.querySelectorAll<HTMLElement>('.workspace-tab'));
const link = (slug: string) =>
	document.querySelector<HTMLAnchorElement>(`.workspace-tab[data-ws-slug="${slug}"] a`)!;
const closeBtn = (slug: string) =>
	document.querySelector<HTMLButtonElement>(`.workspace-tab[data-ws-slug="${slug}"] .workspace-tab-close`)!;

async function settle() {
	for (let i = 0; i < 5; i++) await Promise.resolve();
	await tick();
	flushSync();
}

/**
 * A close animates for 140ms before it writes (TASK-3312), so a close test
 * waits that out first. Without it, the assertions about what a close did or
 * did not do run before the close happens, and the negative ones pass vacuously.
 */
async function settleClose() {
	await new Promise((r) => setTimeout(r, 200));
	await settle();
}

beforeEach(() => {
	mocks.goto.mockClear();
	mocks.current = { slug: 'beta', name: 'Beta', owner_username: 'u' };
	for (const fn of Object.values(mocks.tabs)) fn.mockReset();
	uiStore.clearAddWorkspaceHighlight();
	page.url = new URL('http://localhost/u/beta');
	try {
		localStorage.clear();
	} catch {}
});

afterEach(() => {
	cleanup();
	document.body.innerHTML = '';
});

describe('TopBar tab bar: what it shows', () => {
	it('renders the open set in bar order, marking the current workspace active', async () => {
		await mountWith([ALPHA, BETA, GAMMA]);
		expect(tabEls().map((el) => el.dataset.wsSlug)).toEqual(['alpha', 'beta', 'gamma']);
		expect(tabEls().filter((el) => el.classList.contains('active')).map((el) => el.dataset.wsSlug)).toEqual(['beta']);
		expect(link('beta').getAttribute('aria-current')).toBe('page');
	});

	it('renders no overflow menu and no measurement ghost row', async () => {
		await mountWith([ALPHA, BETA, GAMMA]);
		expect(document.querySelector('#workspace-overflow-menu')).toBeNull();
		expect(document.querySelector('.overflow-trigger')).toBeNull();
		expect(document.querySelector('.workspace-ghost')).toBeNull();
	});

	it('marks an ephemeral tab (italic, PLAN-3002 Q9) and a guest one (Q10)', async () => {
		await mountWith([ALPHA, { ...BETA, ephemeral: true }, { ...GAMMA, is_guest: true }]);
		const [a, b, c] = tabEls();
		expect(a.classList.contains('ephemeral')).toBe(false);
		expect(b.classList.contains('ephemeral')).toBe(true);
		expect(c.classList.contains('guest')).toBe(true);
		expect(link('gamma').title).toBe('Gamma (shared with you)');
	});

	it('keeps real hrefs to each workspace dashboard', async () => {
		await mountWith([ALPHA, BETA]);
		expect(link('alpha').getAttribute('href')).toBe('/u/alpha');
	});
});

describe('TopBar tab bar: clicks', () => {
	it('a plain click restores the workspace\'s last route', async () => {
		await mountWith([{ ...ALPHA, last_route: '/u/alpha/tasks' }, BETA]);
		link('alpha').dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, button: 0, detail: 1 }));
		expect(mocks.goto).toHaveBeenCalledWith('/u/alpha/tasks');
	});

	it('a click on the current workspace goes to its dashboard', async () => {
		await mountWith([ALPHA, { ...BETA, last_route: '/u/beta/tasks' }]);
		link('beta').dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, button: 0, detail: 1 }));
		expect(mocks.goto).toHaveBeenCalledWith('/u/beta');
	});

	it('a modifier click is left to the browser', async () => {
		await mountWith([ALPHA, BETA]);
		const e = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0, metaKey: true, detail: 1 });
		link('alpha').dispatchEvent(e);
		expect(e.defaultPrevented).toBe(false);
		expect(mocks.goto).not.toHaveBeenCalled();
	});

	it('double-click keeps an ephemeral tab, and its second click does not navigate', async () => {
		await mountWith([ALPHA, { ...BETA, ephemeral: true }]);
		mocks.tabs.update.mockResolvedValueOnce(answer([ALPHA, BETA]));
		const second = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0, detail: 2 });
		link('beta').dispatchEvent(second);
		link('beta').dispatchEvent(new MouseEvent('dblclick', { bubbles: true, cancelable: true }));
		await settle();
		expect(mocks.goto).not.toHaveBeenCalled();
		expect(second.defaultPrevented).toBe(true);
		expect(mocks.tabs.update).toHaveBeenCalledWith('beta', { pin: true });
		expect(tabEls()[1].classList.contains('ephemeral')).toBe(false);
	});

	it('double-click on a kept tab sends nothing', async () => {
		await mountWith([ALPHA, BETA]);
		link('alpha').dispatchEvent(new MouseEvent('dblclick', { bubbles: true, cancelable: true }));
		await settle();
		expect(mocks.tabs.update).not.toHaveBeenCalled();
	});
});

describe('TopBar tab bar: closing (PLAN-3002 Q2, Q3)', () => {
	it('closing a tab that is not active stays where you are', async () => {
		await mountWith([ALPHA, BETA, GAMMA]);
		mocks.tabs.close.mockResolvedValueOnce(answer([BETA, GAMMA]));
		closeBtn('alpha').click();
		await settleClose();
		expect(mocks.tabs.close).toHaveBeenCalledWith('alpha');
		expect(mocks.goto).not.toHaveBeenCalled();
		expect(tabEls().map((el) => el.dataset.wsSlug)).toEqual(['beta', 'gamma']);
	});

	it('closing the active tab lands on its left neighbour, at that tab\'s last route', async () => {
		await mountWith([{ ...ALPHA, last_route: '/u/alpha/docs' }, BETA, GAMMA]);
		mocks.tabs.close.mockResolvedValueOnce(answer([{ ...ALPHA, last_route: '/u/alpha/docs' }, GAMMA]));
		closeBtn('beta').click();
		await settleClose();
		expect(mocks.goto).toHaveBeenCalledWith('/u/alpha/docs');
	});

	// The leg above closes the SECOND tab, whose left neighbour is also the
	// first tab, so it cannot tell "left neighbour" from "first remaining"
	// (TASK-3280: a closeTab that read the open set after the close passed it).
	it('closing an active tab further right lands on its left neighbour, not on the first tab', async () => {
		mocks.current = { slug: 'gamma', name: 'Gamma', owner_username: 'u' };
		await mountWith([ALPHA, BETA, GAMMA]);
		mocks.tabs.close.mockResolvedValueOnce(answer([ALPHA, BETA]));
		closeBtn('gamma').click();
		await settleClose();
		expect(mocks.goto).toHaveBeenCalledWith('/u/beta');
	});

	it('closing the active FIRST tab lands on the tab that becomes first (lead ruling on Q3)', async () => {
		mocks.current = { slug: 'alpha', name: 'Alpha', owner_username: 'u' };
		await mountWith([ALPHA, BETA, GAMMA]);
		mocks.tabs.close.mockResolvedValueOnce(answer([BETA, GAMMA]));
		closeBtn('alpha').click();
		await settleClose();
		expect(mocks.goto).toHaveBeenCalledWith('/u/beta');
	});

	it('closing the last tab lands on /console with "+" highlighted', async () => {
		await mountWith([BETA]);
		mocks.tabs.close.mockResolvedValueOnce(answer([]));
		mocks.goto.mockImplementationOnce(async (url: string) => {
			page.url = new URL(url, 'http://localhost');
		});
		closeBtn('beta').click();
		await settleClose();
		expect(mocks.goto).toHaveBeenCalledWith('/console');
		expect(uiStore.addWorkspaceHighlighted).toBe(true);
	});

	it('a close the server refuses moves nothing', async () => {
		await mountWith([ALPHA, BETA]);
		mocks.tabs.close.mockRejectedValueOnce(new TypeError('network down'));
		closeBtn('beta').click();
		await settleClose();
		expect(mocks.tabs.close, 'the close was attempted').toHaveBeenCalledWith('beta');
		expect(mocks.goto).not.toHaveBeenCalled();
		expect(tabEls().map((el) => el.dataset.wsSlug)).toEqual(['alpha', 'beta']);
	});

	it('the close button does not also follow the tab link', async () => {
		await mountWith([ALPHA, BETA, GAMMA]);
		mocks.tabs.close.mockResolvedValueOnce(answer([BETA, GAMMA]));
		const e = new MouseEvent('click', { bubbles: true, cancelable: true, button: 0, detail: 1 });
		closeBtn('alpha').dispatchEvent(e);
		await settleClose();
		expect(e.defaultPrevented).toBe(true);
		expect(mocks.tabs.close, 'the close ran').toHaveBeenCalledWith('alpha');
		expect(mocks.goto).not.toHaveBeenCalled();
	});
});

describe('TopBar: the Q2 highlight', () => {
	it('opening the create flow from "+" clears it', async () => {
		await mountWith([ALPHA]);
		uiStore.highlightAddWorkspace();
		document.querySelector<HTMLButtonElement>('.workspace-add')!.click();
		await tick();
		flushSync();
		// "+" opens the discovery surface (U4); its last option is create.
		document.querySelector<HTMLElement>('.discovery-option.create')!.click();
		flushSync();
		expect(uiStore.addWorkspaceHighlighted).toBe(false);
		uiStore.closeCreateWorkspace();
	});
});

describe('TopBar: no window key owner (successor to TASK-2430)', () => {
	it('consumes neither Escape nor the arrow keys on window', async () => {
		await mountWith([ALPHA, BETA]);
		for (const k of ['Escape', 'ArrowDown', 'ArrowUp']) {
			const e = new KeyboardEvent('keydown', { key: k, bubbles: true, cancelable: true });
			window.dispatchEvent(e);
			expect(e.defaultPrevented, k).toBe(false);
		}
	});
});
