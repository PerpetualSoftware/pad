// TASK-4186 PR A: the chat unread badge on the mobile bottom bar's You slot.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick, flushSync } from 'svelte';

vi.mock('$app/state', () => ({ page: { url: new URL('http://localhost/u/ws') } }));
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

import BottomNav from './BottomNav.svelte';
import { chatStore } from '$lib/stores/chat.svelte';

const youSlot = () =>
	[...document.querySelectorAll<HTMLButtonElement>('.bottom-nav button.bn-item')].find((b) =>
		b.textContent?.includes('You')
	)!;

beforeEach(() => {
	cleanup();
	chatStore.unreadTotal = 0;
});
afterEach(() => cleanup());

describe('BottomNav You-slot chat badge (TASK-4186)', () => {
	it('shows the unread count on the You slot and in its label', async () => {
		chatStore.unreadTotal = 3;
		render(BottomNav);
		await tick();
		const you = youSlot();
		expect(you.querySelector('.unread-badge')?.textContent).toBe('3');
		expect(you.getAttribute('aria-label')).toBe('You, 3 unread chat');
	});

	it('shows no badge and no count in the label at zero', async () => {
		render(BottomNav);
		await tick();
		const you = youSlot();
		expect(you.querySelector('.unread-badge')).toBeNull();
		expect(you.getAttribute('aria-label') ?? 'You').toBe('You');
	});

	it('follows a live change from 0 to 2', async () => {
		render(BottomNav);
		await tick();
		expect(youSlot().querySelector('.unread-badge')).toBeNull();
		chatStore.unreadTotal = 2;
		flushSync();
		expect(youSlot().querySelector('.unread-badge')?.textContent).toBe('2');
		expect(youSlot().getAttribute('aria-label')).toBe('You, 2 unread chat');
	});

	it('only the You slot carries a badge', async () => {
		chatStore.unreadTotal = 3;
		render(BottomNav);
		await tick();
		expect(document.querySelectorAll('.bottom-nav .unread-badge')).toHaveLength(1);
	});
});
