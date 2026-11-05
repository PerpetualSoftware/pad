// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// TASK-4044 3c (U31): the /console nav bar carries the Chat entry, after
// "Needs you", with the unread badge, and a click closes the mobile menu.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { createRawSnippet, tick, flushSync } from 'svelte';

const h = vi.hoisted(() => ({ chatOn: true }));
vi.mock('$lib/api/client', () => {
	const never = () => new Promise(() => {});
	return {
		api: new Proxy({}, { get: () => new Proxy({}, { get: () => never }) })
	};
});
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		load: async () => ({ authenticated: true }),
		get user() {
			return { name: 'E2E', email: 'e2e@example.com', role: 'admin', chat_enabled: h.chatOn };
		},
		cloudMode: false,
		authenticated: true,
		onIdentityChange: () => () => {}
	}
}));
vi.mock('$lib/stores/chat.svelte', () => ({ chatStore: { unreadTotal: 3 } }));

import { page } from '$app/state';
import ConsoleLayout from './+layout.svelte';

const childSnippet = createRawSnippet(() => ({ render: () => `<div>child</div>` }));

async function mountLayout() {
	render(ConsoleLayout, { props: { children: childSnippet } });
	for (let i = 0; i < 50 && !document.querySelector('.nav-links'); i++) {
		await new Promise((r) => setTimeout(r, 5));
		flushSync();
	}
}

beforeEach(async () => {
	h.chatOn = true;
	page.url = new URL('http://localhost/console/agenda');
	await mountLayout();
});

afterEach(() => {
	cleanup();
	document.body.innerHTML = '';
});

describe('console nav Chat entry (TASK-4044 3c)', () => {
	it('is a nav-link to /messages after Agenda and "Needs you", with the unread count', () => {
		const links = [...document.querySelectorAll<HTMLAnchorElement>('.nav-links a')];
		const needs = links.findIndex((a) => a.getAttribute('href') === '/console/needs');
		const chat = links.findIndex((a) => a.getAttribute('href') === '/messages');
		expect(needs).toBeGreaterThanOrEqual(0);
		const agenda = links.findIndex((a) => a.getAttribute('href') === '/console/agenda');
		expect(agenda).toBe(needs + 1);
		expect(chat).toBe(agenda + 1);
		expect(links[agenda].getAttribute('aria-current')).toBe('page');
		expect(links[agenda].classList.contains('active')).toBe(true);
		const workspaces = links.find((a) => a.getAttribute('href') === '/console')!;
		expect(workspaces.getAttribute('aria-current')).toBeNull();
		expect(workspaces.classList.contains('active')).toBe(false);
		expect(links[chat].classList.contains('nav-link')).toBe(true);
		expect(links[chat].getAttribute('aria-label')).toBe('Chat, 3 unread');
		expect(links[chat].textContent).toContain('Chat');
	});

	it('has no Chat entry while the setting is off (TASK-4315)', async () => {
		cleanup();
		document.body.innerHTML = '';
		h.chatOn = false;
		await mountLayout();
		expect(document.querySelector('.nav-links a[href="/console/needs"]')).not.toBeNull();
		expect(document.querySelector('.nav-links a[href="/messages"]')).toBeNull();
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
