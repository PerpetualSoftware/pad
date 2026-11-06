// TASK-4315 PR 3b: the TopBar's Chat button.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';

const h = vi.hoisted(() => ({ unread: 0, chatOn: true, authed: true, open: vi.fn(), isOpen: false }));
vi.mock('$lib/stores/chat.svelte', () => ({
	chatStore: {
		get unreadTotal() {
			return h.unread;
		}
	}
}));
vi.mock('$lib/stores/chatSetting.svelte', () => ({
	chatSetting: {
		get enabled() {
			return h.chatOn;
		}
	}
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get authenticated() {
			return h.authed;
		}
	}
}));
vi.mock('$lib/stores/chatSurface.svelte', () => ({
	chatSurface: {
		get isOpen() {
			return h.isOpen;
		},
		open: h.open,
		handleLinkClick: (e: MouseEvent, href: string) => {
			if (e.button !== 0 || e.ctrlKey || e.metaKey) return;
			e.preventDefault();
			h.open(href);
		}
	}
}));

import ChatButton from './ChatButton.svelte';

async function mount(props: Record<string, unknown> = {}) {
	render(ChatButton, { props });
	await tick();
	return document.querySelector('a') as HTMLAnchorElement | null;
}

beforeEach(() => {
	h.unread = 0;
	h.chatOn = true;
	h.authed = true;
	h.isOpen = false;
	h.open.mockReset();
});
afterEach(cleanup);

describe('ChatButton', () => {
	it('renders nothing while Chat is off, whatever is unread', async () => {
		h.chatOn = false;
		h.unread = 4;
		expect(await mount()).toBeNull();
	});

	it('renders nothing when signed out', async () => {
		h.authed = false;
		expect(await mount()).toBeNull();
	});

	it('is a link to /messages named Chat, with no badge at 0', async () => {
		const a = (await mount())!;
		expect(a.getAttribute('href')).toBe('/messages');
		expect(a.getAttribute('aria-label')).toBe('Chat');
		expect(document.querySelector('.unread-badge')).toBeNull();
	});

	it('names the unread count and shows the badge', async () => {
		h.unread = 3;
		const a = (await mount())!;
		expect(a.getAttribute('aria-label')).toBe('Chat, 3 unread');
		expect(document.querySelector('.unread-badge')).not.toBeNull();
	});

	it('a plain click opens the surface; a modified click keeps the href', async () => {
		const a = (await mount())!;
		a.click();
		expect(h.open).toHaveBeenCalledWith('/messages');
		h.open.mockReset();
		a.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, ctrlKey: true }));
		expect(h.open).not.toHaveBeenCalled();
	});

	it('marks itself current while Chat is open', async () => {
		h.isOpen = true;
		const a = (await mount())!;
		expect(a.getAttribute('aria-current')).toBe('true');
	});

	it('the compact variant keeps the name and the badge, drops the visible label', async () => {
		h.unread = 2;
		const a = (await mount({ compact: true }))!;
		expect(a.classList.contains('compact')).toBe(true);
		expect(a.getAttribute('aria-label')).toBe('Chat, 2 unread');
		expect(a.querySelector('.chat-label')).toBeNull();
		expect(a.querySelector('.unread-badge')).not.toBeNull();
	});
});
