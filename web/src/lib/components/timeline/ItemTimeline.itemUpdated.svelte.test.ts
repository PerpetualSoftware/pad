import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import type { TimelineResponse } from '$lib/types';

/**
 * BUG-3160 — the timeline re-reads its head on `item_updated` for ITS item,
 * through a throttle (at most one re-read per 10s, trailing, none while hidden;
 * lead-ruled). `item_updated` was excluded before because every content save
 * emits it, and refreshing on each one caused jitter and rate-limit errors, so
 * these legs pin the throttle and the item filter as hard as the refresh itself.
 * The end-to-end half (a merged activity row appearing without a reload) is
 * web/e2e/bug-3160-timeline-merged-row.spec.ts.
 */

const timelineListMock = vi.fn<(ws: string, slug: string) => Promise<TimelineResponse>>();

vi.mock('$lib/api/client', () => ({
	api: {
		timeline: { list: (ws: string, slug: string) => timelineListMock(ws, slug) },
		comments: { create: vi.fn(), update: vi.fn(), delete: vi.fn(), addReaction: vi.fn(), removeReaction: vi.fn() },
	},
}));

const sseBox = vi.hoisted(() => ({ handler: null as null | ((e: { type: string; item_id?: string }) => void) }));
vi.mock('$lib/services/sse.svelte', () => ({
	sseService: {
		onItemEvent: (fn: (e: { type: string; item_id?: string }) => void) => {
			sseBox.handler = fn;
			return () => {
				if (sseBox.handler === fn) sseBox.handler = null;
			};
		},
	},
}));

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: { userId: 'user-1', user: { id: 'user-1', role: 'member' }, identityFence: () => () => true },
}));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { canEditItem: () => false } }));
vi.mock('$lib/components/CommentEditor.svelte', async () => ({
	default: (await import('./fixtures/InertCommentEditor.svelte')).default,
}));

const { default: ItemTimeline } = await import('./ItemTimeline.svelte');

let host: HTMLElement;
let app: Record<string, unknown> | null = null;
let visibility: DocumentVisibilityState = 'visible';

async function settle() {
	for (let i = 0; i < 10; i++) {
		await tick();
		flushSync();
	}
}

// `null` means "no itemId" — an explicit `undefined` argument would take the
// default instead, which is how this helper first mounted WITH an id in the
// no-id leg.
async function mountTimeline(itemId: string | null = 'item-a') {
	app = mount(ItemTimeline, {
		target: host,
		props: { wsSlug: 'ws', username: 'alice', itemSlug: 'TASK-1', currentContent: '', itemId: itemId ?? undefined, collectionId: 'coll-1' },
	}) as Record<string, unknown>;
	await settle();
	timelineListMock.mockClear(); // count only SSE-driven re-reads from here
}

async function advance(ms: number) {
	await vi.advanceTimersByTimeAsync(ms);
	await settle();
}

function fire(type: string, item_id?: string) {
	expect(sseBox.handler, 'the timeline is not subscribed').not.toBeNull();
	sseBox.handler!({ type, item_id });
}

beforeEach(() => {
	vi.useFakeTimers();
	host = document.createElement('div');
	document.body.appendChild(host);
	visibility = 'visible';
	Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibility });
	timelineListMock.mockReset();
	timelineListMock.mockResolvedValue({ entries: [], has_more: false });
});

afterEach(() => {
	if (app) unmount(app);
	app = null;
	host.remove();
	vi.useRealTimers();
});

describe('ItemTimeline: item_updated re-reads the head, throttled (BUG-3160)', () => {
	it('an item_updated for THIS item re-reads the head', async () => {
		await mountTimeline();
		fire('item_updated', 'item-a');
		await advance(600);
		expect(timelineListMock).toHaveBeenCalledTimes(1);
	});

	it('an item_updated for ANOTHER item re-reads nothing — onItemEvent is workspace-wide', async () => {
		await mountTimeline();
		fire('item_updated', 'item-b');
		await advance(15_000);
		expect(timelineListMock).not.toHaveBeenCalled();
	});

	it('with no itemId there is nothing to filter on, so item_updated re-reads nothing', async () => {
		await mountTimeline(null);
		fire('item_updated', 'item-a');
		await advance(15_000);
		expect(timelineListMock).not.toHaveBeenCalled();
	});

	it('an edit burst (a flush every 5s for a minute) costs at most one re-read per 10s', async () => {
		await mountTimeline();
		for (let s = 0; s < 60; s += 5) {
			fire('item_updated', 'item-a');
			await advance(5_000);
		}
		await advance(10_000);
		const n = timelineListMock.mock.calls.length;
		expect(n, `re-reads for 12 events over 60s: ${n}`).toBeLessThanOrEqual(7);
		expect(n).toBeGreaterThanOrEqual(6);
	});

	it('nothing while the tab is hidden; one catch-up when it becomes visible', async () => {
		await mountTimeline();
		visibility = 'hidden';
		fire('item_updated', 'item-a');
		fire('item_updated', 'item-a');
		await advance(30_000);
		expect(timelineListMock).not.toHaveBeenCalled();
		visibility = 'visible';
		document.dispatchEvent(new Event('visibilitychange'));
		await advance(600);
		expect(timelineListMock).toHaveBeenCalledTimes(1);
	});

	it('comments keep their own 500ms path, unthrottled by an item_updated burst', async () => {
		await mountTimeline();
		fire('item_updated', 'item-a');
		await advance(600); // the throttled re-read runs, opening its 10s window
		timelineListMock.mockClear();
		fire('comment_created', 'item-a');
		await advance(600);
		expect(timelineListMock, 'a comment must not wait out the item_updated window').toHaveBeenCalledTimes(1);
	});

	it('unmounting clears a pending re-read', async () => {
		await mountTimeline();
		fire('item_updated', 'item-a');
		unmount(app!);
		app = null;
		await advance(15_000);
		expect(timelineListMock).not.toHaveBeenCalled();
	});
});
