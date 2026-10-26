// TASK-4044 PR 2a-ii: the Chat link and its unread badge.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';

const h = vi.hoisted(() => ({ unread: 0, version: 0 }));
vi.mock('$lib/stores/chat.svelte', () => ({
	chatStore: {
		get unreadTotal() {
			return h.unread;
		},
		// A decoy: the badge must read unreadTotal and nothing else.
		get version() {
			return h.version;
		}
	}
}));

import ChatLink from './ChatLink.svelte';

async function mount(unread: number, props: Record<string, unknown> = {}) {
	h.unread = unread;
	h.version = 777;
	render(ChatLink, { props });
	await tick();
	return document.querySelector('a') as HTMLAnchorElement;
}

beforeEach(() => cleanup());
afterEach(() => cleanup());

describe('ChatLink (TASK-4044)', () => {
	it('links to /messages and is labelled Chat', async () => {
		const a = await mount(0);
		expect(a.getAttribute('href')).toBe('/messages');
		expect(a.textContent).toContain('Chat');
	});

	it('hides the badge at 0, with the accessible name "Chat"', async () => {
		const a = await mount(0);
		expect(document.querySelector('.unread-badge')).toBeNull();
		expect(a.getAttribute('aria-label')).toBe('Chat');
	});

	it('shows the count and names it', async () => {
		const a = await mount(5);
		expect(document.querySelector('.unread-badge')?.textContent).toBe('5');
		expect(a.getAttribute('aria-label')).toBe('Chat, 5 unread');
	});

	it('caps the badge at 99+ but names the real count', async () => {
		const a = await mount(150);
		expect(document.querySelector('.unread-badge')?.textContent).toBe('99+');
		expect(a.getAttribute('aria-label')).toBe('Chat, 150 unread');
	});

	it('passes class, role and onclick to the anchor', async () => {
		const onclick = vi.fn();
		const a = await mount(0, { class: 'menu-link', role: 'menuitem', onclick });
		expect(a.classList.contains('menu-link')).toBe(true);
		expect(a.getAttribute('role')).toBe('menuitem');
		a.addEventListener('click', (e) => e.preventDefault());
		a.click();
		expect(onclick).toHaveBeenCalledTimes(1);
	});
});
