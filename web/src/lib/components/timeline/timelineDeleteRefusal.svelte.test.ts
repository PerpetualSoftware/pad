import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import type { TimelineResponse } from '$lib/types';

/**
 * BUG-3252. Deleting a comment that has replies used to fail the parent_id
 * foreign key, and the delete button showed "An internal error occurred".
 * The server then refused with 409 comment_has_replies and a message naming
 * the reply count; since the tombstone such a delete succeeds, and only a
 * server that predates it still sends this refusal. This pins the web door
 * for any refused delete: the timeline shows the SERVER'S message, the
 * comment stays, and nothing reloads as if it had been deleted.
 */

const REFUSAL = 'this comment has 1 reply; delete the reply first';

const listMock = vi.fn<(ws: string, slug: string) => Promise<TimelineResponse>>();
const deleteMock = vi.fn<(ws: string, id: string) => Promise<void>>();

vi.mock('$lib/api/client', () => ({
	api: {
		timeline: { list: (ws: string, slug: string) => listMock(ws, slug) },
		comments: {
			create: vi.fn(),
			update: vi.fn(),
			delete: (ws: string, id: string) => deleteMock(ws, id),
			addReaction: vi.fn(),
			removeReaction: vi.fn(),
		},
		attachments: { downloadUrl: () => '' },
	},
}));

vi.mock('$lib/services/sse.svelte', () => ({
	sseService: { onItemEvent: () => () => {} },
}));

vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		userId: 'user-1',
		user: { id: 'user-1', role: 'member' },
		identityEpoch: 0,
		identityFence: () => () => true,
		onIdentityChange: () => () => {},
	},
}));

vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { canEditItem: () => true },
}));

vi.mock('$lib/components/CommentEditor.svelte', async () => ({
	default: (await import('./fixtures/InertCommentEditor.svelte')).default,
}));

const { default: ItemTimeline } = await import('./ItemTimeline.svelte');

function timeline(): TimelineResponse {
	const c = {
		id: 'c-parent',
		item_id: 'item-a',
		workspace_id: 'ws-1',
		author: 'alice',
		user_id: 'user-1',
		body: 'the parent comment',
		created_by: 'user',
		source: 'web',
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	};
	return {
		entries: [{ id: 'e-1', kind: 'comment', created_at: c.created_at, actor: 'alice', source: 'web', comment: c }],
		has_more: false,
	};
}

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

async function settle() {
	for (let i = 0; i < 8; i++) {
		await tick();
		flushSync();
	}
}

beforeEach(() => {
	host = document.createElement('div');
	document.body.appendChild(host);
	listMock.mockReset().mockImplementation(async () => timeline());
	deleteMock.mockReset();
	vi.spyOn(window, 'confirm').mockReturnValue(true);
});

afterEach(() => {
	if (app) unmount(app);
	app = null;
	host.remove();
	vi.restoreAllMocks();
});

describe('comment delete refused because of replies (BUG-3252)', () => {
	it("shows the server's refusal and keeps the comment", async () => {
		deleteMock.mockRejectedValue(
			Object.assign(new Error(REFUSAL), {
				code: 'comment_has_replies',
				details: { comment_id: 'c-parent', reply_count: 1 },
			})
		);
		app = mount(ItemTimeline, {
			target: host,
			props: {
				wsSlug: 'ws',
				username: 'alice',
				itemSlug: 'TASK-1',
				currentContent: '',
				itemId: 'item-a',
				collectionId: 'coll-1',
				hostToken: 'host-1',
				mutationsEnabled: true,
			},
		}) as Record<string, unknown>;
		await settle();

		const btn = host.querySelector<HTMLButtonElement>('.delete-btn');
		expect(btn, 'the delete button must render for an editor').not.toBeNull();
		const listsBefore = listMock.mock.calls.length;
		btn!.click();
		await settle();

		expect(deleteMock).toHaveBeenCalledWith('ws', 'c-parent');
		expect(host.querySelector('.error')?.textContent).toContain(REFUSAL);
		expect(host.textContent).toContain('the parent comment');
		// A refused delete must not reload as if the comment were gone.
		expect(listMock.mock.calls.length).toBe(listsBefore);
	});
});
