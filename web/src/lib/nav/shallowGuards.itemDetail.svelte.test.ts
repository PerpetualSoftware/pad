/**
 * TASK-4315: a shallow navigation (Chat opening over the item) must not raise
 * ItemDetail's "edits haven't reached the server" question, and must not cancel.
 * The editor stays mounted underneath; nothing is lost. A real navigation with
 * the same edits at risk still asks (control case, which also proves the
 * harness really has edits at risk).
 */
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/svelte';

type Nav = { type: string; shallow: boolean; cancel: () => void };
const nav = vi.hoisted(() => ({ guards: [] as Array<(n: Nav) => void> }));
vi.mock('$app/navigation', () => ({
	beforeNavigate: (fn: (n: Nav) => void) => {
		nav.guards.push(fn);
	},
	afterNavigate: () => {},
	goto: () => Promise.resolve(),
	invalidate: () => Promise.resolve(),
	invalidateAll: () => Promise.resolve(),
	replaceState: () => {},
	pushState: () => {},
	onNavigate: () => {},
	preloadData: () => Promise.resolve(),
	preloadCode: () => Promise.resolve(),
	snapshot: () => {},
	disableScrollHandling: () => {},
}));

vi.mock('$lib/components/editor/Editor.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/editor/EditorBubbleMenu.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/editor/EditorLinkPopover.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/editor/RawMarkdownEditor.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/fields/FieldEditor.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/fields/TagInput.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/timeline/ItemTimeline.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/timeline/TimelineEntryList.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/ChildItems.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/BacklinksPanel.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/items/DecisionChips.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/RelationBacklinksPanel.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/items/ItemPicker.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/common/QuickActionsMenu.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/common/BottomSheet.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/collections/EditCollectionModal.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/ShareDialog.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/items/CopyItemDialog.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/items/PushToAgentDialog.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/items/ItemAttachmentStrip.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/attachments/AttachmentSurfaceHost.svelte', () => import('../../test/StubComponent.svelte'));
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
		editsAtRiskOnClose = true;
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


import ItemDetail from '$lib/components/items/ItemDetail.svelte';

beforeEach(() => {
	nav.guards.length = 0;
});
afterEach(() => {
	cleanup();
	vi.restoreAllMocks();
});

function go(over: Partial<Nav>) {
	let cancelled = false;
	for (const g of nav.guards) g({ type: 'goto', shallow: false, cancel: () => (cancelled = true), ...over });
	return cancelled;
}

describe('TASK-4315: the ItemDetail offline-edits guard and shallow navigation', () => {
	it('a shallow navigation neither prompts nor cancels while edits are at risk', async () => {
		const r = render(ItemDetail, { props: { username: 'u', wsSlug: 'ws', collSlug: 'tasks', ref: 'i1' } });
		await waitFor(() => expect(r.container.textContent).toContain('Item i1'));
		const ask = vi.spyOn(window, 'confirm').mockReturnValue(false);
		// Control first: the harness has edits at risk, so a real navigation asks.
		await waitFor(() => expect(go({ shallow: false })).toBe(true));
		expect(ask).toHaveBeenCalled();
		ask.mockClear();
		expect(go({ shallow: true })).toBe(false);
		expect(ask).not.toHaveBeenCalled();
	});
});
