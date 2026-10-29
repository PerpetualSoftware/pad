// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// TASK-4044 3c (U31): the /console nav bar carries the Chat entry, after
// "Needs you", with the unread badge, and a click closes the mobile menu.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { createRawSnippet, tick, flushSync } from 'svelte';

vi.mock('$lib/api/client', () => {
	const never = () => new Promise(() => {});
	return {
		api: new Proxy({}, { get: () => new Proxy({}, { get: () => never }) })
	};
});
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		load: async () => ({ authenticated: true }),
		user: { name: 'E2E', email: 'e2e@example.com', role: 'admin' },
		cloudMode: false,
		authenticated: true,
		onIdentityChange: () => () => {}
	}
}));
vi.mock('$lib/stores/chat.svelte', () => ({ chatStore: { unreadTotal: 3 } }));

import ConsoleLayout from './+layout.svelte';

const childSnippet = createRawSnippet(() => ({ render: () => `<div>child</div>` }));

beforeEach(async () => {
	render(ConsoleLayout, { props: { children: childSnippet } });
	for (let i = 0; i < 50 && !document.querySelector('.nav-links'); i++) {
		await new Promise((r) => setTimeout(r, 5));
		flushSync();
	}
});

afterEach(() => {
	cleanup();
	document.body.innerHTML = '';
});

describe('console nav Chat entry (TASK-4044 3c)', () => {
	it('is a nav-link to /messages right after "Needs you", with the unread count', () => {
		const links = [...document.querySelectorAll<HTMLAnchorElement>('.nav-links a')];
		const needs = links.findIndex((a) => a.getAttribute('href') === '/console/needs');
		const chat = links.findIndex((a) => a.getAttribute('href') === '/messages');
		expect(needs).toBeGreaterThanOrEqual(0);
		expect(chat).toBe(needs + 1);
		expect(links[chat].classList.contains('nav-link')).toBe(true);
		expect(links[chat].getAttribute('aria-label')).toBe('Chat, 3 unread');
		expect(links[chat].textContent).toContain('Chat');
	});

	it('closes the mobile menu on click', async () => {
		const toggle = document.querySelector<HTMLElement>('[aria-expanded]')!;
		toggle.click();
		await tick();
		flushSync();
		expect(toggle.getAttribute('aria-expanded')).toBe('true');
		const a = document.querySelector<HTMLAnchorElement>('.nav-links a[href="/messages"]')!;
		a.addEventListener('click', (e) => e.preventDefault());
		a.click();
		await tick();
		flushSync();
		expect(toggle.getAttribute('aria-expanded')).toBe('false');
	});
});
