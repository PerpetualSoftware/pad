import { describe, it, expect } from 'vitest';
import { KNOWN_COLLECTION_URL_PARAMS, PANE_ITEM_PARAM, preservePaneItemParam } from './paneUrlParams';

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
