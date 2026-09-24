/**
 * BUG-3192 Unit B (1): the parent's completion is the SERVER's (lead ruling).
 *
 * ItemDetail used to recompute `computedOverrides.progress` from ChildItems'
 * children list, with the union of their `status` terminal_options, racing
 * the load's GET /progress. Measured on a private instance: children in a
 * collection whose done field is `stage` (board_group_by), one shipped and one
 * not, gave server 2/1/50% and client 2/0/0%, because the client read `status`,
 * which that collection does not have. 2 of 3 plain tasks done gave 66% vs 67%
 * (floor vs round). Whichever landed last won. Now:
 *   - the client never computes it; GET /progress is the one writer;
 *   - ChildItems' header takes those numbers through its `progress` prop;
 *   - a CHANGE in the children (ids or statuses) re-reads /progress, and
 *     the first report of a load does not (the load already asked).
 */
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/svelte';
import { flushSync, tick } from 'svelte';

vi.mock('$lib/components/editor/Editor.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/editor/EditorBubbleMenu.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/editor/EditorLinkPopover.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/editor/RawMarkdownEditor.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/fields/FieldEditor.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/fields/TagInput.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/timeline/ItemTimeline.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/timeline/TimelineEntryList.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/ChildItems.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/BacklinksPanel.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/items/DecisionChips.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/RelationBacklinksPanel.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('./ItemPicker.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/common/QuickActionsMenu.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/common/BottomSheet.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/collections/EditCollectionModal.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/ShareDialog.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/items/CopyItemDialog.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/items/PushToAgentDialog.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/items/ItemAttachmentStrip.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/attachments/AttachmentSurfaceHost.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/collab/wsProvider.svelte', () => ({
	CollabProvider: class {
		state = 'connecting';
		synced = false;
		lastOpLogID = undefined;
		itemID: string;
		awareness = { setLocalStateField() {}, on() {}, off() {}, getStates: () => new Map() };
		constructor(itemId: string) {
			this.itemID = itemId;
		}
		destroy() {}
	},
}));

const collFor = (slug: string) => ({
	id: `c-${slug}`, slug, name: slug, prefix: 'TASK',
	schema: '{"fields":[]}', settings: '{}',
});
function itemFor(slug: string, collSlug = 'tasks') {
	return {
		id: slug, slug, title: `Item ${slug}`, item_number: 1, collection_slug: collSlug, collection_id: `c-${collSlug}`,
		fields: '{}', tags: '[]', content: `body of ${slug}`, seq: 1,
		created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
	};
}


const prog = vi.hoisted(() => ({ next: { total: 3, done: 2, percentage: 66 } }));
vi.mock('$lib/api/client', () => ({
	api: {
		items: {
			get: vi.fn(async (_ws: string, slug: string) => itemFor(slug)),
			update: vi.fn(async (_ws: string, id: string) => itemFor(id)),
			flushCollabContent: vi.fn(async (_ws: string, id: string) => itemFor(id)),
			progress: vi.fn(async () => ({ ...prog.next })),
		},
		collections: { get: vi.fn(async (_ws: string, slug: string) => collFor(slug)), list: vi.fn(async () => [collFor('tasks')]) },
		links: { list: vi.fn(async () => []) },
		members: { list: vi.fn(async () => ({ members: [] })) },
		agentRoles: { list: vi.fn(async () => []) },
		tags: { list: vi.fn(async () => []) },
	},
	PadApiError: class PadApiError extends Error { code = ''; },
	isUpdateConflictError: () => false,
}));
vi.mock('$lib/services/sse.svelte', () => ({
	sseService: {
		onItemEvent: () => () => {},
		connect: vi.fn(), disconnect: vi.fn(),
		get connected() { return true; }, get state() { return 'open'; },
	},
}));
vi.mock('$lib/services/sync.svelte', () => ({
	syncService: { onSync: () => () => {}, markSynced: vi.fn() },
}));
// Delegates to the REACTIVE fake. A factory may not close over an import, so it
// imports the module itself.
vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: { bootstrap: vi.fn(async () => {}), getAll: () => [], retagCollection: vi.fn() },
}));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { canEditItem: () => true, get isOwner() { return true; }, setCurrent: vi.fn(async () => {}) },
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get identityEpoch() { return 0; },
		get userId() { return 'u1'; },
		get user() { return { id: 'u1', name: 'A' }; },
		get authenticated() { return true; },
		identityFence: () => () => true,
		onIdentityChange: () => () => {},
	},
}));


import { api } from '$lib/api/client';
import ItemDetail from './ItemDetail.svelte';

type Props = Record<string, unknown>;
const stubs = () => (globalThis as { __stubProps?: Props[] }).__stubProps ?? [];
/** The ChildItems stub: the only stub handed `onChildrenChange`. */
const childItems = () => {
	const p = stubs().find((s) => typeof s.onChildrenChange === 'function');
	if (!p) throw new Error('ChildItems did not mount');
	return p as { onChildrenChange: (c: unknown[]) => void; progress?: { done: number; total: number; percentage: number } };
};
const child = (id: string, status: string) => ({ ...itemFor(id), fields: JSON.stringify({ status }) });
const progressGets = () => vi.mocked(api.items.progress).mock.calls.length;

async function settle() {
	flushSync();
	await tick();
	await new Promise((r) => setTimeout(r, 0));
	await tick();
}

async function loaded() {
	const r = render(ItemDetail, { props: { username: 'u', wsSlug: 'ws', collSlug: 'tasks', ref: 'p1' } });
	await waitFor(() => expect(r.container.textContent).toContain('Item p1'));
	await waitFor(() => expect(childItems().progress).toEqual({ done: 2, total: 3, percentage: 66 }));
	return r;
}

beforeEach(() => {
	(globalThis as { __stubProps?: Props[] }).__stubProps = [];
	prog.next = { total: 3, done: 2, percentage: 66 };
	vi.mocked(api.items.progress).mockClear();
});

afterEach(() => cleanup());

describe('the parent progress is the server\'s (BUG-3192 Unit B)', () => {
	it('the children list does NOT overwrite it: 2 of 3 done stays 66%, not 67%', async () => {
		await loaded();
		childItems().onChildrenChange([child('a', 'done'), child('b', 'done'), child('c', 'open')]);
		await settle();
		expect(childItems().progress).toEqual({ done: 2, total: 3, percentage: 66 });
	});

	it('the first report of a load costs no request; the load already asked', async () => {
		await loaded();
		const before = progressGets();
		childItems().onChildrenChange([child('a', 'done'), child('b', 'done'), child('c', 'open')]);
		await settle();
		expect(progressGets()).toBe(before);
	});

	it('a CHANGED child status re-reads the server, and the header follows it', async () => {
		await loaded();
		childItems().onChildrenChange([child('a', 'done'), child('b', 'done'), child('c', 'open')]);
		await settle();
		const before = progressGets();
		prog.next = { total: 3, done: 3, percentage: 100 };
		childItems().onChildrenChange([child('a', 'done'), child('b', 'done'), child('c', 'done')]);
		await waitFor(() => expect(childItems().progress).toEqual({ done: 3, total: 3, percentage: 100 }));
		expect(progressGets()).toBe(before + 1);
	});

	it('ORDERING: of two overlapping re-reads, the older answer landing last does not win (codex r1)', async () => {
		await loaded();
		childItems().onChildrenChange([child('a', 'done'), child('b', 'done'), child('c', 'open')]);
		await settle();
		// First change: its re-read is HELD, and its answer is the older one.
		let releaseFirst!: (v: unknown) => void;
		vi.mocked(api.items.progress).mockImplementationOnce(
			() => new Promise((r) => { releaseFirst = r; }) as never,
		);
		childItems().onChildrenChange([child('a', 'done'), child('b', 'open'), child('c', 'open')]);
		await settle();
		// Second change: its re-read answers at once, with the newer numbers.
		prog.next = { total: 3, done: 3, percentage: 100 };
		childItems().onChildrenChange([child('a', 'done'), child('b', 'done'), child('c', 'done')]);
		await waitFor(() => expect(childItems().progress).toEqual({ done: 3, total: 3, percentage: 100 }));
		releaseFirst({ total: 3, done: 1, percentage: 33 });
		await settle();
		expect(childItems().progress).toEqual({ done: 3, total: 3, percentage: 100 });
	});

	it('a FAILED load read leaves no numbers, never a local count, and the first report re-asks (codex r1)', async () => {
		vi.mocked(api.items.progress).mockImplementationOnce(async () => {
			throw new Error('blip');
		});
		const r = render(ItemDetail, { props: { username: 'u', wsSlug: 'ws', collSlug: 'tasks', ref: 'p1' } });
		await waitFor(() => expect(r.container.textContent).toContain('Item p1'));
		await settle();
		expect(childItems().progress, 'no server numbers yet').toBeUndefined();
		const before = progressGets();
		childItems().onChildrenChange([child('a', 'done'), child('b', 'done'), child('c', 'open')]);
		await waitFor(() => expect(childItems().progress).toEqual({ done: 2, total: 3, percentage: 66 }));
		expect(progressGets()).toBe(before + 1);
	});

	it('an UNCHANGED re-report (an SSE echo) costs no request', async () => {
		await loaded();
		const kids = [child('a', 'done'), child('b', 'done'), child('c', 'open')];
		childItems().onChildrenChange(kids);
		await settle();
		const before = progressGets();
		childItems().onChildrenChange([...kids].reverse());
		await settle();
		expect(progressGets()).toBe(before);
	});
});
