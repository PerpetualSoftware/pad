import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import type { Comment, TimelineEntry, TimelineResponse } from '$lib/types';

/**
 * BUG-3437, the live half. "commented on update" comes from the timeline
 * entry's comment_on_update, which only the SERVER sets. A comment that
 * arrives while the timeline is open (an SSE comment_created, or an update
 * with --comment from the CLI or MCP) reaches the view through the SSE
 * refresh, which re-reads the timeline endpoint, so it carries the flag like
 * a reload does. This pins that: if a future change built the entry
 * client-side, the label would silently disappear until reload.
 *
 * Harness copied from timelineRefreshDeletion.svelte.test.ts (module mocks
 * are per-file).
 */

type ListParams = { limit?: number; before?: string; before_id?: string; kinds?: readonly string[] } | undefined;

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
	authStore: {
		userId: 'user-1',
		user: { id: 'user-1', role: 'member' },
		// ItemTimeline fences every load and handler on identity (BUG-3105);
		// identity never changes in this suite.
		identityFence: () => () => true,
		// The shared confirm dialog (TASK-2221) registers for identity changes on import.
		onIdentityChange: () => () => {},
	},
}));

vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { canEditItem: () => false },
}));

vi.mock('$lib/components/CommentEditor.svelte', async () => ({
	default: (await import('./fixtures/InertCommentEditor.svelte')).default,
}));

const { default: ItemTimeline } = await import('./ItemTimeline.svelte');

function entry(id: string, createdAt: string, body: string, onUpdate?: boolean): TimelineEntry {
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
	return {
		id,
		kind: 'comment',
		created_at: createdAt,
		actor: 'alice',
		source: 'cli',
		comment: { ...c, activity_id: `act-${id}` }, // every comment has one
		...(onUpdate ? { comment_on_update: true } : {})
	};
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

function labelled(body: string): boolean {
	const card = Array.from(host.querySelectorAll('.comment-card')).find((el) => (el.textContent ?? '').includes(body));
	if (!card) throw new Error(`no card for "${body}"`);
	return (card.textContent ?? '').includes('commented on update');
}

describe('"commented on update" on a comment that arrives live (BUG-3437)', () => {
	it('an SSE refresh brings the label for an update\'s comment, and not for a plain one', async () => {
		pages.push({ entries: [entry('e-plain', '2026-10-06T12:00:01Z', 'a plain comment')], has_more: false });
		pages.push({
			entries: [
				entry('e-upd', '2026-10-06T12:00:09Z', 'why I renamed it', true),
				entry('e-plain', '2026-10-06T12:00:01Z', 'a plain comment')
			],
			has_more: false
		});

		app = mount(ItemTimeline, { target: host, props }) as Record<string, unknown>;
		await settle();
		expect(labelled('a plain comment')).toBe(false);

		await fireRefresh();

		expect(labelled('why I renamed it'), 'the live update comment lost its label').toBe(true);
		expect(labelled('a plain comment')).toBe(false);
	});
});
