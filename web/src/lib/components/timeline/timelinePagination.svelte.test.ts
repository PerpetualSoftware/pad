import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import type { Comment, TimelineEntry, TimelineResponse } from '$lib/types';

/**
 * BUG-2765 — paging past a window the server dropped.
 *
 * The server over-fetches per source and drops rows that cannot render, so a
 * page can carry fewer entries than the rows it consumed, or none at all with
 * more history behind them. This component derived its cursor from the last
 * RENDERED entry, which fails in two ways: an empty page yields no cursor, and
 * a later all-dropped page yields the SAME cursor, so the next press re-asks
 * for the window it just got and nothing ever moves.
 *
 * These drive Load More against a mock that RECORDS the cursor it was called
 * with — the assertion is about what the component ASKS FOR, not only what it
 * ends up displaying, because a component that displays the right thing by
 * re-fetching page one forever is the bug.
 */

type ListParams = { limit?: number; before?: string; before_id?: string } | undefined;

const calls: ListParams[] = [];
const pages: TimelineResponse[] = [];

const timelineListMock = vi.fn(async (_ws: string, _slug: string, params: ListParams) => {
	calls.push(params);
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

vi.mock('$lib/services/sse.svelte', () => ({
	sseService: { onItemEvent: () => () => {} },
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

function entry(id: string, createdAt: string, body = 'hello'): TimelineEntry {
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
	for (let i = 0; i < 8; i++) {
		await tick();
		flushSync();
	}
}

function loadMoreButton(): HTMLButtonElement | null {
	return host.querySelector<HTMLButtonElement>('.load-more-btn');
}

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	calls.length = 0;
	pages.length = 0;
	timelineListMock.mockClear();
});

afterEach(() => {
	if (app) unmount(app);
	app = null;
	host.remove();
});

describe('timeline pagination past dropped windows (BUG-2765)', () => {
	it('offers Load More on an empty first page and pages with the server cursor', async () => {
		// Every row in the first window dropped server-side. Without the
		// server's cursor there is nothing to page from at all.
		pages.push({
			entries: [],
			has_more: true,
			next_before: '2026-01-01T00:00:05Z',
			next_before_id: 'raw-read-row',
		});
		pages.push({ entries: [entry('e-old', '2026-01-01T00:00:01Z')], has_more: false });

		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();

		const btn = loadMoreButton();
		expect(btn, 'an empty page with has_more must still offer Load More').not.toBeNull();

		btn!.click();
		await settle();

		expect(calls[1]).toEqual({
			before: '2026-01-01T00:00:05Z',
			before_id: 'raw-read-row',
		});
		expect(host.textContent).toContain('hello');
	});

	it('advances past a page whose rows all dropped instead of re-asking for it', async () => {
		// Page 1 renders. Page 2 comes back empty — the wedge: the last
		// rendered entry has not moved, so the pre-fix component would send
		// the page-1 cursor again, forever.
		pages.push({
			entries: [entry('e-1', '2026-01-01T00:00:09Z')],
			has_more: true,
			next_before: '2026-01-01T00:00:09Z',
			next_before_id: 'e-1',
		});
		pages.push({
			entries: [],
			has_more: true,
			next_before: '2026-01-01T00:00:05Z',
			next_before_id: 'raw-read-row',
		});
		pages.push({ entries: [entry('e-2', '2026-01-01T00:00:01Z')], has_more: false });

		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();

		loadMoreButton()!.click();
		await settle();

		// The cursors asked for, in order, are strictly the ones the server
		// handed back — never a repeat.
		expect(calls[1]).toEqual({ before: '2026-01-01T00:00:09Z', before_id: 'e-1' });
		expect(calls[2]).toEqual({ before: '2026-01-01T00:00:05Z', before_id: 'raw-read-row' });
		expect(calls).toHaveLength(3);
		// And the press produced something: the entry behind the dropped window.
		expect(host.querySelectorAll('.comment-card')).toHaveLength(2);
	});

	it('stops asking after the hop bound rather than fanning out requests', async () => {
		// Every page empty and every page claiming more. One press must not
		// turn into an unbounded request loop.
		for (let i = 0; i < 20; i++) {
			pages.push({
				entries: [],
				has_more: true,
				next_before: `2026-01-01T00:00:${String(20 - i).padStart(2, '0')}Z`,
				next_before_id: `raw-${i}`,
			});
		}

		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();

		loadMoreButton()!.click();
		await settle();

		// 1 initial load + at most the hop bound.
		expect(calls.length).toBeGreaterThan(1);
		expect(calls.length).toBeLessThanOrEqual(6);
		// Still offered, so the user can continue deliberately.
		expect(loadMoreButton()).not.toBeNull();
	});

	it('falls back to the last entry when the server sends no cursor', async () => {
		// A server older than this field. The component must behave exactly as
		// it did before — including being unable to page an empty page, which
		// is why the server half is the fix and this is only compatibility.
		pages.push({ entries: [entry('e-1', '2026-01-01T00:00:09Z')], has_more: true });
		pages.push({ entries: [entry('e-2', '2026-01-01T00:00:01Z')], has_more: false });

		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();

		loadMoreButton()!.click();
		await settle();

		expect(calls[1]).toEqual({ before: '2026-01-01T00:00:09Z', before_id: 'e-1' });
	});

	it('merges a later page into date order instead of appending it', async () => {
		// The overlap the server's cursor deliberately creates: one source's
		// window ran out before another's, so page 2 legitimately carries an
		// entry NEWER than the oldest one page 1 showed. Concatenating prints
		// it below — a comment from 12:00:07 sitting under one from 12:00:01.
		pages.push({
			entries: [
				entry('e-newest', '2026-01-01T12:00:09Z', 'newest'),
				entry('e-oldest-shown', '2026-01-01T12:00:01Z', 'oldest shown'),
			],
			has_more: true,
			next_before: '2026-01-01T12:00:08Z',
			next_before_id: 'raw-tail',
		});
		pages.push({
			entries: [
				// Newer than the last entry already on screen, older than the
				// cursor — the shape only the server can produce.
				entry('e-between', '2026-01-01T12:00:07Z', 'in between'),
				entry('e-older', '2026-01-01T12:00:00Z', 'older'),
			],
			has_more: false,
		});

		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();

		loadMoreButton()!.click();
		await settle();

		const bodies = Array.from(host.querySelectorAll('.comment-card')).map((el) =>
			(el.textContent ?? '').trim()
		);
		const order = ['newest', 'in between', 'oldest shown', 'older'].map((label) =>
			bodies.findIndex((b) => b.includes(label))
		);
		expect(order.every((i) => i >= 0), `all four entries rendered: ${bodies}`).toBe(true);
		expect(order, 'entries must read newest-first after the merge').toEqual(
			[...order].sort((a, b) => a - b)
		);
	});

	it('makes ONE request per click when a cursorless server cannot advance', async () => {
		// The pre-cursor server, on a page it emptied: has_more with no
		// cursor, so the fallback re-derives the same last entry forever. The
		// component cannot fix that server — but it must not amplify it into a
		// request per hop either.
		pages.push({ entries: [entry('e-1', '2026-01-01T00:00:09Z')], has_more: true });
		pages.push({ entries: [], has_more: true });
		pages.push({ entries: [], has_more: true });
		pages.push({ entries: [], has_more: true });

		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();

		loadMoreButton()!.click();
		await settle();

		// Initial load + exactly one Load More request.
		expect(calls).toHaveLength(2);
		expect(calls[1]).toEqual({ before: '2026-01-01T00:00:09Z', before_id: 'e-1' });
	});

	it('does not offer the previous item\'s cursor while a switch is in flight', async () => {
		// The old entries stay on screen while the new item's page 1 is in
		// flight, so the button is still clickable. Its cursor must not be the
		// previous item's position, aimed at the new item.
		pages.push({
			entries: [entry('e-a', '2026-01-01T00:00:09Z')],
			has_more: true,
			next_before: '2026-01-01T00:00:09Z',
			next_before_id: 'e-a',
		});

		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();
		expect(loadMoreButton(), 'page one offered Load More').not.toBeNull();

		// Switch items; hold the new load unresolved.
		let release: (r: TimelineResponse) => void = () => {};
		const pending = new Promise<TimelineResponse>((r) => {
			release = r;
		});
		timelineListMock.mockImplementationOnce(async (_ws, _slug, params) => {
			calls.push(params);
			return pending;
		});
		props.itemSlug = 'TASK-2';
		await settle();

		const callsDuringSwitch = calls.length;
		expect(loadMoreButton(), 'no paging is offered until the new page lands').toBeNull();

		release({ entries: [entry('e-b', '2026-02-01T00:00:09Z')], has_more: false });
		await settle();

		// Nothing was requested with the old item's cursor in between.
		expect(calls).toHaveLength(callsDuringSwitch);
		props.itemSlug = 'TASK-1';
	});

	it('discards a Load More page held across a switch away and BACK', async () => {
		// The case the identity check cannot see: by the time the stale page
		// resolves, itemSlug matches again, so only the view generation
		// distinguishes "this page belongs to what is on screen" from "this
		// page belongs to a view that has since been replaced twice". The same
		// generation is what a local comment delete advances via loadTimeline
		// (codex round 6); this is the harness-reachable form of it.
		pages.push({
			entries: [entry('e-a1', '2026-01-01T00:00:09Z', 'A page one')],
			has_more: true,
			next_before: '2026-01-01T00:00:09Z',
			next_before_id: 'e-a1',
		});

		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();

		let releaseStale: (r: TimelineResponse) => void = () => {};
		const stale = new Promise<TimelineResponse>((r) => {
			releaseStale = r;
		});
		timelineListMock.mockImplementationOnce(async (_ws, _slug, params) => {
			calls.push(params);
			return stale;
		});
		loadMoreButton()!.click();
		await settle();

		// Away and back. Both reloads land normally.
		pages.push({ entries: [entry('e-b1', '2026-02-01T00:00:09Z', 'B page one')], has_more: false });
		props.itemSlug = 'TASK-2';
		await settle();
		pages.push({ entries: [entry('e-a1', '2026-01-01T00:00:09Z', 'A page one')], has_more: false });
		props.itemSlug = 'TASK-1';
		await settle();

		releaseStale({
			entries: [entry('e-a2', '2026-01-01T00:00:01Z', 'A older page')],
			has_more: false,
		});
		await settle();

		expect(
			host.textContent,
			'a page fetched two views ago must not land, even back on the same item'
		).not.toContain('A older page');
		expect(host.textContent, 'the current view is intact').toContain('A page one');
	});
});
