import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import { page } from '$app/state';
import type { Activity } from '$lib/types';

/**
 * BUG-3160, the workspace activity page: an item event re-reads the feed's
 * head, so a row the debounce merge restamped shows at the top without a
 * reload. The page listens WORKSPACE-wide, so the throttle is the point of
 * these legs as much as the refresh is (lead ruling: at most 6 re-reads a
 * minute per tab, however busy the workspace).
 */
const listActivity = vi.hoisted(() =>
	vi.fn<(slug: string, params: Record<string, unknown>) => Promise<Activity[]>>()
);

vi.mock('$lib/api/client', () => ({
	api: {
		collections: { list: vi.fn().mockResolvedValue([]) },
		activity: { list: (slug: string, params: Record<string, unknown>) => listActivity(slug, params) },
		comments: { list: vi.fn().mockResolvedValue([]) }
	},
	PadApiError: class extends Error {}
}));

const sseBox = vi.hoisted(() => ({ handler: null as null | ((e: { type: string; item_id?: string }) => void) }));
vi.mock('$lib/services/sse.svelte', () => ({
	sseService: {
		onItemEvent: (fn: (e: { type: string; item_id?: string }) => void) => {
			sseBox.handler = fn;
			return () => {
				if (sseBox.handler === fn) sseBox.handler = null;
			};
		}
	}
}));

// BUG-3181: the page brings the workspace's local index up on entry. That is
// the shared helper's job (and its own test's), not this file's — and the
// real one would drive the local index against an API mock with no index
// endpoints, which never settles. Stubbed here; the one leg below checks
// the page asks for it.
const enterWorkspaceIndex = vi.hoisted(() => vi.fn(async () => true));
vi.mock('$lib/stores/workspaceIndexEntry', () => ({ enterWorkspaceIndex }));

const { default: ActivityPage } = await import('./[username]/[workspace]/activity/+page.svelte');

function act(id: string, secondsAgo: number, action = 'settings_changed'): Activity {
	return {
		id,
		workspace_id: 'ws',
		action,
		actor: 'user',
		source: 'web',
		metadata: '{}',
		created_at: new Date(Date.UTC(2026, 7, 24, 12, 0, 0) - secondsAgo * 1000).toISOString()
	} as Activity;
}

let host: HTMLElement;
let app: Record<string, unknown> | null = null;
let visibility: DocumentVisibilityState = 'visible';

async function settle(): Promise<void> {
	flushSync();
	for (let i = 0; i < 8; i++) {
		await Promise.resolve();
		await tick();
	}
	flushSync();
}

async function advance(ms: number) {
	await vi.advanceTimersByTimeAsync(ms);
	await settle();
}

beforeEach(() => {
	vi.useFakeTimers();
	host = document.createElement('div');
	document.body.appendChild(host);
	page.params.workspace = 'ws';
	page.params.username = 'alice';
	localStorage.clear();
	localStorage.setItem('pad-activity-view', 'audit');
	visibility = 'visible';
	Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibility });
	listActivity.mockReset();
});

afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	host.remove();
	vi.useRealTimers();
});

async function mountPage(first: Activity[]) {
	listActivity.mockResolvedValue(first);
	app = mount(ActivityPage, { target: host, props: {} }) as Record<string, unknown>;
	await settle();
	expect(listActivity, 'precondition: the initial load ran').toHaveBeenCalledTimes(1);
	listActivity.mockClear();
}

describe('workspace activity head re-read (BUG-3160)', () => {
	it('brings the workspace local index up on entry (BUG-3181)', async () => {
		enterWorkspaceIndex.mockClear();
		await mountPage([act('a1', 5)]);
		expect(enterWorkspaceIndex).toHaveBeenCalledTimes(1);
		expect(enterWorkspaceIndex.mock.calls[0][0]).toBe('ws');
	});

	it('a row the debounce merge restamped moves to the top after an item event', async () => {
		await mountPage([act('b', 10), act('a', 20)]);
		expect([...host.querySelectorAll('[data-activity-id]')].map((e) => e.getAttribute('data-activity-id'))).toEqual(['b', 'a']);
		listActivity.mockResolvedValue([act('a', 0), act('b', 10)]); // a restamped to now
		sseBox.handler!({ type: 'item_updated', item_id: 'x' });
		await advance(600);
		expect(listActivity).toHaveBeenCalledTimes(1);
		expect([...host.querySelectorAll('[data-activity-id]')].map((e) => e.getAttribute('data-activity-id'))).toEqual(['a', 'b']);
	});

	it('a workspace-wide burst costs at most one head re-read per 10s (≤6/min per tab)', async () => {
		await mountPage([act('a', 20)]);
		listActivity.mockResolvedValue([act('a', 20)]);
		// Two busy seats: an item event every 250ms for a minute (240 events).
		for (let ms = 0; ms < 60_000; ms += 250) {
			sseBox.handler!({ type: 'item_updated', item_id: `i${ms % 7}` });
			await advance(250);
		}
		await advance(10_000);
		const n = listActivity.mock.calls.length;
		expect(n, `head re-reads for 240 events over 60s: ${n}`).toBeLessThanOrEqual(7);
		expect(n).toBeGreaterThanOrEqual(6);
	});

	it('nothing while hidden; one catch-up when visible', async () => {
		await mountPage([act('a', 20)]);
		visibility = 'hidden';
		sseBox.handler!({ type: 'item_updated', item_id: 'x' });
		await advance(30_000);
		expect(listActivity).not.toHaveBeenCalled();
		visibility = 'visible';
		document.dispatchEvent(new Event('visibilitychange'));
		await advance(600);
		expect(listActivity).toHaveBeenCalledTimes(1);
	});

	it('the head re-read carries the active filters', async () => {
		await mountPage([act('a', 20)]);
		const select = host.querySelector<HTMLSelectElement>('#filter-action')!;
		listActivity.mockResolvedValue([]);
		select.value = 'created';
		select.dispatchEvent(new Event('change', { bubbles: true }));
		await settle();
		listActivity.mockClear();
		sseBox.handler!({ type: 'item_updated', item_id: 'x' });
		await advance(600);
		expect(listActivity).toHaveBeenCalledTimes(1);
		expect(listActivity.mock.calls[0][1].action).toBe('created');
		expect('before' in listActivity.mock.calls[0][1], 'a head read has no cursor').toBe(false);
	});

	it('a head re-read that lands after a filter reset is dropped, not merged into the new feed', async () => {
		await mountPage([act('a', 20)]);
		let release!: (rows: Activity[]) => void;
		listActivity.mockImplementationOnce(() => new Promise<Activity[]>((r) => (release = r)));
		sseBox.handler!({ type: 'item_updated', item_id: 'x' });
		await advance(600); // the head re-read is now in flight
		listActivity.mockResolvedValue([act('f', 5, 'created')]);
		const select = host.querySelector<HTMLSelectElement>('#filter-action')!;
		select.value = 'created';
		select.dispatchEvent(new Event('change', { bubbles: true }));
		await settle();
		release([act('stale', 1)]); // rows from the OLD filter
		await settle();
		const ids = [...host.querySelectorAll('[data-activity-id]')].map((e) => e.getAttribute('data-activity-id'));
		expect(ids, 'precondition: the reset rendered').toContain('f');
		expect(ids, 'a stale head read leaked old-filter rows into the new feed').not.toContain('stale');
	});

	it('an item event during a load is not lost: the head is re-read once the load finishes (codex round 1)', async () => {
		let release!: (rows: Activity[]) => void;
		listActivity.mockImplementationOnce(() => new Promise<Activity[]>((r) => (release = r)));
		app = mount(ActivityPage, { target: host, props: {} }) as Record<string, unknown>;
		await settle();
		expect(listActivity, 'precondition: the initial load is in flight').toHaveBeenCalledTimes(1);
		// The event's write may have landed AFTER the load's read was sent.
		sseBox.handler!({ type: 'item_updated', item_id: 'x' });
		await advance(600);
		listActivity.mockResolvedValue([act('new', 0), act('a', 20)]);
		release([act('a', 20)]);
		await settle();
		await advance(10_000);
		expect(listActivity.mock.calls.length, 'no head re-read followed the event that arrived mid-load').toBeGreaterThanOrEqual(2);
		const ids = [...host.querySelectorAll('[data-activity-id]')].map((e) => e.getAttribute('data-activity-id'));
		expect(ids).toContain('new');
	});
});
