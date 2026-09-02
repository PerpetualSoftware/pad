// The two-view split of the item timeline (IDEA-2843, GitHub #1228).
//
// Comments render under the item content on Details; changes and versions
// render in the pane's tabs. One component cannot be in two DOM locations, so
// `ItemTimeline` stays the single owner — one fetch, one SSE subscription, one
// composer — and MIRRORS its feed out for a second `TimelineEntryList`.
//
// Two properties hold the split up, and both fail SILENTLY if broken:
//
//  1. The mirror carries the WHOLE feed, not the owner's rendered slice. The
//     owner renders comments only, so publishing `visibleEntries` instead of
//     `entries` is a one-word edit that leaves Activity and Versions
//     permanently empty with nothing to report.
//  2. Every entry kind is routed to some view. A kind in none of the three
//     filters renders NOWHERE — which is how `note` / `decision` shipped
//     invisible the first time (BUG-2301).
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import type { TimelineEntry, TimelineResponse } from '$lib/types';
import {
	ALL_TIMELINE_KINDS,
	CHANGE_KINDS,
	COMMENT_KINDS,
	VERSION_KINDS,
	type TimelineFeed
} from './feed';

const timelineListMock = vi.fn<() => Promise<TimelineResponse>>();

vi.mock('$lib/api/client', () => ({
	api: {
		timeline: { list: () => timelineListMock() },
		comments: {
			create: vi.fn(),
			update: vi.fn(),
			delete: vi.fn(),
			addReaction: vi.fn(),
			removeReaction: vi.fn()
		},
		attachments: {
			downloadUrl: (ws: string, id: string) => `/api/v1/workspaces/${ws}/attachments/${id}`
		}
	}
}));

const ItemTimeline = (await import('./ItemTimeline.svelte')).default;
const TimelineEntryList = (await import('./TimelineEntryList.svelte')).default;

function entry(kind: TimelineEntry['kind'], id: string): TimelineEntry {
	const base = { id, kind, created_at: '2026-09-02T10:00:00Z', actor: 'alice' };
	if (kind === 'comment') {
		return {
			...base,
			comment: { id, body: `comment ${id}`, created_at: base.created_at, user_id: 'u1' }
		} as unknown as TimelineEntry;
	}
	if (kind === 'activity') {
		return { ...base, activity: { id, action: 'updated', created_at: base.created_at } } as unknown as TimelineEntry;
	}
	if (kind === 'version') {
		return { ...base, version: { id, version: 2, created_at: base.created_at } } as unknown as TimelineEntry;
	}
	return { ...base, [kind]: { body: `${kind} body` } } as unknown as TimelineEntry;
}

const FEED: TimelineEntry[] = [
	entry('comment', 'c1'),
	entry('activity', 'a1'),
	entry('version', 'v1'),
	entry('note', 'n1'),
	entry('decision', 'd1')
];

let host: HTMLDivElement;
let app: Record<string, unknown> | null = null;

beforeEach(() => {
	document.body.innerHTML = '';
	host = document.body.appendChild(document.createElement('div'));
	timelineListMock.mockReset();
	timelineListMock.mockResolvedValue({ entries: FEED, has_more: false } as unknown as TimelineResponse);
});

afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	vi.restoreAllMocks();
});

async function settle() {
	for (let i = 0; i < 8; i++) {
		await Promise.resolve();
		flushSync();
	}
}

describe('timeline second view — the mirrored feed', () => {
	it('mirrors the WHOLE feed, not the kinds the owner renders', async () => {
		const state = $state<{ feed: TimelineFeed | undefined }>({ feed: undefined });
		app = mount(ItemTimeline, {
			target: host,
			props: {
				wsSlug: 'ws',
				itemSlug: 'TASK-1',
				itemId: 'item-a',
				collectionId: 'coll-1',
				currentContent: '',
				// The owner renders comments only — exactly as ItemDetail mounts it.
				visibleKinds: [...COMMENT_KINDS],
				get feed() {
					return state.feed;
				},
				set feed(v: TimelineFeed | undefined) {
					state.feed = v;
				}
			}
		}) as Record<string, unknown>;
		await settle();

		expect(state.feed).toBeDefined();
		const kinds = state.feed!.entries.map((e) => e.kind).sort();

		// The assertion that matters: kinds the OWNER does not render are still
		// in the mirror. Publishing `visibleEntries` would leave this ['comment'].
		expect(kinds).toEqual(['activity', 'comment', 'decision', 'note', 'version']);

		// And pagination rides along, so a tab that shows older entries can ask
		// for them instead of being a dead end.
		expect(typeof state.feed!.loadMore).toBe('function');
	});
});

describe('timeline second view — the mirrored error', () => {
	it('mirrors a load FAILURE, so the second view can tell empty from broken', async () => {
		timelineListMock.mockReset();
		timelineListMock.mockRejectedValue(new Error('network is down'));

		const state = $state<{ feed: TimelineFeed | undefined }>({ feed: undefined });
		app = mount(ItemTimeline, {
			target: host,
			props: {
				wsSlug: 'ws',
				itemSlug: 'TASK-1',
				itemId: 'item-a',
				collectionId: 'coll-1',
				currentContent: '',
				visibleKinds: [...COMMENT_KINDS],
				get feed() {
					return state.feed;
				},
				set feed(v: TimelineFeed | undefined) {
					state.feed = v;
				}
			}
		}) as Record<string, unknown>;
		await settle();

		// Without this, a failed load reaches the Activity tab as zero entries
		// and renders as "No timeline entries yet." — an unreachable server
		// wearing an empty timeline's clothes (codex round 1).
		expect(state.feed?.error).toBeTruthy();
		expect(state.feed?.entries).toEqual([]);
	});

	it('does not claim the item has no history while reporting an error', async () => {
		// The two states contradict each other: "No timeline entries yet" is a
		// statement about the ITEM, and a failed load knows nothing about the
		// item. Rendering both at once — an unguarded `showEmpty` — puts
		// something false next to something true (codex round 2).
		//
		// Asserted against the MOUNTED owner's DOM rather than by re-evaluating
		// the condition: a test that recomputes `entries.length === 0 &&
		// !loading && !error` passes whatever the component actually renders.
		// The second view's copy of this condition is identical, and this is
		// the one a unit test can reach.
		timelineListMock.mockReset();
		timelineListMock.mockRejectedValue(new Error('network is down'));

		app = mount(ItemTimeline, {
			target: host,
			props: {
				wsSlug: 'ws',
				itemSlug: 'TASK-1',
				itemId: 'item-a',
				collectionId: 'coll-1',
				currentContent: ''
			}
		}) as Record<string, unknown>;
		await settle();

		expect(host.textContent).toContain('network is down');
		expect(host.textContent).not.toContain('No timeline entries yet');
	});
});

describe('timeline second view — kind routing', () => {
	it('renders only the kinds it is handed', () => {
		app = mount(TimelineEntryList, {
			target: host,
			props: { entries: FEED.filter((e) => (CHANGE_KINDS as readonly string[]).includes(e.kind)), wsSlug: 'ws' }
		}) as Record<string, unknown>;
		flushSync();

		// Three change entries, and no comment card — the composer and the
		// comment thread belong to the Details view now.
		expect(host.querySelectorAll('.entry').length).toBe(3);
		expect(host.querySelector('.comment-card')).toBeNull();
		expect(host.textContent).not.toContain('comment c1');
	});

	it('routes every kind to some view — none renders nowhere', () => {
		const routed = new Set<string>([...COMMENT_KINDS, ...CHANGE_KINDS, ...VERSION_KINDS]);
		const orphaned = ALL_TIMELINE_KINDS.filter((k) => !routed.has(k));

		// BUG-2301's class, as a test rather than a comment: a kind in none of
		// the three filters is invisible everywhere and nothing reports it.
		expect(orphaned).toEqual([]);
	});

	it('keeps the three views disjoint, so nothing renders twice', () => {
		const all = [...COMMENT_KINDS, ...CHANGE_KINDS, ...VERSION_KINDS];
		expect(new Set(all).size).toBe(all.length);
	});
});
