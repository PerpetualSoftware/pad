/**
 * TASK-2228 — a split-pane switch makes each request once.
 *
 * THE DEFECT. The route effect that calls `loadData` runs AFTER the render
 * that carries the new `ref`. For that one render the loaded body was mounted
 * for the new ref with the PREVIOUS item still loaded, so every panel under
 * `{#key itemSlug}` mounted (and fetched) for a ref/item pair that does not
 * exist; then `loading` flipped, the body unmounted behind the skeleton, and
 * the panels mounted and fetched again when the load landed. A switch fetched
 * the timelines, children and backlinks twice (measured by
 * e2e/task-2228-pane-switch-requests.spec.ts: 20 requests per switch before,
 * 14 after, no duplicates).
 *
 * The panels here are the recording stub, so a mount is a props snapshot: the
 * first leg asserts no panel ever mounts with an `itemSlug` naming one item
 * and an `itemId` naming another. The second pins that progress and links are
 * issued together rather than one after the other.
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
vi.mock('$lib/components/BacklinksPanel.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/ChildItems.svelte', () => import('../../../test/StubComponent.svelte'));
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
vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: { bootstrap: vi.fn(async () => {}), getAll: () => [], retagCollection: vi.fn(), bootstrapStateFor: () => 'ready' },
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

const props = (over: Record<string, unknown> = {}) => ({ username: 'u', wsSlug: 'ws', collSlug: 'tasks', ref: 'i1', ...over });

async function settle() {
	flushSync();
	await tick();
	await new Promise((r) => setTimeout(r, 0));
	await tick();
}

type Rec = Record<string, unknown>;
/** The identity each stub mounted with. The stub records its live props
 *  object, which reads the CURRENT values later, so this snapshots the two
 *  identity props at the moment the stub pushes (its mount). */
let mounts: { itemSlug: unknown; itemId: unknown }[] = [];
/** Panel mounts: stubs that carry both halves of an item identity. */
const panelMounts = () => mounts.filter((p) => typeof p.itemSlug === 'string' && typeof p.itemId === 'string');

beforeEach(() => {
	mounts = [];
	const sink: Rec[] = [];
	sink.push = (...recs: Rec[]) => {
		for (const r of recs) mounts.push({ itemSlug: r.itemSlug, itemId: r.itemId });
		return Array.prototype.push.apply(sink, recs);
	};
	(globalThis as { __stubProps?: Rec[] }).__stubProps = sink;
	vi.mocked(api.items.progress).mockClear();
	vi.mocked(api.links.list).mockClear();
});

afterEach(() => {
	cleanup();
	delete (globalThis as { __stubProps?: Rec[] }).__stubProps;
});

describe('a split-pane switch (TASK-2228)', () => {
	it('never mounts a panel for the new ref with the previous item', async () => {
		const r = render(ItemDetail, { props: props({ embedded: true }) });
		await settle();
		await waitFor(() => expect(r.container.textContent).toContain('Item i1'));
		expect(panelMounts().length, 'the panels must mount at all, or the leg below is vacuous').toBeGreaterThan(0);
		const before = panelMounts().length;
		for (const next of ['i2', 'i3']) {
			await r.rerender(props({ embedded: true, ref: next }));
			await settle();
			await waitFor(() => expect(r.container.textContent).toContain(`Item ${next}`));
		}
		const switched = panelMounts().slice(before);
		expect(switched.length).toBeGreaterThan(0);
		const mismatched = switched.filter((p) => p.itemSlug !== p.itemId).map((p) => `${p.itemSlug}≠${p.itemId}`);
		expect(mismatched).toEqual([]);
	});

	it('issues progress and links together, not one after the other', async () => {
		let releaseProgress: () => void = () => {};
		const r = render(ItemDetail, { props: props({ embedded: true }) });
		await settle();
		await waitFor(() => expect(r.container.textContent).toContain('Item i1'));
		vi.mocked(api.items.progress).mockImplementationOnce(
			() => new Promise((res) => { releaseProgress = () => res({ total: 0, done: 0, percentage: 0 } as never); }),
		);
		vi.mocked(api.links.list).mockClear();
		await r.rerender(props({ embedded: true, ref: 'i2' }));
		await settle();
		await waitFor(() => expect(vi.mocked(api.items.progress)).toHaveBeenCalledWith('ws', 'i2'));
		// progress has not answered; links must already be on the wire.
		expect(vi.mocked(api.links.list)).toHaveBeenCalledWith('ws', 'i2');
		releaseProgress();
		await waitFor(() => expect(r.container.textContent).toContain('Item i2'));
	});
});
