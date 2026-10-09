/**
 * TASK-2205 (audit C37): version restore is gated on edit permission. The
 * restore UI rendered for a read-only viewer, who could only ever be refused;
 * ItemDetail froze it only while peeking. Both timeline mounts now get
 * restoreFrozen when the viewer cannot edit the item.
 */
import { describe, expect, it, vi, afterEach } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/svelte';
import { flushSync, tick } from 'svelte';
import { finishBootstrap } from '../../../test/reactiveBootstrapMock.svelte';

vi.mock('$lib/components/editor/Editor.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/editor/EditorBubbleMenu.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/editor/EditorLinkPopover.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/editor/RawMarkdownEditor.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/fields/FieldEditor.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/fields/TagInput.svelte', () => import('../../../test/StubComponent.svelte'));
// A probe in ItemTimeline's place: records the restoreFrozen each instance is given.
// The headless instance only feeds the History tab and renders nothing; the
// tab itself renders through HistoryView, probed the same way.
const probes = vi.hoisted(() => [] as Array<{ who: string; restoreFrozen: () => unknown }>);
vi.mock('$lib/components/timeline/ItemTimeline.svelte', () => ({
	default: (_anchor: unknown, props: Record<string, unknown>) => {
		if (!props.headless) probes.push({ who: 'ItemTimeline', restoreFrozen: () => props.restoreFrozen });
		// The headless owner publishes the feed the History tab renders from.
		else props.feed = { entries: [], loading: false, error: null, hasMore: false, loadMore: () => {}, refresh: () => {} };
	},
}));
vi.mock('$lib/components/timeline/HistoryView.svelte', () => ({
	default: (_anchor: unknown, props: Record<string, unknown>) => {
		probes.push({ who: 'HistoryView', restoreFrozen: () => props.restoreFrozen });
	},
}));
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
		localIndex: { bootstrap: vi.fn(() => m.reactiveBootstrap()), getAll: () => [], retagCollection: vi.fn(), bootstrapStateFor: () => 'cold' },
	};
});
const perms = vi.hoisted(() => ({ canEdit: true }));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { canEditItem: () => perms.canEdit, get isOwner() { return perms.canEdit; }, setCurrent: vi.fn(async () => {}) },
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

import ItemDetail from './ItemDetail.svelte';

async function settle() {
	flushSync();
	await tick();
	await new Promise((r) => setTimeout(r, 0));
	await tick();
}

async function load(canEdit: boolean) {
	perms.canEdit = canEdit;
	probes.length = 0;
	const r = render(ItemDetail, { props: { username: 'u', wsSlug: 'ws', collSlug: 'tasks', ref: 'i1' } });
	await settle();
	finishBootstrap();
	await settle();
	await waitFor(() => expect(r.container.textContent).toContain('Item i1'));
	await waitFor(() => { if (probes.length === 0) throw new Error('no timeline mounted'); });
}

afterEach(() => cleanup());

describe('ItemDetail: version restore needs edit permission (TASK-2205)', () => {
	it('a viewer who cannot edit gets every timeline with restore frozen', async () => {
		await load(false);
		await waitFor(() => expect(probes.map((p) => p.who).sort()).toEqual(['HistoryView', 'ItemTimeline']));
		for (const p of probes) expect(p.restoreFrozen()).toBe(true);
	});

	it('CONTROL: an editor gets restore unfrozen', async () => {
		await load(true);
		await waitFor(() => expect(probes.map((p) => p.who).sort()).toEqual(['HistoryView', 'ItemTimeline']));
		for (const p of probes) expect(p.restoreFrozen()).toBe(false);
	});
});
