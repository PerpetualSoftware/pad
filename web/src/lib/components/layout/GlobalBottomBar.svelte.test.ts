// TASK-4315 PR 4: the compact phone bar outside a workspace (console, workspace
// picker, standalone /messages): Workspaces, Chat, You.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick, flushSync } from 'svelte';
import { cs, parked, chatSurfaceMock } from './chatSurfaceMock.fixture.svelte';
import { kb } from '../../chat/uiKeyboard.fixture.svelte';

const h = vi.hoisted(() => ({ chatOn: true, authed: true, hasWorkspace: true, goto: vi.fn() }));
vi.mock('$lib/stores/homeOverview.svelte', () => ({ homeOverview: { needs: { total: 7 }, load: vi.fn(), arrive: vi.fn() } }));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { get current() { return h.hasWorkspace ? { slug: 'ws', owner_username: 'u', name: 'WS' } : null; } } }));
vi.mock('$lib/components/layout/WorkspaceSheet.svelte', async () => await import('./bottomNavSheetStub.fixture.svelte'));
vi.mock('$lib/components/layout/QuickCaptureSheet.svelte', async () => await import('./bottomNavSheetStub.fixture.svelte'));
vi.mock('$app/state', () => ({ page: { url: new URL('http://localhost/') } }));
vi.mock('$app/navigation', () => ({ goto: h.goto, afterNavigate: vi.fn() }));
vi.mock('$lib/stores/ui.svelte', () => ({
	uiStore: { closeSearch: vi.fn(), openSearch: vi.fn(), onNavigate: vi.fn(),
		get isMobile() {
			return kb.mobile;
		},
		get keyboardVisible() {
			return kb.visible;
		}
	}
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get authenticated() {
			return h.authed;
		},
		userId: 'u1',
		onIdentityChange: () => () => {}
	}
}));
vi.mock('$lib/stores/chatSurface.svelte', async () => ({
	chatSurface: (await import('./chatSurfaceMock.fixture.svelte')).chatSurfaceMock
}));
vi.mock('$lib/stores/chatSetting.svelte', () => ({
	chatSetting: {
		get enabled() {
			return h.chatOn;
		}
	}
}));

import GlobalBottomBar from './GlobalBottomBar.svelte';
import { chatStore } from '$lib/stores/chat.svelte';

const items = () => [...document.querySelectorAll<HTMLElement>('nav.bottom-nav .bn-item')];
const label = (b: HTMLElement) => b.getAttribute('aria-label') ?? b.querySelector('.bn-label')?.textContent;

beforeEach(() => {
	cleanup();
	h.chatOn = true;
	h.authed = true; h.hasWorkspace = true;
	h.goto.mockClear();
	kb.mobile = true;
	kb.visible = false;
	cs.open = false;
	parked.length = 0;
	chatSurfaceMock.open.mockClear();
	chatSurfaceMock.closeThen.mockClear();
	chatStore.unreadTotal = 0;
	document.body.className = '';
});
afterEach(() => {
	cleanup();
	kb.mobile = false;
	kb.visible = false;
	chatStore.unreadTotal = 0;
});

describe('GlobalBottomBar', () => {
	it('shares the five-slot phone bar', async () => {
		render(GlobalBottomBar);
		await tick();
		expect(items().map(label)).toEqual(['Home', 'Workspace', 'Quick capture', 'Chat', 'Search']);
		expect(document.querySelector<HTMLAnchorElement>('nav.bottom-nav a')!.getAttribute('href')).toBe('/');
		expect(document.querySelector('nav.bottom-nav')!.getAttribute('aria-label')).toBe('Primary');
	});

	it('carries the unread badge on Chat', async () => {
		chatStore.unreadTotal = 4;
		render(GlobalBottomBar);
		await tick();
		const chat = document.querySelector<HTMLElement>('[data-chat-slot]')!;
		expect(chat.getAttribute('aria-label')).toBe('Chat, 4 unread');
		expect(chat.querySelector('.unread-badge')?.textContent).toBe('4');
	});

	it('has no Chat slot when the Chat setting is off', async () => {
		h.chatOn = false;
		render(GlobalBottomBar);
		await tick();
		expect(items().map(label)).toEqual(['Home', 'Workspace', 'Quick capture', 'Search']);
	});

	it('renders nothing when signed out', async () => {
		h.authed = false;
		render(GlobalBottomBar);
		await tick();
		expect(document.querySelector('nav')).toBeNull();
	});

	it('renders nothing on a wide screen', async () => {
		kb.mobile = false;
		render(GlobalBottomBar);
		await tick();
		expect(document.querySelector('nav')).toBeNull();
	});

	it('hides while the keyboard is up and returns after', async () => {
		render(GlobalBottomBar);
		await tick();
		kb.visible = true;
		flushSync();
		expect(document.querySelector('nav')).toBeNull();
		expect(document.body.classList.contains('has-bottom-nav')).toBe(false);
		kb.visible = false;
		flushSync();
		expect(document.querySelector('nav.bottom-nav')).not.toBeNull();
		expect(document.body.classList.contains('has-bottom-nav')).toBe(true);
	});

	it('marks the body while shown and clears it when unmounted', async () => {
		const { unmount } = render(GlobalBottomBar);
		await tick();
		expect(document.body.classList.contains('has-bottom-nav')).toBe(true);
		unmount();
		expect(document.body.classList.contains('has-bottom-nav')).toBe(false);
	});

	it('Chat opens the Chat sheet', async () => {
		render(GlobalBottomBar);
		await tick();
		document.querySelector<HTMLElement>('[data-chat-slot]')!.click();
		expect(chatSurfaceMock.open).toHaveBeenCalledWith('/messages');
	});

	it('Workspace waits for an open Chat to close, then opens', async () => {
		cs.open = true;
		render(GlobalBottomBar);
		await tick();
		const you = items().find((b) => label(b) === 'Workspace')!;
		you.click();
		flushSync();
		expect(you.getAttribute('aria-expanded')).toBe('false');
		cs.open = false;
		parked.shift()!();
		flushSync();
		expect(you.getAttribute('aria-expanded')).toBe('true');
	});

	it('Home waits for an open Chat to close, then goes', async () => {
		cs.open = true;
		render(GlobalBottomBar);
		await tick();
		document.querySelector<HTMLAnchorElement>('nav.bottom-nav a')!.click();
		expect(h.goto).not.toHaveBeenCalled();
		parked.shift()!();
		expect(h.goto).toHaveBeenCalledWith('/');
	});
});

it('Workspace goes to the picker when no workspace exists', async () => {
 h.hasWorkspace = false;
 render(GlobalBottomBar); await tick();
 items().find(b => label(b) === 'Workspace')!.click();
 expect(h.goto).toHaveBeenCalledWith('/');
});
