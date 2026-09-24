/**
 * BUG-3192 Unit A — one item-page load is ONE `loadData`, and an identity
 * change of the route (item, collection, workspace) is still a load.
 *
 * THE DEFECT. The load `$effect` called `loadData()` inside its tracking scope,
 * so every reactive read before `loadData`'s first await became a dependency of
 * the effect. `localIndex.bootstrap()` is called in that prefix and reads the
 * workspace's `bootstrapState` (`$state`), then writes it: `'cold'` →
 * `'loading'` synchronously, `'loading'` → `'ready'` when the index lands. Each
 * write re-ran the effect, so a cold item page fetched the item and its
 * collection THREE times (BUG-3192 checkpoint 1, measured on a real page:
 * runs at 447, 466 and 1017 ms, the last just after `items-index` resolved).
 *
 * WHY NO EARLIER SUITE SAW IT. Every ItemDetail mount stubs `bootstrap` as a
 * plain `async () => {}`: its values without its reactivity. This suite's fake
 * reads and writes a real `$state` in the same order, and its first leg proves
 * that before any other leg relies on it.
 *
 * THE FIX UNDER TEST. The effect reads `wsSlug`, `collSlug` and `itemSlug` as
 * its explicit dependencies and runs the body under `untrack`. The navigation
 * legs are the other half: untracking the call must not untrack the identity.
 */
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/svelte';
import { flushSync, tick } from 'svelte';
import {
	finishBootstrap,
	isBootstrapReactive,
	reactiveBootstrap,
	resetBootstrap,
} from '../../../test/reactiveBootstrapMock.svelte';

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

vi.mock('$lib/api/client', () => ({
	api: {
		items: {
			get: vi.fn(async (_ws: string, slug: string) => itemFor(slug)),
			update: vi.fn(async (_ws: string, id: string) => itemFor(id)),
			flushCollabContent: vi.fn(async (_ws: string, id: string) => itemFor(id)),
			progress: vi.fn(async () => ({ total: 0, done: 0, percentage: 0 })),
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
vi.mock('$lib/stores/localIndex.svelte', async () => {
	const m = await import('../../../test/reactiveBootstrapMock.svelte');
	return {
		localIndex: { bootstrap: vi.fn(() => m.reactiveBootstrap()), getAll: () => [], retagCollection: vi.fn() },
	};
});
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

const props = (over: Record<string, unknown> = {}) => ({ username: 'u', wsSlug: 'ws', collSlug: 'tasks', ref: 'i1', ...over });

async function settle() {
	flushSync();
	await tick();
	await new Promise((r) => setTimeout(r, 0));
	await tick();
}

const itemGets = () => vi.mocked(api.items.get).mock.calls.map((c) => `${c[0]}/${c[1]}`);
const collGets = () => vi.mocked(api.collections.get).mock.calls.map((c) => `${c[0]}/${c[1]}`);

/** Mounted on a COLD index, then the index lands: the whole first-load lifecycle. */
async function coldLoad() {
	const r = render(ItemDetail, { props: props() });
	await settle();
	finishBootstrap();
	await settle();
	await waitFor(() => expect(r.container.textContent).toContain('Item i1'));
	return r;
}

beforeEach(() => {
	resetBootstrap();
	vi.mocked(api.items.get).mockClear();
	vi.mocked(api.collections.get).mockClear();
});

afterEach(() => {
	cleanup();
});

describe('one item-page load is one loadData (BUG-3192 Unit A)', () => {
	it('PRECONDITION: the fake bootstrap re-runs an effect that calls it, as the real one does', () => {
		expect(isBootstrapReactive(), 'an inert double would make every leg below pass vacuously').toBe(true);
	});

	it('a cold mount fetches the item and its collection ONCE, across both bootstrap writes', async () => {
		await coldLoad();
		// A late re-run would land after the index resolves; give it the chance.
		await settle();
		expect(itemGets()).toEqual(['ws/i1']);
		expect(collGets()).toEqual(['ws/tasks']);
	});

	it('navigating to another item still loads it (the ref stays a tracked input)', async () => {
		const r = await coldLoad();
		await r.rerender(props({ ref: 'i2' }));
		await settle();
		await waitFor(() => expect(r.container.textContent).toContain('Item i2'));
		expect(itemGets()).toEqual(['ws/i1', 'ws/i2']);
		// And back: a revisit is a load too, not a cached no-op of the effect.
		await r.rerender(props({ ref: 'i1' }));
		await settle();
		await waitFor(() => expect(r.container.textContent).toContain('Item i1'));
		expect(itemGets()).toEqual(['ws/i1', 'ws/i2', 'ws/i1']);
	});

	it('a collection change reloads (collSlug stays a tracked input)', async () => {
		const r = await coldLoad();
		await r.rerender(props({ collSlug: 'bugs' }));
		await settle();
		await waitFor(() => expect(collGets()).toEqual(['ws/tasks', 'ws/bugs']));
		expect(itemGets()).toEqual(['ws/i1', 'ws/i1']);
	});

	it('a workspace change reloads (wsSlug stays a tracked input)', async () => {
		const r = await coldLoad();
		await r.rerender(props({ wsSlug: 'ws2' }));
		await settle();
		await waitFor(() => expect(itemGets()).toEqual(['ws/i1', 'ws2/i1']));
		expect(collGets()).toEqual(['ws/tasks', 'ws2/tasks']);
	});
});
