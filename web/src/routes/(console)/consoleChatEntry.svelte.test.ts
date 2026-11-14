vi.mock('$lib/stores/breakpoint.svelte', () => ({ viewport: { isMobile: true } }));
// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// TASK-4044 3c (U31): the / nav bar carries the Chat entry, after
// "Needs you", with the unread badge, and a click closes the mobile menu.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { flushSync } from 'svelte';

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
import MobileYouBar from '$lib/components/layout/MobileYouBar.svelte';
vi.mock('$lib/stores/ui.svelte', () => ({ uiStore: { isMobile: true, closeSearch: vi.fn() } }));
vi.mock('$lib/stores/homeOverview.svelte', () => ({ homeOverview: { needs: { total: 3 } } }));


async function mountLayout() {
	render(MobileYouBar);
	for (let i = 0; i < 50 && !document.querySelector('nav'); i++) {
		await new Promise((r) => setTimeout(r, 5));
		flushSync();
	}
}

beforeEach(async () => {
	h.chatOn = true;
	page.url = new URL('http://localhost/agenda');
	await mountLayout();
});

afterEach(() => {
	cleanup();
	document.body.innerHTML = '';
});

describe('phone You header', () => {
 it('keeps Needs you and Agenda visible with the active route and count', () => {
  const links = [...document.querySelectorAll<HTMLAnchorElement>('nav a')];
  expect(links.map(a => a.getAttribute('href'))).toEqual(['/', '/needs', '/agenda']);
  expect(links[2].getAttribute('aria-current')).toBe('page');
  expect(links[0].getAttribute('aria-current')).toBeNull();
  expect(links[1].textContent).toContain('3');
 });
 it.each([true, false])('has no top-bar Chat entry with Chat enabled: %s', async enabled => {
  cleanup(); h.chatOn = enabled; await mountLayout();
  expect(document.querySelector('a[href="/messages"]')).toBeNull();
  expect(document.querySelector('button[aria-label="Account"]')).not.toBeNull();
 });
});
