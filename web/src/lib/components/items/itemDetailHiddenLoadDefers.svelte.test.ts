/**
 * BUG-3192 Unit B (2): a tab loaded HIDDEN issues its essentials only, and
 * the rest when it is first shown, exactly once (lead ruling).
 *
 * Session restore and middle-click open tabs hidden, several at once, against
 * one per-user request budget. The gated panels (decisions, attachments,
 * comments timeline, children, backlinks, relation backlinks) are nobody's to
 * see yet. They mount behind `visibility.seenVisible`, which flips once and
 * never back, so nothing is ever unmounted (PLAN-2290's rule stands).
 *
 * Every gated panel is a stub here, so "its read was issued" is observed as
 * "it mounted": each one's read happens in its own mount.
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
import { visibility } from '$lib/services/visibility.svelte';
import ItemDetail from './ItemDetail.svelte';

type Props = Record<string, unknown>;
const stubs = () => (globalThis as { __stubProps?: Props[] }).__stubProps ?? [];

/** A stub whose props are EXACTLY these keys (two panels share a prop name with a dialog). */
const exactly = (...keys: string[]) => (p: Props) => Object.keys(p).sort().join(',') === [...keys].sort().join(',');

/** Each gated panel, by props only it receives. */
const GATED: Record<string, (p: Props) => boolean> = {
	decisions: exactly('itemId', 'itemRef', 'wsSlug'),
	attachments: (p) => 'canDelete' in p,
	timeline: (p) => p.title === 'Comments',
	children: (p) => typeof p.onChildrenChange === 'function',
	backlinks: (p) => typeof p.onCountChange === 'function',
	relationBacklinks: exactly('itemId', 'itemSlug', 'username', 'wsSlug'),
};
const mounts = () =>
	Object.fromEntries(Object.entries(GATED).map(([k, f]) => [k, stubs().filter(f).length]));
const none = Object.fromEntries(Object.keys(GATED).map((k) => [k, 0]));
const once = Object.fromEntries(Object.keys(GATED).map((k) => [k, 1]));

let hidden = false;
function setHidden(h: boolean) {
	hidden = h;
	document.dispatchEvent(new Event('visibilitychange'));
}

async function settle() {
	flushSync();
	await tick();
	await new Promise((r) => setTimeout(r, 0));
	await tick();
}

const mountItem = () =>
	render(ItemDetail, { props: { username: 'u', wsSlug: 'ws', collSlug: 'tasks', ref: 'p1' } });

beforeEach(() => {
	(globalThis as { __stubProps?: Props[] }).__stubProps = [];
	Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden });
	vi.mocked(api.items.get).mockClear();
	vi.mocked(api.collections.get).mockClear();
});

afterEach(() => {
	cleanup();
	hidden = false;
	visibility.resetSeenVisibleForTest(false);
});

describe('a hidden load defers the non-essential reads (BUG-3192 Unit B)', () => {
	it('CONTROL: a visible load mounts every gated panel at once', async () => {
		hidden = false;
		visibility.resetSeenVisibleForTest(false);
		const r = mountItem();
		await waitFor(() => expect(r.container.textContent).toContain('Item p1'));
		await settle();
		expect(mounts()).toEqual(once);
	});

	it('a hidden load issues the essentials only, then the rest on first visible, exactly once', async () => {
		hidden = true;
		visibility.resetSeenVisibleForTest(true);
		const r = mountItem();
		await waitFor(() => expect(r.container.textContent).toContain('Item p1'));
		await settle();
		expect(vi.mocked(api.items.get)).toHaveBeenCalledTimes(1);
		expect(vi.mocked(api.collections.get)).toHaveBeenCalledTimes(1);
		expect(mounts()).toEqual(none);

		setHidden(false);
		await settle();
		expect(mounts()).toEqual(once);

		// Hidden and shown again: one-way, so nothing remounts.
		setHidden(true);
		await settle();
		setHidden(false);
		await settle();
		expect(mounts()).toEqual(once);
		// And the reveal did not reload the item.
		expect(vi.mocked(api.items.get)).toHaveBeenCalledTimes(1);
	});
});
