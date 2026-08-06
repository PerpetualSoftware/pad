// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// TASK-2430 — the ROOT app-shell shortcuts in `+layout.svelte` were entirely
// unguarded. Every one of them mutates shell state that sits UNDER a frontmost
// viewer, and `isInputFocused()` cannot help: it only recognises text controls,
// so a focused viewer BUTTON reads as "not typing" and each shortcut fires
// straight through the viewer.
//
// The layout's `<svelte:window onkeydown>` is declared OUTSIDE its `authReady`
// gate, so the handler is live from mount even though the app shell itself
// renders nothing here — which is exactly the surface under test.
//
// Every blocked case is paired with an EMPTY-STACK REGRESSION.
//
// HOW TO READ THE PAIRS. The BLOCKED tests are what fail if a guard is deleted
// or weakened. The EMPTY-STACK REGRESSIONS are the opposite check — they fail
// if a guard declines UNCONDITIONALLY, which is the way a "deference" change
// silently breaks the app for the 99% of the time no viewer is open. Neither
// half subsumes the other, and an empty-stack test passing with the guard
// removed is by design, not a false green. Every guard in the files under test
// was mutation-verified to kill at least one case here (one documented
// exception, flagged at the guard itself).
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { createRawSnippet, tick, flushSync } from 'svelte';

// The layout's children and its whole chrome are irrelevant to the shortcut
// handler; stub the network so `onMount`'s auth probe simply fails closed.
vi.mock('$lib/api/client', () => {
	const never = () => new Promise(() => {});
	return {
		api: new Proxy({}, { get: () => new Proxy({}, { get: () => never }) }),
		setAccessRevokedHandler: () => {},
		setRateLimitHandler: () => {},
		isPlanLimitError: () => false,
		planLimitMessage: () => '',
	};
});

import Layout from './+layout.svelte';
import { uiStore } from '$lib/stores/ui.svelte';
import { acquire, __resetViewerBackdropForTests } from '$lib/a11y/viewerBackdrop';

const childSnippet = createRawSnippet(() => ({ render: () => `<div>child</div>` }));

function mountViewer(): HTMLElement {
	const root = document.createElement('div');
	root.className = 'attachment-viewer';
	root.setAttribute('role', 'dialog');
	document.body.appendChild(root);
	return root;
}

function press(key: string, mod = false): KeyboardEvent {
	const e = new KeyboardEvent('keydown', {
		key,
		metaKey: mod,
		bubbles: true,
		cancelable: true,
	});
	window.dispatchEvent(e);
	return e;
}

beforeEach(async () => {
	render(Layout, { props: { children: childSnippet } });
	await tick();
	flushSync();
	if (uiStore.searchOpen) uiStore.closeSearch();
});

afterEach(() => {
	cleanup();
	__resetViewerBackdropForTests();
	if (uiStore.searchOpen) uiStore.closeSearch();
	vi.restoreAllMocks();
	document.body.innerHTML = '';
});

describe('+layout app-shell shortcuts — defer to a frontmost surface (TASK-2430)', () => {
	it('EMPTY-STACK REGRESSION: Cmd+K still opens the search palette', () => {
		const e = press('k', true);
		flushSync();
		expect(e.defaultPrevented).toBe(true);
		expect(uiStore.searchOpen).toBe(true);
	});

	it('EMPTY-STACK REGRESSION: Cmd+\\ still toggles the sidebar', () => {
		const before = uiStore.sidebarOpen;
		const e = press('\\', true);
		flushSync();
		expect(e.defaultPrevented).toBe(true);
		expect(uiStore.sidebarOpen).toBe(!before);
	});

	it('declines EVERY shortcut this handler owns while a viewer lease is frontmost', () => {
		// All of them, not a sample: a per-key guard (rather than the single
		// handler-level bail) would let whichever keys it missed straight
		// through, and a sampled test would not see it.
		acquire(mountViewer());
		const before = uiStore.sidebarOpen;
		const topbarBefore = uiStore.topbarOpen;

		for (const [key, mod] of [
			['k', true],
			['\\', true],
			[']', true],
			['n', true],
			['f', true],
			['?', false],
			['Escape', false],
		] as const) {
			expect({ key, prevented: press(key, mod).defaultPrevented }).toEqual({
				key,
				prevented: false,
			});
		}
		flushSync();

		expect(uiStore.searchOpen).toBe(false);
		expect(uiStore.sidebarOpen).toBe(before);
		expect(uiStore.topbarOpen).toBe(topbarBefore);
	});

	it('OWNER ARGUMENT: a keydown ORIGINATING inside the viewer is still declined', () => {
		// The discriminating case for "the argument is the SURFACE ASKING TO ACT,
		// not `event.target`". Every other blocked test here dispatches on
		// `window`, where `event.target` is outside the viewer too — so they
		// would ALSO pass if the handler wrongly passed `e.target`. Here the key
		// is typed with focus inside the viewer, which is the realistic case: an
		// `e.target` owner would report "not blocked" and fire the shortcut.
		const viewer = mountViewer();
		const btn = document.createElement('button');
		viewer.appendChild(btn);
		acquire(viewer);

		const e = new KeyboardEvent('keydown', {
			key: 'k',
			metaKey: true,
			bubbles: true,
			cancelable: true,
		});
		btn.dispatchEvent(e);
		flushSync();
		expect(e.defaultPrevented).toBe(false);
		expect(uiStore.searchOpen).toBe(false);
	});

	it('a focused viewer BUTTON is not enough on its own — the lease is what blocks', () => {
		// `isInputFocused()` returns false for a button, so this is precisely the
		// case the old handler got wrong: focus inside the viewer, shortcut fires.
		const viewer = mountViewer();
		const btn = document.createElement('button');
		viewer.appendChild(btn);
		const lease = acquire(viewer);
		btn.focus();

		expect(press('k', true).defaultPrevented).toBe(false);
		flushSync();
		expect(uiStore.searchOpen).toBe(false);

		// Same focused button, no lease → the shortcut works again, proving the
		// guard keys off lease state and not off "something is focused".
		lease.release();
		expect(press('k', true).defaultPrevented).toBe(true);
		flushSync();
		expect(uiStore.searchOpen).toBe(true);
	});

	// NOT PROVABLE HERE — the NATIVE `showModal()` branch of the precedence rule
	// ("a top-layer dialog wins outright") needs a real engine. jsdom implements
	// neither the top layer nor the `:modal` pseudo-class (verified: the
	// selector THROWS, which is exactly the unsupported path `isBlockedByModal`
	// answers "no native modal" for), and `setup-jsdom.ts` only fakes
	// `showModal()` by reflecting the `open` attribute. A jsdom test here would
	// assert the fallback, not the contract, and would pass whatever the guard
	// did — so there is no test here at all rather than an assertion-free
	// placeholder. That guarantee belongs to TASK-2436's Playwright suite,
	// alongside the inertness and stacking guarantees jsdom cannot see either.
	// (`viewerBackdrop.svelte.test.ts` does fence the helper's own native branch
	// by emulating a supporting engine; what cannot be reproduced here is the
	// real top layer this handler would sit beneath.)
});
