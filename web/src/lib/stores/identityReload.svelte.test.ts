import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

/**
 * BUG-3005 — what a real identity change does to the tab.
 *
 * The fix stopped being per-surface after three review rounds each found
 * another layer, so the mechanism is now: clear the state a reload does NOT
 * drop, then reload. These tests pin both halves, and the storage keys, because
 * a clear that stops matching its key is silent.
 */

vi.mock('$app/environment', () => ({ browser: true }));
vi.mock('$lib/components/editor/attachment-metadata', () => ({
	clearAttachmentMetadataCache: vi.fn(),
}));

import {
	clearPersistentIdentityState,
	reloadForIdentityChange,
	RECENT_SEARCHES_KEY,
	LAST_ROUTE_PREFIX,
	LAST_SCROLL_PREFIX,
} from './identityReload.svelte';
import { CURSOR_STORAGE_PREFIX } from '$lib/collab/wsProvider.svelte';
import { clearAttachmentMetadataCache } from '$lib/components/editor/attachment-metadata';

describe('clearPersistentIdentityState', () => {
	beforeEach(() => {
		localStorage.clear();
		sessionStorage.clear();
		vi.mocked(clearAttachmentMetadataCache).mockClear();
	});

	it('drops the palette\'s recent searches', async () => {
		// Search terms carry private item names and project context, and the key
		// has no user in it.
		localStorage.setItem(RECENT_SEARCHES_KEY, JSON.stringify(['alphas secret project']));
		// PRECONDITION: it is really there.
		expect(localStorage.getItem(RECENT_SEARCHES_KEY)).not.toBeNull();

		clearPersistentIdentityState();

		expect(localStorage.getItem(RECENT_SEARCHES_KEY)).toBeNull();
	});

	it('drops every collab cursor, and leaves unrelated session keys alone', async () => {
		// Cursors are keyed by item id ALONE, so B opening an item A edited in
		// this tab would inherit A's op-log position. The negative half matters
		// as much: a clear that took the whole of sessionStorage would be a
		// different, undocumented change.
		sessionStorage.setItem(`${CURSOR_STORAGE_PREFIX}item-a`, '42');
		sessionStorage.setItem(`${CURSOR_STORAGE_PREFIX}item-b`, '7');
		sessionStorage.setItem('pad-unrelated', 'keep me');

		clearPersistentIdentityState();

		expect(sessionStorage.getItem(`${CURSOR_STORAGE_PREFIX}item-a`)).toBeNull();
		expect(sessionStorage.getItem(`${CURSOR_STORAGE_PREFIX}item-b`)).toBeNull();
		expect(sessionStorage.getItem('pad-unrelated')).toBe('keep me');
	});

	it('drops route and scroll memory, which carry private item paths', async () => {
		// `pad-last-scroll-...` embeds the pathname in its own KEY, so a private
		// item slug is legible from the key list without reading any value. Both
		// are keyed by workspace with no user in them.
		localStorage.setItem(`${LAST_ROUTE_PREFIX}ws`, '/alice/ws/ideas/alphas-secret');
		localStorage.setItem(`${LAST_SCROLL_PREFIX}ws-/alice/ws/ideas/alphas-secret`, '420');
		localStorage.setItem('pad-theme', 'dark');

		clearPersistentIdentityState();

		expect(localStorage.getItem(`${LAST_ROUTE_PREFIX}ws`)).toBeNull();
		expect(localStorage.getItem(`${LAST_SCROLL_PREFIX}ws-/alice/ws/ideas/alphas-secret`)).toBeNull();
		// And leaves an ordinary UI preference alone — the clear is scoped, not
		// a localStorage wipe.
		expect(localStorage.getItem('pad-theme')).toBe('dark');
	});

	it('LAST_ROUTE_PREFIX still matches the key workspace-route.ts builds', async () => {
		const src = readFileSync(
			resolve(__dirname, '../utils/workspace-route.ts'),
			'utf8',
		);
		expect(src).toContain(LAST_ROUTE_PREFIX);
	});

	it('drops the attachment-metadata memo', async () => {
		clearPersistentIdentityState();
		expect(clearAttachmentMetadataCache).toHaveBeenCalledTimes(1);
	});

	it('RECENT_SEARCHES_KEY still matches the literal in CommandPalette.svelte', async () => {
		// The palette is a .svelte file and cannot export the constant, so the
		// clear duplicates it. A rename on either side would leave a clear that
		// silently matches nothing — the failure this whole mechanism exists to
		// avoid, reintroduced one string at a time.
		const src = readFileSync(
			resolve(__dirname, '../components/search/CommandPalette.svelte'),
			'utf8',
		);
		const match = src.match(/RECENT_SEARCHES_KEY\s*=\s*'([^']+)'/);
		expect(match, 'CommandPalette.svelte no longer declares RECENT_SEARCHES_KEY').not.toBeNull();
		expect(match![1]).toBe(RECENT_SEARCHES_KEY);
	});
});

describe('reloadForIdentityChange', () => {
	const original = window.location;

	afterEach(() => {
		Object.defineProperty(window, 'location', { value: original, configurable: true });
	});

	it('clears first, then reloads', async () => {
		const order: string[] = [];
		vi.mocked(clearAttachmentMetadataCache).mockImplementation(() => { order.push('clear'); });
		const reload = vi.fn(() => { order.push('reload'); });
		Object.defineProperty(window, 'location', {
			value: { ...original, reload },
			configurable: true,
		});

		reloadForIdentityChange();

		// ORDER, not just both: reloading first would tear the page down before
		// the clears ran, leaving the palette history and collab cursors for the
		// next user — the exact state the reload cannot drop by itself.
		expect(order).toEqual(['clear', 'reload']);
	});
});
