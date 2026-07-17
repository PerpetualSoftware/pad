import { describe, it, expect } from 'vitest';
import {
	KNOWN_COLLECTION_URL_PARAMS,
	PANE_ITEM_PARAM,
	buildCollectionUrlParams,
	preservePaneItemParam,
	repairDeadItemLastRoute,
	type CollectionUrlFilterState,
	type DeadItemRoute,
} from './paneUrlParams';

// Baseline "nothing changed" state — each `buildCollectionUrlParams` spec
// below overrides only the field(s) it's exercising.
function state(overrides: Partial<CollectionUrlFilterState> = {}): CollectionUrlFilterState {
	return {
		viewMode: 'list',
		activeFilters: {},
		selectedTags: [],
		unparentedApplied: false,
		searchQuery: '',
		...overrides,
	};
}

describe('preservePaneItemParam', () => {
	it('re-emits the currently-open pane ref onto a freshly rebuilt params object', () => {
		const currentUrl = new URL('https://pad.test/alice/ws/tasks?view=board&item=TASK-5');
		const params = new URLSearchParams();
		params.set('view', 'board');
		preservePaneItemParam(params, currentUrl);
		expect(params.get('item')).toBe('TASK-5');
	});

	it('is a no-op when no pane is open — nothing to preserve', () => {
		const currentUrl = new URL('https://pad.test/alice/ws/tasks?view=board');
		const params = new URLSearchParams();
		preservePaneItemParam(params, currentUrl);
		expect(params.has('item')).toBe(false);
	});

	// This is the actual regression `updateUrlFilters` guards against: it
	// calls `new URLSearchParams()` from scratch on every filter/sort/view/
	// tag/search change, with zero knowledge of the open pane. Without this
	// re-emit, the very next unrelated filter change would silently close
	// the pane by dropping `?item=` from the rebuilt query.
	it('survives a simulated filter-change rebuild that starts from a blank params object', () => {
		const currentUrl = new URL('https://pad.test/alice/ws/tasks?item=BUG-12&status=open');
		const rebuilt = new URLSearchParams();
		rebuilt.set('status', 'closed'); // simulates the new filter/sort/view state
		preservePaneItemParam(rebuilt, currentUrl);
		expect(rebuilt.get('item')).toBe('BUG-12');
		expect(rebuilt.get('status')).toBe('closed');
	});

	it('re-targets to whatever ref is currently open, not a stale one', () => {
		const currentUrl = new URL('https://pad.test/alice/ws/tasks?item=TASK-9');
		const params = new URLSearchParams();
		params.set('item', 'TASK-1'); // some stale/pre-existing value on the rebuilt params
		preservePaneItemParam(params, currentUrl);
		expect(params.get('item')).toBe('TASK-9');
	});
});

// `buildCollectionUrlParams` is the exact function `+page.svelte`'s
// `updateUrlFilters` calls in production (no inline duplicate logic left in
// the route) — so these specs exercise the real `?item=` round-trip, not a
// parallel reimplementation. Removing the `preservePaneItemParam` call
// inside `buildCollectionUrlParams`, or reverting `updateUrlFilters` to
// build params without delegating to it, breaks these specs.
describe('buildCollectionUrlParams', () => {
	it('preserves an existing ?item= across a filter change — the updateUrlFilters call path', () => {
		const currentUrl = new URL('https://pad.test/alice/ws/tasks?item=BUG-12&status=open');
		const params = buildCollectionUrlParams(state({ activeFilters: { status: 'closed' } }), currentUrl);
		expect(params.get('item')).toBe('BUG-12');
		expect(params.get('status')).toBe('closed');
	});

	it('preserves an existing ?item= across a view-mode change', () => {
		const currentUrl = new URL('https://pad.test/alice/ws/tasks?item=TASK-5');
		const params = buildCollectionUrlParams(state({ viewMode: 'board' }), currentUrl);
		expect(params.get('view')).toBe('board');
		expect(params.get('item')).toBe('TASK-5');
	});

	it('preserves an existing ?item= across a tag/search change', () => {
		const currentUrl = new URL('https://pad.test/alice/ws/tasks?item=DOC-2');
		const params = buildCollectionUrlParams(
			state({ selectedTags: ['a', 'b'], searchQuery: 'hello' }),
			currentUrl
		);
		expect(params.get('tags')).toBe('a,b');
		expect(params.get('q')).toBe('hello');
		expect(params.get('item')).toBe('DOC-2');
	});

	it('omits ?item= entirely when no pane is open', () => {
		const currentUrl = new URL('https://pad.test/alice/ws/tasks');
		const params = buildCollectionUrlParams(state({ activeFilters: { status: 'open' } }), currentUrl);
		expect(params.has('item')).toBe(false);
	});
});

// TASK-2123: after a paned item is hard-deleted, its dead `?item=` ref must
// be scrubbed from `pad-last-route-{ws}` so the workspace switcher doesn't
// re-restore a broken split on re-entry. The pre-fix code only ever built a
// full-page `failedPath` (`/{user}/{ws}/{coll}/<ref>`), which never matches
// the embedded pane shape (`/{user}/{ws}/{coll}?item=<ref>`), so the dead
// pane persisted. These specs pin both shapes plus the navigate-away guard.
describe('repairDeadItemLastRoute', () => {
	function dead(overrides: Partial<DeadItemRoute> = {}): DeadItemRoute {
		return {
			username: 'alice',
			wsSlug: 'ws',
			collSlug: 'tasks',
			itemSlug: 'TASK-5',
			embedded: true,
			...overrides,
		};
	}

	describe('embedded pane', () => {
		it('drops the whole entry when the dead pane ref was its only URL state', () => {
			// The core TASK-2123 scenario: `/alice/ws/tasks?item=TASK-5` with a
			// now-dead TASK-5. The old full-page compare missed this shape.
			expect(repairDeadItemLastRoute('/alice/ws/tasks?item=TASK-5', dead())).toBeNull();
		});

		it('strips only ?item= and keeps the collection view/sort/filter state', () => {
			const cached = '/alice/ws/tasks?view=board&status=open&item=TASK-5';
			const repaired = repairDeadItemLastRoute(cached, dead());
			// Cleaned route restores the board view minus the dead pane.
			expect(repaired).toBe('/alice/ws/tasks?view=board&status=open');
			expect(repaired).not.toContain('item=');
		});

		it('leaves the entry untouched when it now points at a DIFFERENT open ref (navigate-away)', () => {
			// The user opened TASK-9 while TASK-5's load was still failing; the
			// +layout effect already persisted the newer pane. Must not clobber.
			expect(
				repairDeadItemLastRoute('/alice/ws/tasks?item=TASK-9', dead({ itemSlug: 'TASK-5' })),
			).toBeUndefined();
		});

		it('leaves the entry untouched when it now points at a different collection page', () => {
			expect(
				repairDeadItemLastRoute('/alice/ws/ideas?item=TASK-5', dead()),
			).toBeUndefined();
		});

		it('handles a slug-form ?item= value the same as a ref', () => {
			// itemUrlId falls back to the slug when an item has no ref, so the
			// pane param — and thus `itemSlug` — can be a slug.
			expect(
				repairDeadItemLastRoute('/alice/ws/tasks?item=my-dead-task', dead({ itemSlug: 'my-dead-task' })),
			).toBeNull();
		});

		it('is a no-op on a missing cache entry', () => {
			expect(repairDeadItemLastRoute(null, dead())).toBeUndefined();
		});
	});

	describe('full page', () => {
		it('removes the entry when the whole URL is the dead item path', () => {
			expect(
				repairDeadItemLastRoute('/alice/ws/tasks/TASK-5', dead({ embedded: false })),
			).toBeNull();
		});

		it('removes the entry even when the dead item path carries a query/hash', () => {
			expect(
				repairDeadItemLastRoute('/alice/ws/tasks/TASK-5?foo=1#frag', dead({ embedded: false })),
			).toBeNull();
		});

		it('leaves the entry untouched when it points elsewhere (navigate-away)', () => {
			expect(
				repairDeadItemLastRoute('/alice/ws/tasks/TASK-99', dead({ embedded: false })),
			).toBeUndefined();
		});

		// A full-page dead item never matches the pane shape and vice-versa —
		// the branch is chosen by `embedded`, not by sniffing the stored URL.
		it('does not treat a full-page dead path as an embedded pane match', () => {
			expect(
				repairDeadItemLastRoute('/alice/ws/tasks/TASK-5', dead({ embedded: true })),
			).toBeUndefined();
		});
	});
});

describe('KNOWN_COLLECTION_URL_PARAMS', () => {
	it('whitelists the pane `item` param', () => {
		expect(KNOWN_COLLECTION_URL_PARAMS).toContain(PANE_ITEM_PARAM);
		expect(KNOWN_COLLECTION_URL_PARAMS).toContain('item');
	});

	// Mirrors `loadUrlFilters`'s else-branch exactly: any param NOT in this
	// whitelist is absorbed into `activeFilters` as a schema-field filter.
	// `item` must be known, or a `?item=TASK-5` URL would produce a
	// phantom `activeFilters.item = 'TASK-5'` field filter instead of
	// opening the split pane.
	it('treats `item` as known (not a phantom field filter) while leaving ordinary schema fields unknown', () => {
		const knownParams = new Set(KNOWN_COLLECTION_URL_PARAMS);
		expect(knownParams.has('item')).toBe(true);
		expect(knownParams.has('status')).toBe(false);
		expect(knownParams.has('priority')).toBe(false);
	});
});
