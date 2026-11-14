vi.mock('$lib/stores/breakpoint.svelte', () => ({ viewport: { isMobile: true } }));
// The Account sheet retains the console menu's Escape arbitration coverage.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick, flushSync } from 'svelte';

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
		// The page's sign-out asks through confirmDialog (BUG-3571), which
		// listens for identity changes when it loads.
		onIdentityChange: () => () => {},
	},
}));

import AccountButton from '$lib/components/layout/AccountButton.svelte';
vi.mock('$lib/stores/ui.svelte', () => ({ uiStore: { isMobile: true, closeSearch: vi.fn() } }));
import { acquire, __resetViewerBackdropForTests } from '$lib/a11y/viewerBackdrop';


/**
 * The avatar exposes the sheet state, so drive it through its real toggle and read
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
	render(AccountButton);
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

describe('Account sheet Escape — defers to a frontmost surface (TASK-2430)', () => {
	it('opens Account through its real toggle (fixture sanity)', () => {
		expect(menuOpen()).toBe(true);
	});

	it('EMPTY-STACK REGRESSION: Escape still closes Account', async () => {
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

// jsdom does not provide the Web Animations API used by sheet transitions.
if (!('animate' in Element.prototype)) {
	(Element.prototype as unknown as { animate: unknown }).animate = function () {
		const animation = {
			onfinish: null as null | (() => void),
			cancel() {},
			finish() {},
			currentTime: 0,
			addEventListener() {},
			finished: Promise.resolve()
		};
		queueMicrotask(() => animation.onfinish?.());
		return animation;
	};
}
