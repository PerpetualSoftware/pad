// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
//
// TASK-2430 — the workspace overflow menu owns Escape and Up/Down on a WINDOW
// keydown handler. While a viewer is frontmost the app shell is inert, so this
// handler must not consume Escape (the viewer's) and must not `.focus()` menu
// items underneath it — which would pull focus out of the frontmost surface and
// into inert chrome.
//
// Each blocked case is paired with an EMPTY-STACK REGRESSION.
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
import { tick, flushSync } from 'svelte';

const WORKSPACES = [
	{ id: 'w1', slug: 'alpha', name: 'Alpha', owner_username: 'u' },
	{ id: 'w2', slug: 'beta', name: 'Beta', owner_username: 'u' },
	{ id: 'w3', slug: 'gamma', name: 'Gamma', owner_username: 'u' },
];

vi.mock('$lib/api/client', () => ({
	api: {
		workspaces: {
			list: vi.fn(async () => WORKSPACES),
			reorder: vi.fn(async () => {}),
		},
	},
}));

import TopBar from './TopBar.svelte';
import { workspaceStore } from '$lib/stores/workspace.svelte';
import { acquire, __resetViewerBackdropForTests } from '$lib/a11y/viewerBackdrop';

function mountViewer(): HTMLElement {
	const root = document.createElement('div');
	root.className = 'attachment-viewer';
	root.setAttribute('role', 'dialog');
	document.body.appendChild(root);
	return root;
}

/**
 * The menu's rendered rows. Matched on the dnd-rewritten role: `svelte-dnd-action`
 * overwrites the authored `role="menu"`/`menuitem"` on this zone with
 * `list`/`listitem`. That is ALSO why the component's own roving-focus query
 * (menuitem-only) matches nothing and its arrow branch does not move focus — a
 * pre-existing defect this task deliberately does not fix, so the arrow tests
 * below assert only what the handler really does today: consume the key.
 */
function menuRows(): HTMLElement[] {
	return Array.from(
		document.querySelectorAll<HTMLElement>('#workspace-overflow-menu [role="listitem"]'),
	);
}

function key(k: string): KeyboardEvent {
	return new KeyboardEvent('keydown', { key: k, bubbles: true, cancelable: true });
}

let offsetWidthSpy: PropertyDescriptor | undefined;

beforeEach(async () => {
	// jsdom lays nothing out, so the visible/overflow split would put every
	// workspace in the VISIBLE zone and never mount the overflow menu at all.
	// A tiny available width plus wide pills forces the whole set to overflow.
	vi.stubGlobal(
		'ResizeObserver',
		class {
			cb: ResizeObserverCallback;
			constructor(cb: ResizeObserverCallback) {
				this.cb = cb;
			}
			observe(target: Element) {
				this.cb(
					[{ target, contentRect: { width: 40 } } as unknown as ResizeObserverEntry],
					this as unknown as ResizeObserver,
				);
			}
			unobserve() {}
			disconnect() {}
		},
	);
	offsetWidthSpy = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'offsetWidth');
	Object.defineProperty(HTMLElement.prototype, 'offsetWidth', {
		configurable: true,
		get: () => 500,
	});

	await workspaceStore.loadAll();
	render(TopBar, { props: {} });
	// The pill measurement is rAF-deferred; poll until the menu is mounted.
	for (let i = 0; i < 50 && menuRows().length === 0; i++) {
		await new Promise((r) => setTimeout(r, 5));
		flushSync();
	}
	// Open the menu through its real trigger.
	const trigger = document.querySelector<HTMLElement>('.overflow-trigger-wrap button');
	trigger?.click();
	await tick();
	flushSync();
});

afterEach(() => {
	cleanup();
	__resetViewerBackdropForTests();
	// Restore precisely: if `offsetWidth` was NOT an own property of the
	// prototype, re-defining is wrong — delete, so the stub cannot outlive this
	// file. (`vi.restoreAllMocks()` does not undo a raw `defineProperty`.)
	if (offsetWidthSpy) {
		Object.defineProperty(HTMLElement.prototype, 'offsetWidth', offsetWidthSpy);
	} else {
		delete (HTMLElement.prototype as unknown as Record<string, unknown>).offsetWidth;
	}
	vi.unstubAllGlobals();
	vi.restoreAllMocks();
	document.body.innerHTML = '';
});

describe('TopBar overflow menu — defers to a frontmost viewer (TASK-2430)', () => {
	it('mounts an open overflow menu with rows (test fixture sanity)', () => {
		expect(menuRows().length).toBeGreaterThan(0);
		expect(document.querySelector('#workspace-overflow-menu')?.classList).toContain('open');
	});

	it('EMPTY-STACK REGRESSION: Escape still closes the menu with no lease held', async () => {
		window.dispatchEvent(key('Escape'));
		await tick();
		flushSync();
		expect(document.querySelector('#workspace-overflow-menu')?.classList).not.toContain('open');
	});

	it('EMPTY-STACK REGRESSION: ArrowDown is still CONSUMED by the open menu', () => {
		// What the handler actually does today: `preventDefault()` runs before the
		// roving-focus query, which (see `menuRows`) matches nothing. So consuming
		// the key is the whole observable effect, and it is what must not change
		// when no viewer is present.
		const before = document.activeElement;
		const e = key('ArrowDown');
		window.dispatchEvent(e);
		expect(e.defaultPrevented).toBe(true);
		expect(document.activeElement).toBe(before);
	});

	it('does not consume Escape while a viewer lease is frontmost', async () => {
		acquire(mountViewer());
		window.dispatchEvent(key('Escape'));
		await tick();
		flushSync();
		// Menu untouched — the viewer's own Escape owner gets the key.
		expect(document.querySelector('#workspace-overflow-menu')?.classList).toContain('open');
	});

	it('does not consume ArrowDown while a viewer lease is frontmost', () => {
		// The viewer pages its own images with the arrow keys, so the inert menu
		// underneath must not swallow them — nor reach the `.focus()` call that
		// follows in the handler, which would pull focus out of the viewer and
		// into inert chrome the moment the dnd role rewrite is ever fixed.
		const viewer = mountViewer();
		const inViewer = document.createElement('button');
		viewer.appendChild(inViewer);
		acquire(viewer);
		inViewer.focus();

		const e = key('ArrowDown');
		window.dispatchEvent(e);
		expect(e.defaultPrevented).toBe(false);
		expect(document.activeElement).toBe(inViewer);
	});

	it('OWNER ARGUMENT: an Escape ORIGINATING inside the viewer is still declined', async () => {
		// The discriminating case for "the argument is the SURFACE ASKING TO ACT,
		// not `event.target`". The other blocked tests dispatch on `window`,
		// where the target is outside the viewer too, so they would ALSO pass
		// with the wrong argument. Here the key is pressed with focus inside the
		// viewer — an `e.target` owner would report "not blocked" and close the
		// menu underneath it.
		const viewer = mountViewer();
		const btn = document.createElement('button');
		viewer.appendChild(btn);
		acquire(viewer);

		btn.dispatchEvent(key('Escape'));
		await tick();
		flushSync();
		expect(document.querySelector('#workspace-overflow-menu')?.classList).toContain('open');
	});

	it('takes both keys back once the lease is released', async () => {
		const lease = acquire(mountViewer());
		window.dispatchEvent(key('Escape'));
		await tick();
		flushSync();
		expect(document.querySelector('#workspace-overflow-menu')?.classList).toContain('open');

		// Both keys, as the name says: the arrow is consumed again too.
		lease.release();
		const arrow = key('ArrowDown');
		window.dispatchEvent(arrow);
		expect(arrow.defaultPrevented).toBe(true);

		window.dispatchEvent(key('Escape'));
		await tick();
		flushSync();
		expect(document.querySelector('#workspace-overflow-menu')?.classList).not.toContain('open');
	});
});
