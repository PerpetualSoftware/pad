/**
 * TASK-4600 PR2 fix 1: "+ Add date" lives in the item header, LAST in the
 * action row, so it shifts nothing when the commitment read lands. It shows
 * only for the current identity's snapshot of this item with no commitment
 * and the server's editable hint, and it opens the panel's dialog through the
 * `addDateOpen` prop (the panel owns the dialog and every write).
 *
 * Same stubs as itemDetailAppAttribution; the panel stub records its props,
 * so the suite publishes snapshots through the real `onSnapshot` callback.
 */
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
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
vi.mock('$lib/components/items/ItemCommitmentPanel.svelte', () => import('../../../test/StubComponent.svelte'));
vi.mock('$lib/components/items/ItemCommitmentLinks.svelte', () => import('../../../test/StubComponent.svelte'));
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

const identity = vi.hoisted(() => ({ listeners: [] as ((previous: string | null) => void)[] }));
const coll = { id: 'c-tasks', slug: 'tasks', name: 'tasks', prefix: 'TASK', schema: '{"fields":[]}', settings: '{}' };
function item(slug: string) {
	return {
		id: slug, slug, title: `Item ${slug}`, item_number: 1, collection_slug: 'tasks', collection_id: 'c-tasks',
		fields: '{}', tags: '[]', content: '', seq: 1,
		created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', created_by: 'Dana',
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
		onIdentityChange: (fn: (previous: string | null) => void) => { identity.listeners.push(fn); return () => {}; },
	},
}));

import ItemDetail from './ItemDetail.svelte';
import type { ItemCommitmentSnapshot } from '$lib/commitment';

type PanelProps = { onSnapshot: (s: ItemCommitmentSnapshot) => void; addDateOpen?: boolean; onAddDateClose?: () => void };
const sink = (globalThis as { __stubProps?: Record<string, unknown>[] });

async function settle() {
	flushSync();
	await tick();
	await new Promise((r) => setTimeout(r, 0));
	await tick();
}

function panels(): PanelProps[] {
	return (sink.__stubProps ?? []).filter((p) => typeof p.onSnapshot === 'function') as unknown as PanelProps[];
}
function panel(): PanelProps {
	const all = panels();
	expect(all.length).toBeGreaterThan(0);
	return all[all.length - 1];
}

async function mount() {
	const r = render(ItemDetail, { props: { username: 'u', wsSlug: 'ws', collSlug: 'tasks', ref: 'i1' } });
	await settle();
	finishBootstrap();
	await settle();
	await waitFor(() => expect(r.container.querySelector('.meta-actions')).not.toBeNull());
	await waitFor(() => expect(panels().length).toBeGreaterThan(0));
	return r;
}

function snapshot(extra: Partial<ItemCommitmentSnapshot> = {}): ItemCommitmentSnapshot {
	return { wsSlug: 'ws', itemId: 'i1', itemSlug: 'i1', authEpoch: 0, zone: 'UTC', view: null, editable: true, ...extra } as ItemCommitmentSnapshot;
}

async function publish(s: ItemCommitmentSnapshot) {
	panel().onSnapshot(s);
	await settle();
}

const addDate = () => screen.queryByRole('button', { name: 'Add date' });

beforeEach(() => { resetBootstrap(); sink.__stubProps = []; identity.listeners.length = 0; });
afterEach(() => { cleanup(); delete sink.__stubProps; });

describe('item header "+ Add date" (TASK-4600)', () => {
	it('is hidden until a snapshot lands, then shows LAST in the action row for an editor with no commitment', async () => {
		const r = await mount();
		expect(addDate()).toBeNull();
		await publish(snapshot());
		const button = addDate();
		expect(button).not.toBeNull();
		expect(button!.textContent?.trim()).toBe('+ Add date');
		expect(button!.classList.contains('action-btn')).toBe(true);
		const row = r.container.querySelector('.meta-actions')!;
		expect(row.lastElementChild).toBe(button);
	});

	it('clicking it opens the panel dialog through addDateOpen, and the panel closes it', async () => {
		await mount();
		await publish(snapshot());
		expect(panel().addDateOpen).toBe(false);
		await fireEvent.click(addDate()!);
		await settle();
		expect(panel().addDateOpen).toBe(true);
		panel().onAddDateClose!();
		await settle();
		expect(panel().addDateOpen).toBe(false);
	});

	it('is hidden for a reader', async () => {
		await mount();
		await publish(snapshot({ editable: false }));
		expect(addDate()).toBeNull();
	});

	it('is hidden for an item that has a commitment', async () => {
		await mount();
		await publish(snapshot({ view: { item_id: 'i1' } as unknown as ItemCommitmentSnapshot['view'] }));
		expect(addDate()).toBeNull();
	});

	it('is hidden while the read is in flight (view undefined)', async () => {
		await mount();
		await publish(snapshot({ view: undefined }));
		expect(addDate()).toBeNull();
	});

	it('is hidden for a snapshot of another item or identity epoch', async () => {
		await mount();
		await publish(snapshot({ itemSlug: 'other', itemId: 'other' }));
		expect(addDate()).toBeNull();
		await publish(snapshot({ authEpoch: 1 }));
		expect(addDate()).toBeNull();
	});

	it('hides, and drops an open dialog, once the account identity changes', async () => {
		await mount();
		await publish(snapshot());
		await fireEvent.click(addDate()!);
		await settle();
		expect(panel().addDateOpen).toBe(true);
		const stale = panel();
		expect(identity.listeners.length).toBeGreaterThan(0);
		for (const fn of identity.listeners) fn('u1');
		await settle();
		expect(addDate()).toBeNull();
		expect(panel().addDateOpen).toBe(false);
		// The superseded panel's snapshot no longer brings it back.
		stale.onSnapshot(snapshot());
		await settle();
		expect(addDate()).toBeNull();
	});
});
