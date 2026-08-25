import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import type { Comment, TimelineEntry, TimelineResponse } from '$lib/types';

/**
 * BUG-2773 — an entry missing from a refreshed first page is not necessarily
 * deleted.
 *
 * The SSE-driven refresh re-fetches the FIRST page, which is the newest N
 * entries, and treated anything previously on that page and now absent as
 * deleted. Once enough newer entries exist, a perfectly alive entry ROLLS OFF
 * that window — and disappeared from the reader's view, from the middle of a
 * timeline whose neighbours on both sides were still shown if they had pressed
 * Load More.
 *
 * Deletion is now inferred only for a position the fresh page still COVERS.
 * Every leg below pairs a roll-off with a real deletion, because a fix that
 * simply stopped removing anything passes the roll-off half on its own.
 */

type ListParams = { limit?: number; before?: string; before_id?: string } | undefined;

const pages: TimelineResponse[] = [];
const timelineListMock = vi.fn(async (_ws: string, _slug: string, _params: ListParams) => {
	return pages.shift() ?? { entries: [], has_more: false };
});

vi.mock('$lib/api/client', () => ({
	api: {
		timeline: {
			list: (ws: string, slug: string, params: ListParams) => timelineListMock(ws, slug, params),
		},
		comments: {
			create: vi.fn(),
			update: vi.fn(),
			delete: vi.fn(),
			addReaction: vi.fn(),
			removeReaction: vi.fn(),
		},
		attachments: {
			downloadUrl: (ws: string, id: string) => `/api/v1/workspaces/${ws}/attachments/${id}`,
		},
	},
}));

// FANS OUT rather than only recording: a mock that returns a disposer leaves
// the component subscribed to nothing and every refresh assertion passes
// vacuously (BUG-2509's lesson).
let itemEventCb: ((event: { type: string }) => void) | undefined;
vi.mock('$lib/services/sse.svelte', () => ({
	sseService: {
		onItemEvent: (cb: (event: { type: string }) => void) => {
			itemEventCb = cb;
			return () => {
				itemEventCb = undefined;
			};
		},
	},
}));

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: { userId: 'user-1', user: { id: 'user-1', role: 'member' } },
}));

vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { canEditItem: () => false },
}));

vi.mock('$lib/components/CommentEditor.svelte', async () => ({
	default: (await import('./fixtures/InertCommentEditor.svelte')).default,
}));

const { default: ItemTimeline } = await import('./ItemTimeline.svelte');

function entry(id: string, createdAt: string, body: string): TimelineEntry {
	const c: Comment = {
		id,
		item_id: 'item-a',
		workspace_id: 'ws-1',
		author: 'alice',
		body,
		created_by: 'alice',
		source: 'web',
		created_at: createdAt,
		updated_at: createdAt,
	};
	return { id, kind: 'comment', created_at: createdAt, actor: 'alice', source: 'web', comment: c };
}

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

const props = $state({
	wsSlug: 'ws',
	username: 'alice',
	itemSlug: 'TASK-1',
	currentContent: '',
	itemId: 'item-a',
	collectionId: 'coll-1',
	hostToken: 'host-1',
	resourceGen: 0,
	visibleKinds: undefined as Array<'comment' | 'activity' | 'version'> | undefined,
	mutationsEnabled: false,
});

async function settle() {
	for (let i = 0; i < 10; i++) {
		await tick();
		flushSync();
	}
}

/**
 * Fire a relevant SSE event and let the debounced refresh run. The component
 * debounces these by 500ms, so the wait has to clear it — and the helper
 * ASSERTS the refresh actually happened, because every expectation below is
 * about what the refresh did and would pass vacuously if it never ran.
 */
async function fireRefresh() {
	const before = timelineListMock.mock.calls.length;
	itemEventCb?.({ type: 'comment_created' });
	await new Promise((r) => setTimeout(r, 700));
	await settle();
	if (timelineListMock.mock.calls.length === before) {
		throw new Error('the SSE refresh never fired — every assertion after this would be vacuous');
	}
}

function shownBodies(): string[] {
	return Array.from(host.querySelectorAll('.comment-card')).map((el) =>
		(el.textContent ?? '').trim()
	);
}

function shows(label: string): boolean {
	return shownBodies().some((b) => b.includes(label));
}

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	pages.length = 0;
	timelineListMock.mockClear();
});

afterEach(() => {
	if (app) unmount(app);
	app = null;
	host.remove();
});

describe('SSE refresh: roll-off is not deletion (BUG-2773)', () => {
	it('keeps an entry that rolled off the first page and drops one actually deleted', async () => {
		// Page one holds two entries.
		pages.push({
			entries: [
				entry('e-new', '2026-01-01T12:00:09Z', 'newer one'),
				entry('e-rolled', '2026-01-01T12:00:01Z', 'rolled off'),
			],
			has_more: false,
		});
		// The refresh: newer traffic pushed `e-rolled` out of the window, and
		// `e-new` was genuinely deleted while still inside it.
		pages.push({
			entries: [
				entry('e-fresh-a', '2026-01-01T12:00:20Z', 'fresh a'),
				entry('e-fresh-b', '2026-01-01T12:00:05Z', 'fresh b'),
			],
			has_more: true,
		});

		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();
		expect(shows('rolled off')).toBe(true);

		await fireRefresh();

		expect(shows('rolled off'), 'an entry older than the fresh window is out of view, not deleted').toBe(
			true
		);
		expect(shows('newer one'), 'an entry inside the fresh window and absent from it WAS deleted').toBe(
			false
		);
		expect(shows('fresh a')).toBe(true);
	});

	it('decides the boundary case on the id, the way the cursor orders it', async () => {
		// Both share the fresh floor's instant. `e-z` sorts at-or-newer than
		// the floor (id greater) so it is covered; `e-a` sorts older so it is
		// out of window. Neither is in the refreshed page.
		pages.push({
			entries: [
				entry('e-z', '2026-01-01T12:00:05Z', 'same instant higher id'),
				entry('e-a', '2026-01-01T12:00:05Z', 'same instant lower id'),
			],
			has_more: false,
		});
		pages.push({
			entries: [entry('e-m', '2026-01-01T12:00:05Z', 'the floor')],
			has_more: true,
		});

		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();

		await fireRefresh();

		expect(shows('same instant higher id'), 'covered by the fresh page — absence means deleted').toBe(
			false
		);
		expect(shows('same instant lower id'), 'sorts below the floor — out of window, kept').toBe(true);
	});

	it('deletes nothing when the refreshed page is empty', async () => {
		// Every row in that window dropped as unrenderable. It says nothing
		// about what still exists, so it must not wipe the view.
		pages.push({
			entries: [entry('e-1', '2026-01-01T12:00:09Z', 'still here')],
			has_more: false,
		});
		pages.push({ entries: [], has_more: true });

		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();

		await fireRefresh();

		expect(shows('still here')).toBe(true);
	});

	it('compares instants, not strings, at the coverage boundary', async () => {
		// A GUARD, not a live scenario, and labelled as one after codex round
		// 3 corrected me: every timestamp this endpoint emits is whole-second
		// today — the store writes RFC3339 seconds and the handler truncates
		// the structured kinds' hand-written ones — so nothing currently
		// produces this input. It pins the comparison the ordering MEANS:
		// `12:00:05.500Z` is NEWER than `12:00:05Z` as an instant and SMALLER
		// as a string, so the day a writer stops truncating, a string
		// comparison would judge it out of window and keep an entry that was
		// really deleted.
		pages.push({
			entries: [entry('e-sub', '2026-01-01T12:00:05.500Z', 'sub second')],
			has_more: false,
		});
		pages.push({
			entries: [entry('e-floor', '2026-01-01T12:00:05Z', 'the floor')],
			has_more: true,
		});

		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();
		expect(shows('sub second')).toBe(true);

		await fireRefresh();

		expect(shows('sub second'), 'newer than the floor as an INSTANT, so its absence is a deletion').toBe(
			false
		);
	});

	it('a FINAL page covers the whole history, so an older missing entry is gone', async () => {
		// has_more false means the server returned everything it has. An entry
		// the client holds and that page does not contain cannot have merely
		// rolled off — there is nothing behind it to roll off into. Without
		// this, the coverage rule would keep a genuinely deleted entry
		// indefinitely (codex round 1).
		pages.push({
			entries: [
				entry('e-keep', '2026-01-01T12:00:09Z', 'still there'),
				entry('e-gone', '2026-01-01T12:00:01Z', 'deleted one'),
			],
			has_more: false,
		});
		pages.push({
			entries: [entry('e-keep', '2026-01-01T12:00:09Z', 'still there')],
			has_more: false,
		});

		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();
		expect(shows('deleted one')).toBe(true);

		await fireRefresh();

		expect(shows('deleted one'), 'below the floor but the page is FINAL — it is gone').toBe(false);
		expect(shows('still there')).toBe(true);
	});

	it('an empty FINAL page clears the view, unlike an empty page with more behind it', async () => {
		// The pair matters: both refreshes are empty, and only the final one
		// means "nothing is left". Asserting either alone would let "empty
		// always clears" or "empty never clears" pass.
		pages.push({
			entries: [entry('e-1', '2026-01-01T12:00:09Z', 'subject')],
			has_more: false,
		});
		pages.push({ entries: [], has_more: false });

		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();
		expect(shows('subject')).toBe(true);

		await fireRefresh();

		expect(shows('subject'), 'an empty FINAL page means the history is empty').toBe(false);
	});

	it('ignores an older refresh that resolves after a newer one', async () => {
		// Two refreshes of the SAME item can overlap — the retry path fires
		// while a newly debounced one is running — and resolve out of order.
		// The older one landing last would undo the newer one's view.
		pages.push({
			entries: [entry('e-1', '2026-01-01T12:00:09Z', 'original')],
			has_more: false,
		});

		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();

		let releaseOld: (r: TimelineResponse) => void = () => {};
		let releaseNew: (r: TimelineResponse) => void = () => {};
		const oldFlight = new Promise<TimelineResponse>((r) => {
			releaseOld = r;
		});
		const newFlight = new Promise<TimelineResponse>((r) => {
			releaseNew = r;
		});
		timelineListMock.mockImplementationOnce(async () => oldFlight);
		timelineListMock.mockImplementationOnce(async () => newFlight);

		// Dispatch both, each past the 500ms debounce so both are in flight.
		itemEventCb?.({ type: 'comment_created' });
		await new Promise((r) => setTimeout(r, 700));
		itemEventCb?.({ type: 'comment_created' });
		await new Promise((r) => setTimeout(r, 700));
		expect(timelineListMock.mock.calls.length, 'both refreshes dispatched').toBeGreaterThanOrEqual(3);

		// The NEWER one lands first and adds a comment...
		releaseNew({
			entries: [
				entry('e-2', '2026-01-01T12:00:20Z', 'posted by someone else'),
				entry('e-1', '2026-01-01T12:00:09Z', 'original'),
			],
			has_more: false,
		});
		await settle();
		expect(shows('posted by someone else')).toBe(true);

		// ...then the OLDER one resolves with the pre-comment view. Applying
		// it would delete a live entry: its page is FINAL and does not contain
		// e-2, so without the fence the coverage rule removes it.
		releaseOld({
			entries: [entry('e-1', '2026-01-01T12:00:09Z', 'original')],
			has_more: false,
		});
		await settle();

		expect(shows('posted by someone else'), 'a stale refresh must not write').toBe(true);
	});

	it('removes an entry loaded via Load More when the refreshed page is FINAL', async () => {
		// The leg that fails against the OLD rule rather than only against an
		// intermediate version of the new one. The old gate only ever removed
		// entries it had seen on a first page, so an entry the reader paged
		// back to was preserved unconditionally — including after it was
		// deleted. Coverage on a final page removes it, and coverage is also
		// what keeps it while the page is NOT final (the leg above).
		pages.push({
			entries: [entry('e-new', '2026-01-01T12:00:09Z', 'page one')],
			has_more: true,
			next_before: '2026-01-01T12:00:09Z',
			next_before_id: 'e-new',
		});
		pages.push({
			entries: [entry('e-older', '2026-01-01T12:00:01Z', 'paged back to')],
			has_more: false,
		});

		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();
		host.querySelector<HTMLButtonElement>('.load-more-btn')!.click();
		await settle();
		expect(shows('paged back to'), 'the older page loaded').toBe(true);

		// The refresh: a FINAL page that no longer contains it.
		pages.push({
			entries: [entry('e-new', '2026-01-01T12:00:09Z', 'page one')],
			has_more: false,
		});
		await fireRefresh();

		expect(shows('paged back to'), 'a final page contains everything — its absence is a deletion').toBe(
			false
		);
		expect(shows('page one')).toBe(true);
	});

	it('a Load More response in flight cannot resurrect an entry a refresh deleted', async () => {
		// The paging request is older than the refresh and does not know: its
		// page still contains the deleted entry, and appending it verbatim
		// puts it back on screen (codex round 4).
		pages.push({
			entries: [
				entry('e-top', '2026-01-01T12:00:20Z', 'top'),
				entry('e-doomed', '2026-01-01T12:00:10Z', 'about to be deleted'),
			],
			has_more: true,
			next_before: '2026-01-01T12:00:10Z',
			next_before_id: 'e-doomed',
		});

		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();
		expect(shows('about to be deleted')).toBe(true);

		// Load More goes out and is held.
		let releasePage: (r: TimelineResponse) => void = () => {};
		const heldPage = new Promise<TimelineResponse>((r) => {
			releasePage = r;
		});
		timelineListMock.mockImplementationOnce(async () => heldPage);
		host.querySelector<HTMLButtonElement>('.load-more-btn')!.click();
		await settle();

		// While it is in flight, a refresh deletes the entry. Its page is
		// FINAL, so coverage removes it.
		pages.push({
			entries: [entry('e-top', '2026-01-01T12:00:20Z', 'top')],
			has_more: false,
		});
		await fireRefresh();
		expect(shows('about to be deleted'), 'the refresh removed it').toBe(false);

		// The paging response lands, still carrying the deleted row.
		releasePage({
			entries: [
				entry('e-doomed', '2026-01-01T12:00:10Z', 'about to be deleted'),
				entry('e-older', '2026-01-01T12:00:01Z', 'genuinely older'),
			],
			has_more: false,
		});
		await settle();

		// The whole page is discarded, not filtered: it was assembled before
		// the deletion and nothing in it can be trusted to reflect the current
		// view. The cost is one wasted click, and the assertion below is what
		// makes that a deliberate choice rather than a silent drop — the
		// cursor is untouched, so Load More is still offered.
		expect(shows('about to be deleted'), 'a stale page must not resurrect it').toBe(false);
		expect(shows('genuinely older'), 'the stale page is discarded whole').toBe(false);
		expect(
			host.querySelector('.load-more-btn'),
			'the cursor is untouched, so the reader can simply click again'
		).not.toBeNull();
	});
});
