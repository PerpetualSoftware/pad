import { describe, it, expect, afterEach, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import TimelineVersionCard from './TimelineVersionCard.svelte';
import type { Version } from '$lib/types';

// BUG-2263 / Codex P1 + P2 — REAL-component coverage for the version-restore
// exception. Unlike the other REST surfaces, version restore REST-writes this
// item's `items.content` directly (colliding with the retained Y.Doc on a peeking
// side), so it stays FROZEN while peeking. ItemDetail wires that as
// `restoreFrozen={peeking}` → ItemTimeline passes `frozen={frozen || restoreFrozen}`
// → TimelineVersionCard's `frozen`. This mounts the ACTUAL card (not a probe) to
// lock the terminal gate: the restore control is present iff NOT frozen.
//
// BUG-2271 — the same real card also owns the flush-before-restore contract:
// confirmRestore must AWAIT `flushBeforeRestore` (which drains the initiating
// client's live collab editor into items.content) BEFORE issuing the restore
// POST, so the server-side undo-point captures in-flight edits — and must NOT let
// a flush failure block the restore. We mock the API client so the restore is a
// recorded no-op and assert call ORDER against a passed flush spy.

// `vi.mock`'s factory is hoisted above imports, so the shared order array must be
// created via `vi.hoisted` to be referenceable from inside the factory.
const { apiCalls } = vi.hoisted(() => ({ apiCalls: [] as string[] }));

vi.mock('$lib/api/client', () => ({
	api: {
		versions: {
			restore: vi.fn(async () => {
				apiCalls.push('restore');
				return { id: 'i1' };
			}),
			// Never reached (the fixture version is non-diff so ensureResolved
			// short-circuits), but present so the module shape is complete.
			get: vi.fn(async () => ({ content: '' })),
		},
	},
}));

function target(): HTMLElement {
	return document.body.appendChild(document.createElement('div'));
}

// Minimal non-diff version so no mount-time API fetch fires (is_diff=false → the
// content-resolve effect returns early) and the card renders its restore area.
const version = {
	id: 'v1',
	item_id: 'i1',
	content: 'hello',
	is_diff: false,
	change_summary: 'edited',
	created_by: 'user',
	source: 'web',
	created_at: '2026-07-20T00:00:00Z',
} as unknown as Version;

describe('TimelineVersionCard restore gate (BUG-2263)', () => {
	let root: HTMLElement | null = null;
	let instance: ReturnType<typeof mount> | null = null;

	function render(frozen: boolean) {
		root = target();
		instance = mount(TimelineVersionCard, {
			target: root,
			props: { version, wsSlug: 'ws', itemSlug: 'ITEM-1', currentContent: 'now', onRestore: () => {}, frozen },
		});
		flushSync();
		// The restore-area lives inside the expanded card body — expand it first.
		(root.querySelector('.card-header') as HTMLButtonElement).click();
		flushSync();
		return root;
	}

	afterEach(() => {
		if (instance) unmount(instance);
		root?.remove();
		instance = null;
		root = null;
	});

	it('shows the restore control when NOT frozen (active side)', () => {
		const r = render(false);
		expect(r.querySelector('.restore-area')).not.toBeNull();
	});

	it('HIDES the restore control when frozen (peeking side — same-item content collision)', () => {
		const r = render(true);
		expect(r.querySelector('.restore-area')).toBeNull();
	});
});

describe('TimelineVersionCard flush-before-restore (BUG-2271)', () => {
	let root: HTMLElement | null = null;
	let instance: ReturnType<typeof mount> | null = null;

	// Mount the card, expand it, and drive Restore → Confirm Restore.
	function confirmRestoreFlow(flushBeforeRestore: () => Promise<void>) {
		root = target();
		instance = mount(TimelineVersionCard, {
			target: root,
			props: {
				version,
				wsSlug: 'ws',
				itemSlug: 'ITEM-1',
				currentContent: 'now',
				onRestore: () => {},
				flushBeforeRestore,
			},
		});
		flushSync();
		(root.querySelector('.card-header') as HTMLButtonElement).click(); // expand
		flushSync();
		(root.querySelector('.btn-restore') as HTMLButtonElement).click(); // startRestore
		flushSync();
		(root.querySelector('.btn-restore-confirm') as HTMLButtonElement).click(); // confirmRestore
		flushSync();
	}

	afterEach(() => {
		if (instance) unmount(instance);
		root?.remove();
		instance = null;
		root = null;
		apiCalls.length = 0;
	});

	it('awaits flushBeforeRestore BEFORE issuing the restore POST', async () => {
		const flushBeforeRestore = vi.fn(async () => {
			// Resolve on a real microtask: if confirmRestore did NOT await the
			// flush, the restore POST would win this race and record 'restore'
			// first — so the ['flush','restore'] ordering only holds when the
			// flush is genuinely awaited before the restore.
			await Promise.resolve();
			apiCalls.push('flush');
		});
		confirmRestoreFlow(flushBeforeRestore);
		await vi.waitFor(() => {
			expect(apiCalls).toEqual(['flush', 'restore']);
		});
		expect(flushBeforeRestore).toHaveBeenCalledTimes(1);
	});

	it('still restores when flushBeforeRestore rejects (best-effort flush)', async () => {
		const flushBeforeRestore = vi.fn(async () => {
			throw new Error('flush failed');
		});
		confirmRestoreFlow(flushBeforeRestore);
		// A flush failure must not block a user-confirmed restore.
		await vi.waitFor(() => {
			expect(apiCalls).toContain('restore');
		});
	});
});
