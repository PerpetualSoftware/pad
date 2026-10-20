/**
 * TASK-3980 item 3 — an item an app's bot created reads "Created … by
 * <app name>", not the generic "by agent" that created_by stores. A person
 * acting through an app keeps their own name with the quiet "via <app>"
 * label (SPEC-6 U9c), and a plain agent write is unchanged.
 *
 * Asserts the rendered meta line of a mounted ItemDetail (CONVE-19), with
 * the same stubs as itemDetailLoadsOnce.
 */
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/svelte';
import { flushSync, tick } from 'svelte';
import { finishBootstrap, resetBootstrap } from '../../../test/reactiveBootstrapMock.svelte';

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

const attribution = vi.hoisted(() => ({ created_by: 'agent' as string, via_app_name: undefined as string | undefined }));

const coll = { id: 'c-tasks', slug: 'tasks', name: 'tasks', prefix: 'TASK', schema: '{"fields":[]}', settings: '{}' };
function item(slug: string) {
	return {
		id: slug, slug, title: `Item ${slug}`, item_number: 1, collection_slug: 'tasks', collection_id: 'c-tasks',
		fields: '{}', tags: '[]', content: '', seq: 1,
		created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
		created_by: attribution.created_by,
		...(attribution.via_app_name ? { via_app: 'inst-1', via_app_name: attribution.via_app_name } : {}),
	};
}

vi.mock('$lib/api/client', () => ({
	api: {
		items: {
			get: vi.fn(async (_ws: string, slug: string) => item(slug)),
			update: vi.fn(async (_ws: string, id: string) => item(id)),
			flushCollabContent: vi.fn(async (_ws: string, id: string) => item(id)),
			progress: vi.fn(async () => ({ total: 0, done: 0, percentage: 0 })),
		},
		collections: { get: vi.fn(async () => coll), list: vi.fn(async () => [coll]) },
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
vi.mock('$lib/stores/localIndex.svelte', async () => {
	const m = await import('../../../test/reactiveBootstrapMock.svelte');
	return {
		localIndex: { bootstrap: vi.fn(() => m.reactiveBootstrap()), getAll: () => [], retagCollection: vi.fn(), bootstrapStateFor: () => 'cold' },
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

import ItemDetail from './ItemDetail.svelte';

async function settle() {
	flushSync();
	await tick();
	await new Promise((r) => setTimeout(r, 0));
	await tick();
}

async function metaLine(createdBy: string, viaAppName?: string) {
	attribution.created_by = createdBy;
	attribution.via_app_name = viaAppName;
	const r = render(ItemDetail, { props: { username: 'u', wsSlug: 'ws', collSlug: 'tasks', ref: 'i1' } });
	await settle();
	finishBootstrap();
	await settle();
	await waitFor(() => expect(r.container.querySelector('.meta-info')).not.toBeNull());
	return (r.container.querySelector('.meta-info')?.textContent ?? '').replace(/\s+/g, ' ');
}

beforeEach(() => resetBootstrap());
afterEach(() => cleanup());

describe('item meta line names the app that created it (TASK-3980 item 3)', () => {
	it("an app's bot: by the app, no generic agent and no second via label", async () => {
		const line = await metaLine('agent', 'Helpdesk Bridge');
		expect(line).toContain('by Helpdesk Bridge');
		expect(line).not.toContain('by agent');
		expect(line).not.toContain('via Helpdesk Bridge');
	});

	it('a person acting through an app keeps their name, with the via label', async () => {
		const line = await metaLine('Dana', 'Helpdesk Bridge');
		expect(line).toContain('by Dana');
		expect(line).toContain('via Helpdesk Bridge');
	});

	it('a plain agent write is unchanged', async () => {
		const line = await metaLine('agent');
		expect(line).toContain('by agent');
		expect(line).not.toContain('via');
	});
});
