// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// TASK-2430 (round-2 sweep) — the console shell owns a global Escape that
// closes its collapsible nav. (The nav's toggle is CSS-hidden above the mobile
// breakpoint, but the gating is PURELY CSS — there is no JS media query in this
// component — so the handler's behaviour under test is identical at any width,
// and no media stub is needed. Nothing here claims to render at a mobile size.)
//
// No attachment viewer can exist on console routes (no
// `ItemDetail`, so no viewer host), but the root layout DOES mount
// `CreateWorkspaceModal` / `OpenChildrenDialog` here, so a native dialog can be
// in front of this handler — and one press would then close the nav underneath
// it as well. This is the same one-owner-per-Escape rule as the seven owners,
// applied to a route the original owner list did not enumerate.
//
// HOW TO READ THE PAIRS: the BLOCKED test fails if the guard is deleted; the
// EMPTY-STACK REGRESSION fails if it declines unconditionally.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { createRawSnippet, tick, flushSync } from 'svelte';

vi.mock('$lib/api/client', () => {
	const never = () => new Promise(() => {});
	return {
		api: new Proxy({}, { get: () => new Proxy({}, { get: () => never }) }),
	};
});

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		load: async () => ({ authenticated: true }),
		user: { name: 'E2E', email: 'e2e@example.com', role: 'admin' },
		cloudMode: false,
		authenticated: true,
	},
}));

import ConsoleLayout from './+layout.svelte';
import { acquire, __resetViewerBackdropForTests } from '$lib/a11y/viewerBackdrop';

const childSnippet = createRawSnippet(() => ({ render: () => `<div>child</div>` }));

/**
 * The nav has no exported state, so drive it through its real toggle and read
 * the toggle's `aria-expanded` — the user-visible truth.
 */
function toggle(): HTMLElement {
	const el = document.querySelector<HTMLElement>('[aria-expanded]');
	if (!el) throw new Error('menu toggle not found');
	return el;
}

function menuOpen(): boolean {
	return toggle().getAttribute('aria-expanded') === 'true';
}

function mountViewer(): HTMLElement {
	const root = document.createElement('div');
	root.className = 'attachment-viewer';
	root.setAttribute('role', 'dialog');
	document.body.appendChild(root);
	return root;
}

beforeEach(async () => {
	render(ConsoleLayout, { props: { children: childSnippet } });
	// `{#if ready}` waits on the auth probe; poll until the shell renders.
	for (let i = 0; i < 50 && !document.querySelector('[aria-expanded]'); i++) {
		await new Promise((r) => setTimeout(r, 5));
		flushSync();
	}
	toggle().click();
	await tick();
	flushSync();
});

afterEach(() => {
	cleanup();
	__resetViewerBackdropForTests();
	vi.restoreAllMocks();
	document.body.innerHTML = '';
});

describe('console shell Escape — defers to a frontmost surface (TASK-2430)', () => {
	it('opens the nav through its real toggle (fixture sanity)', () => {
		expect(menuOpen()).toBe(true);
	});

	it('EMPTY-STACK REGRESSION: Escape still closes the nav', async () => {
		window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
		await tick();
		flushSync();
		expect(menuOpen()).toBe(false);
	});

	it('declines Escape while a lease is frontmost, and takes it back on release', async () => {
		const lease = acquire(mountViewer());
		window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
		await tick();
		flushSync();
		expect(menuOpen()).toBe(true);

		lease.release();
		window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
		await tick();
		flushSync();
		expect(menuOpen()).toBe(false);
	});
});
