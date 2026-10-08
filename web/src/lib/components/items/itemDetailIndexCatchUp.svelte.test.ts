/**
 * BUG-3473: an item load waits for the workspace index only while the index is
 * not yet populated. On an index that is already `ready`, `bootstrap` with a
 * pending resync runs a reconcile and returns its promise, and that loop reads
 * /items-changes until a page comes back empty, which in a workspace others
 * are writing to took seconds while the pane sat on its skeleton.
 *
 * `bootstrap` here never settles: the load can only render by not waiting.
 */
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';

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
const idx = vi.hoisted(() => ({ state: 'ready' as string }));
vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: {
		bootstrap: vi.fn(() => new Promise<void>(() => {})),
		getAll: () => [],
		retagCollection: vi.fn(),
		bootstrapStateFor: () => idx.state,
	},
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


import { localIndex } from '$lib/stores/localIndex.svelte';
import ItemDetail from './ItemDetail.svelte';

const mountItem = () =>
	render(ItemDetail, { props: { username: 'u', wsSlug: 'ws', collSlug: 'tasks', ref: 'p1' } });

beforeEach(() => {
	(globalThis as { __stubProps?: unknown[] }).__stubProps = [];
	vi.mocked(localIndex.bootstrap).mockClear();
});

afterEach(() => cleanup());

describe('an item load does not wait for the index to catch up (BUG-3473)', () => {
	it('a ready index: the item renders while the catch-up is still running', async () => {
		idx.state = 'ready';
		const r = mountItem();
		await waitFor(() => expect(r.container.textContent).toContain('Item p1'));
		// The catch-up was still scheduled.
		expect(vi.mocked(localIndex.bootstrap)).toHaveBeenCalledTimes(1);
	});

	it('CONTROL: an index that is not populated yet is waited for', async () => {
		idx.state = 'loading';
		const r = mountItem();
		await new Promise((res) => setTimeout(res, 50));
		await tick();
		expect(r.container.querySelector('[aria-label="Loading content"]')).not.toBeNull();
		expect(r.container.textContent).not.toContain('Item p1');
	});
});
