// TASK-4315 PR 4: the phone bottom bar. Slots are Workspace, Search, plus, Chat, You;
// Activity moved into the Workspace sheet. The Chat slot opens the Chat sheet and
// carries the unread badge (it moved from You, TASK-4186).
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick, flushSync } from 'svelte';
import { cs, parked, chatSurfaceMock } from './chatSurfaceMock.fixture.svelte';

const h = vi.hoisted(() => ({ chatOn: true, path: '/u/ws' }));
vi.mock('$app/state', () => ({
	page: {
		get url() {
			return new URL(`http://localhost${h.path}`);
		}
	}
}));
vi.mock('$app/navigation', () => ({ goto: vi.fn(), afterNavigate: vi.fn() }));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { current: { slug: 'ws', owner_username: 'u', name: 'WS' } }
}));
vi.mock('$lib/stores/ui.svelte', () => ({
	uiStore: {
		isMobile: true,
		keyboardVisible: false,
		searchOpen: false,
		closeSearch: vi.fn(),
		openSearch: vi.fn(),
		onNavigate: vi.fn()
	}
}));
vi.mock('$lib/components/layout/QuickCaptureSheet.svelte', async () => await import('./bottomNavSheetStub.fixture.svelte'));
vi.mock('$lib/components/layout/WorkspaceSheet.svelte', async () => await import('./bottomNavSheetStub.fixture.svelte'));
vi.mock('$lib/components/layout/YouSheet.svelte', async () => await import('./bottomNavSheetStub.fixture.svelte'));
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

import BottomNav from './BottomNav.svelte';
import { chatStore } from '$lib/stores/chat.svelte';

const slots = () => [...document.querySelectorAll<HTMLElement>('.bottom-nav .bn-item')];
const slot = (text: string) => slots().find((b) => (b.getAttribute('aria-label') ?? b.textContent ?? '').includes(text))!;
const chatSlot = () => document.querySelector<HTMLButtonElement>('.bottom-nav [data-chat-slot]')!;
const youSlot = () => slot('You');

beforeEach(() => {
	cleanup();
	h.chatOn = true;
	h.path = '/u/ws';
	cs.open = false;
	parked.length = 0;
	chatSurfaceMock.open.mockClear();
	chatSurfaceMock.closeThen.mockClear();
	chatStore.unreadTotal = 0;
});
afterEach(() => {
	cleanup();
	chatStore.unreadTotal = 0;
});

describe('BottomNav slots', () => {
	it('are Workspace, Search, Quick capture, Chat, You, in that order, with no Activity', async () => {
		render(BottomNav);
		await tick();
		const names = slots().map((b) => b.getAttribute('aria-label')?.replace(/,.*/, '') ?? b.querySelector('.bn-label')?.textContent);
		expect(names).toEqual(['Workspace', 'Search', 'Quick capture', 'Chat', 'You']);
		expect(document.querySelector('.bottom-nav a[href$="/activity"]')).toBeNull();
		expect(document.querySelector('.bottom-nav')!.textContent).not.toContain('Activity');
	});

	it('Workspace is lit on the activity page', async () => {
		h.path = '/u/ws/activity';
		render(BottomNav);
		await tick();
		expect(slot('Workspace').classList.contains('active')).toBe(true);
	});
});

describe('BottomNav Chat slot (TASK-4186, TASK-4315)', () => {
	it('is a button naming the dialog it opens, with the unread count in its label', async () => {
		chatStore.unreadTotal = 3;
		render(BottomNav);
		await tick();
		const chat = chatSlot();
		expect(chat.tagName).toBe('BUTTON');
		expect(chat.getAttribute('aria-haspopup')).toBe('dialog');
		expect(chat.getAttribute('aria-label')).toBe('Chat, 3 unread');
		expect(chat.querySelector('.unread-badge')?.textContent).toBe('3');
	});

	it('the You slot no longer carries the badge or a chat label', async () => {
		chatStore.unreadTotal = 3;
		render(BottomNav);
		await tick();
		expect(youSlot().querySelector('.unread-badge')).toBeNull();
		expect(youSlot().getAttribute('aria-label')).toBeNull();
		expect(document.querySelectorAll('.bottom-nav .unread-badge')).toHaveLength(1);
	});

	it('shows no badge and a plain label at zero', async () => {
		render(BottomNav);
		await tick();
		expect(chatSlot().querySelector('.unread-badge')).toBeNull();
		expect(chatSlot().getAttribute('aria-label')).toBe('Chat');
	});

	it('follows a live change from 0 to 2', async () => {
		render(BottomNav);
		await tick();
		chatStore.unreadTotal = 2;
		flushSync();
		expect(chatSlot().querySelector('.unread-badge')?.textContent).toBe('2');
		expect(chatSlot().getAttribute('aria-label')).toBe('Chat, 2 unread');
	});

	it('shows no badge while the Chat setting is off', async () => {
		h.chatOn = false;
		chatStore.unreadTotal = 3;
		render(BottomNav);
		await tick();
		expect(document.querySelectorAll('.bottom-nav .unread-badge')).toHaveLength(0);
		expect(chatSlot().getAttribute('aria-label')).toBe('Chat');
	});

	it('tapping it opens Chat through the surface', async () => {
		render(BottomNav);
		await tick();
		chatSlot().click();
		expect(chatSurfaceMock.open).toHaveBeenCalledWith('/messages');
	});

	it('tapping it closes the other sheets', async () => {
		render(BottomNav);
		await tick();
		youSlot().click();
		flushSync();
		expect(youSlot().getAttribute('aria-expanded')).toBe('true');
		chatSlot().click();
		flushSync();
		expect(youSlot().getAttribute('aria-expanded')).toBe('false');
	});

	it('is lit and expanded while the Chat sheet is open', async () => {
		render(BottomNav);
		await tick();
		expect(chatSlot().getAttribute('aria-expanded')).toBe('false');
		expect(chatSlot().classList.contains('active')).toBe(false);
		cs.open = true;
		flushSync();
		expect(chatSlot().getAttribute('aria-expanded')).toBe('true');
		expect(chatSlot().classList.contains('active')).toBe(true);
	});
});

describe('BottomNav while Chat is open', () => {
	it('You waits for Chat to close, then opens', async () => {
		cs.open = true;
		render(BottomNav);
		await tick();
		youSlot().click();
		flushSync();
		expect(chatSurfaceMock.closeThen).toHaveBeenCalledTimes(1);
		expect(youSlot().getAttribute('aria-expanded')).toBe('false');
		cs.open = false;
		parked.shift()!();
		flushSync();
		expect(youSlot().getAttribute('aria-expanded')).toBe('true');
	});

	it('Workspace and Quick capture wait the same way', async () => {
		cs.open = true;
		render(BottomNav);
		await tick();
		slot('Workspace').click();
		slot('Quick capture').click();
		flushSync();
		expect(chatSurfaceMock.closeThen).toHaveBeenCalledTimes(2);
		expect(slot('Workspace').getAttribute('aria-expanded')).toBe('false');
		expect(slot('Quick capture').getAttribute('aria-expanded')).toBe('false');
	});

	it('with Chat closed a slot opens straight away, without closeThen holding it', async () => {
		render(BottomNav);
		await tick();
		youSlot().click();
		flushSync();
		expect(youSlot().getAttribute('aria-expanded')).toBe('true');
		expect(parked).toHaveLength(0);
	});
});
