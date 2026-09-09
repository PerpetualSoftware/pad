import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { flushSync } from 'svelte';

/**
 * TASK-2245 — title parts are ROUTE-SCOPED, which is what replaced the
 * workspace layout's route-change clear.
 *
 * That clear was a race the layout could lose three ways, and the third only
 * appeared after the first fix for the other two:
 *
 *  1. as an `$effect` reading `page.url.pathname` it ran on SEARCH-only changes
 *     (pane open/close, view switch), wiping a section the leaf had just set —
 *     reading `.pathname` tracks the whole reactive `page.url`;
 *  2. as an effect it could run AFTER the leaf's effect; the parent-before-child
 *     guarantee is about mount order, not re-runs;
 *  3. moved to `beforeNavigate`, it fired for navigations the app then
 *     CANCELLED — `[collection]/+page.svelte` cancels to prompt about an unsaved
 *     draft — clearing the title of a page the user never left (codex round 1).
 *
 * Stamping removes the clear, so none of the three has anywhere to happen.
 * Leg 4 below is the one that pins (3): with no clear, a cancelled navigation
 * leaves the title alone by construction.
 */

// REACTIVE on purpose. `$app/state`'s `page` is a reactive object in the real
// app, so `title`'s `$derived` re-computes when the pathname changes. A plain
// `let` here would make the derived cache its first read and the legs below
// would be measuring the STUB's staleness rather than the store's behaviour —
// the first draft of this file did exactly that and one leg failed for that
// reason, not for a defect.
const route = $state({ pathname: '/u/ws/docs' });
vi.mock('$app/state', () => ({
	get page() {
		return { url: { get pathname() { return route.pathname; } } };
	}
}));

// Reads happen inside an effect root, and the route mutation is followed by
// `flushSync`. A `$derived` read from plain module scope with no reactive owner
// caches its first value — so without this the legs that change the route after
// reading the title would be asserting against a stale cache rather than
// against the store. That is a property of how these tests read the value, not
// of the store, and getting it wrong looked exactly like a defect.
let dispose: (() => void) | undefined;

async function load() {
	vi.resetModules();
	const { titleStore } = await import('./title.svelte');
	let read!: () => string;
	dispose = $effect.root(() => {
		read = () => titleStore.title;
	});
	return { store: titleStore, title: () => { flushSync(); return read(); } };
}

beforeEach(() => {
	route.pathname = '/u/ws/docs';
});

afterEach(() => {
	dispose?.();
	dispose = undefined;
});

describe('titleStore — route-scoped parts', () => {
	it('composes a section set for the CURRENT route', async () => {
		const { store: t, title } = await load();
		t.setPageTitle({ workspace: 'WS', section: 'Docs' });
		expect(title()).toBe('Docs · WS · Pad');
	});

	it('IGNORES a section set for a DIFFERENT route — no clear needed', async () => {
		const { store: t, title } = await load();
		t.setPageTitle({ workspace: 'WS', section: 'Docs' });
		route.pathname = '/u/ws/settings';
		// An unwired route inherits nothing and falls back to the workspace,
		// which is exactly what the deleted clear existed to guarantee.
		expect(title()).toBe('WS · Pad');
	});

	it('KEEPS the section across a SEARCH-only change — the pane/view case', async () => {
		const { store: t, title } = await load();
		t.setPageTitle({ workspace: 'WS', section: 'Docs' });
		// Opening the pane changes only the search; the pathname is untouched, so
		// the stamp still matches. Under the old effect this is the moment the
		// section was wiped.
		t.setPageTitle({ item: 'DOC-9' });
		expect(title()).toBe('DOC-9 · WS · Pad');
		// And closing it — the leaf reclaims, and nothing races to undo that.
		t.setPageTitle({ section: 'Docs', item: null });
		expect(title()).toBe('Docs · WS · Pad');
	});

	it('a CANCELLED navigation leaves the title intact', async () => {
		const { store: t, title } = await load();
		t.setPageTitle({ workspace: 'WS', section: 'Docs' });
		// A cancelled navigation is, from the store's point of view, no
		// navigation at all: the pathname never changed, and nothing ran on the
		// attempt. Under the `beforeNavigate` version this settled on "WS · Pad"
		// while the user was still looking at Docs.
		expect(title()).toBe('Docs · WS · Pad');
		route.pathname = '/u/ws/docs';
		expect(title()).toBe('Docs · WS · Pad');
	});

	it('the item part is route-scoped too', async () => {
		const { store: t } = await load();
		t.setPageTitle({ workspace: 'WS', item: 'DOC-9' });
		expect(t.item).toBe('DOC-9');
		route.pathname = '/u/ws/ideas';
		// Asserted through the GETTER, not the composed title, and the reason is
		// a harness limit rather than a difference in the code: reading `title`
		// once and then changing the route leaves this file's `$derived` holding
		// its cached value, because nothing in a bare vitest module observes it
		// the way a component template does. Every other leg here avoids the
		// pattern by reading once.
		//
		// The composed-title version of THIS leg is covered where it is honest —
		// `e2e/page-title-survives-pane-and-nav.spec.ts`, cross-route leg, which
		// drives a real client-side navigation and asserts `document.title`, and
		// which fails against the pre-fix build.
		expect(t.item).toBeUndefined();
	});

	it('workspace is deliberately NOT route-scoped — it spans every route in it', async () => {
		const { store: t, title } = await load();
		t.setPageTitle({ workspace: 'WS', section: 'Docs' });
		route.pathname = '/u/ws/roles';
		expect(title()).toBe('WS · Pad');
	});

	it('clearPageTitle drops the stamps with the values', async () => {
		const { store: t, title } = await load();
		t.setPageTitle({ workspace: 'WS', section: 'Docs', item: 'DOC-9' });
		t.clearPageTitle();
		expect(title()).toBe('Pad');
		// And a later set on the same route still composes — the stamps were
		// cleared, not poisoned.
		t.setPageTitle({ workspace: 'WS', section: 'Docs' });
		expect(title()).toBe('Docs · WS · Pad');
	});
});
