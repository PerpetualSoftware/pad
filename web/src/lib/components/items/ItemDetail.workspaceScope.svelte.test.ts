/**
 * TASK-4902: an ItemDetail under a provided workspace scope (the home item
 * pane) takes owner and edit rights, and its collections, from that scope,
 * never from the global stores, which on home describe the shell workspace.
 */
import { describe, expect, it, vi, afterEach, beforeEach } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { flushSync, tick } from 'svelte';
import { finishBootstrap } from '../../../test/reactiveBootstrapMock.svelte';
import type { Collection, WorkspaceMembership } from '$lib/types';
import type { WorkspaceScope } from '$lib/stores/workspaceScope.svelte';

const mounted = vi.hoisted(() => [] as string[]);

vi.mock('$lib/components/editor/Editor.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/editor/EditorBubbleMenu.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/editor/EditorLinkPopover.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/editor/RawMarkdownEditor.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/fields/FieldEditor.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/fields/TagInput.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/timeline/ItemTimeline.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/timeline/HistoryView.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/timeline/TimelineEntryList.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/ChildItems.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/BacklinksPanel.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/items/ItemCommitmentPanel.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/items/ItemCommitmentLinks.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/items/DecisionChips.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/RelationBacklinksPanel.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('./ItemPicker.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/common/QuickActionsMenu.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/common/BottomSheet.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/collections/EditCollectionModal.svelte', () => ({ default: () => { mounted.push('EditCollectionModal'); } }));
vi.mock('$lib/components/ShareDialog.svelte', () => ({ default: () => { mounted.push('ShareDialog'); } }));
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
		collections: { get: vi.fn(async (_ws: string, slug: string) => collFor(slug)), list: vi.fn(async () => [collFor('shell-only')]) },
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
// The GLOBAL stores describe the shell workspace; `perms.globalOwner` is
// what they say about it.
const perms = vi.hoisted(() => ({ globalOwner: true }));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: {
		get isOwner() { return perms.globalOwner; },
		get currentMembership() { return perms.globalOwner ? { role: 'owner' } : { role: 'viewer' }; },
		canEditItem: () => perms.globalOwner,
		canEditCollection: () => perms.globalOwner,
		setCurrent: vi.fn(async () => {}),
	},
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
import WithWorkspaceScope from '$lib/stores/__fixtures__/WithWorkspaceScope.svelte';
import * as permissions from '$lib/utils/permissions';
import { page } from '$app/state';

function membership(role: WorkspaceMembership['role']): WorkspaceMembership {
	return { role, collection_access: 'all', visible_collection_ids: [], full_access_collection_ids: [], collection_grants: [], item_grants: [] };
}
/** A scope for workspace `ws`, as the home pane builds one from that workspace's /me. */
function scopeFor(role: WorkspaceMembership['role'], collections: Collection[]): WorkspaceScope {
	const m = membership(role);
	return {
		slug: 'ws', username: 'kit', isOwner: role === 'owner', currentMembership: m,
		canEditItem: (i) => permissions.canEditItem(m, i),
		canEditCollection: (id) => permissions.canEditCollection(m, id),
		collections, collectionsWorkspace: 'ws',
		collectionsAreFreshFor: (s) => s === 'ws',
		reloadCollections: async () => {},
	};
}

async function settle() {
	flushSync();
	await tick();
	await new Promise((r) => setTimeout(r, 0));
	await tick();
}

async function load(scope: WorkspaceScope) {
	const r = render(WithWorkspaceScope, { props: { scope, component: ItemDetail, props: { username: 'kit', wsSlug: 'ws', collSlug: 'tasks', ref: 'i1', embedded: true } } });
	await settle();
	finishBootstrap();
	await settle();
	await waitFor(() => expect(r.container.textContent).toContain('Item i1'));
	await settle();
	return r;
}

async function openMenu() {
	await fireEvent.click(screen.getByRole('button', { name: 'More item actions' }));
	await settle();
	return screen.getByRole('menu', { name: 'Item actions' });
}

beforeEach(() => {
	mounted.length = 0;
	// Home: no workspace route params.
	page.params = {};
});
afterEach(() => cleanup());

const scopeCollections = [collFor('tasks'), collFor('docs'), collFor('notes')].map((c) => ({ ...c, name: c.slug })) as unknown as Collection[];

describe('ItemDetail under a workspace scope (TASK-4902)', () => {
	it('a viewer scope shows no owner or edit controls while the global store says owner', async () => {
		perms.globalOwner = true;
		const { container } = await load(scopeFor('viewer', scopeCollections));
		expect(container.querySelector('button.title')).toBeNull();
		expect(mounted).not.toContain('ShareDialog');
		expect(mounted).not.toContain('EditCollectionModal');
		const menu = await openMenu();
		expect(within(menu).queryByRole('menuitem', { name: /Move to collection/ })).toBeNull();
		expect(within(menu).queryByRole('menuitem', { name: /Share/ })).toBeNull();
		expect(within(menu).queryByRole('menuitem', { name: /Delete/ })).toBeNull();
	});

	it('an owner scope shows them while the global store says viewer, and moves to the scope\'s collections', async () => {
		perms.globalOwner = false;
		const { container } = await load(scopeFor('owner', scopeCollections));
		expect(container.querySelector('button.title')).not.toBeNull();
		expect(mounted).toContain('ShareDialog');
		expect(mounted).toContain('EditCollectionModal');
		const menu = await openMenu();
		expect(within(menu).getByRole('menuitem', { name: /Share/ })).toBeInTheDocument();
		await fireEvent.click(within(menu).getByRole('menuitem', { name: /Move to collection/ }));
		await settle();
		const moveMenu = screen.getByRole('menu', { name: 'Item actions' });
		const targets = within(moveMenu).getAllByRole('menuitem').map((el) => el.textContent?.replace(/^\W+/u, '').trim());
		expect(targets).toEqual(['Back', 'docs', 'notes']);
	});
});
