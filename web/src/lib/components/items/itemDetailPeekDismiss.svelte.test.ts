/**
 * TASK-2337 — the peek-begin transition dismisses every open item-scoped
 * mutation surface (TASK-2172), so no new edit can be COMPLETED on a side that
 * has gone passive. An armed delete confirmation, a half-typed title or an
 * open add-relationship box must not sit live behind the freeze.
 *
 * WHY A MOUNT OF THE REAL ItemDetail. The reset is a state TRANSITION
 * (`peeking` false → true), which FreezeProbe's static render mirror cannot
 * express, and an e2e cannot isolate: every pointer interaction that makes a
 * side peek is also an outside-click that closes a menu on its own. Here
 * nothing but the prop changes, so the only thing that can close a surface is
 * the transition's reset.
 *
 * Each leg opens ONE surface through the component's own trigger, proves it
 * open, flips `peeking`, and proves it closed. Each was red-first against a
 * mutant that drops exactly that surface's line from the reset (the trail has
 * the receipts).
 *
 * ONE LINE HAS NO LEG OF ITS OWN, BY CONSTRUCTION: `paneMenuView = 'root'`.
 * Dropping it alone is an equivalent mutant. The reset also closes the menu,
 * and the ⋯ button, the menu's only opener, sets the view to 'root' before it
 * toggles open, so no reachable state differs. The armed-delete leg covers the
 * PAIR: dropping `paneMenuOpen = false` AND `paneMenuView = 'root'` together
 * turns it red, and so does the ⋯ menu leg.
 */
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
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

const coll = { id: 'c-tasks', slug: 'tasks', name: 'tasks', prefix: 'TASK', schema: '{"fields":[]}', settings: '{}' };
const item = {
	id: 'i1', slug: 'i1', title: 'Item i1', item_number: 1, collection_slug: 'tasks', collection_id: 'c-tasks',
	fields: '{}', tags: '[]', content: 'body', seq: 1,
	created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
};

vi.mock('$lib/api/client', () => ({
	api: {
		items: {
			get: vi.fn(async () => item),
			update: vi.fn(async () => item),
			flushCollabContent: vi.fn(async () => item),
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
vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: { bootstrap: vi.fn(async () => {}), getAll: () => [], retagCollection: vi.fn(), bootstrapStateFor: () => 'cold' },
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

type StubProps = Record<string, unknown>;
const stubs = () => (globalThis as { __stubProps?: StubProps[] }).__stubProps ?? [];
// The stub records each instance's live props object, so reading a bound prop
// later returns the parent's CURRENT value.
const stubWith = (key: string) => stubs().filter((p) => key in p).at(-1);

// `peeking` is its OWN signal, flipped directly, never through rerender().
// testing-library keeps every prop in ONE $state.raw object and replaces it on
// rerender, so every prop read depends on that one signal: a rerender that
// changes only `peeking` re-runs the route-load effect as if the item changed,
// and loadData's item-switch reset then closes every surface itself. That made
// the first draft of this suite pass against all six drop-one mutants. The
// PRECONDITION leg below pins that the peek here neither reloads nor remounts.
let peekingNow = $state(false);
const props = () => ({
	username: 'u', wsSlug: 'ws', collSlug: 'tasks', ref: 'i1',
	get peeking() { return peekingNow; },
});

async function settle() {
	flushSync();
	await tick();
	await new Promise((r) => setTimeout(r, 0));
	await tick();
}

async function mount() {
	const r = render(ItemDetail, { props: props() });
	await settle();
	await waitFor(() => expect(r.container.textContent).toContain('Item i1'));
	return r;
}

async function peek(_r: Awaited<ReturnType<typeof mount>>) {
	peekingNow = true;
	await settle();
}

const menuButton = (r: Awaited<ReturnType<typeof mount>>) =>
	r.container.querySelector('.pane-more-btn') as HTMLButtonElement;

async function openPaneMenu(r: Awaited<ReturnType<typeof mount>>) {
	await fireEvent.click(menuButton(r));
	await settle();
	expect(menuButton(r).getAttribute('aria-expanded'), 'premise: the ⋯ menu opened').toBe('true');
}

const menuItem = (label: string) =>
	Array.from(document.querySelectorAll('[role="menuitem"]')).find((el) => el.textContent?.includes(label)) as
		| HTMLElement
		| undefined;

beforeEach(() => {
	peekingNow = false;
	(globalThis as { __stubProps?: StubProps[] }).__stubProps = [];
});

afterEach(() => {
	cleanup();
	delete (globalThis as { __stubProps?: StubProps[] }).__stubProps;
});

describe('peek-begin dismisses every open mutation surface (TASK-2337)', () => {
	it('PRECONDITION: flipping peeking neither reloads nor remounts the item', async () => {
		const r = await mount();
		const gets = vi.mocked(api.items.get).mock.calls.length;
		const title = r.container.querySelector('button.title');
		await peek(r);
		expect(vi.mocked(api.items.get).mock.calls.length, 'the peek reloaded the item').toBe(gets);
		expect(r.container.querySelector('button.title'), 'the peek remounted the item').toBe(title);
	});

	it('an in-place title edit closes', async () => {
		const r = await mount();
		await fireEvent.click(r.container.querySelector('button.title') as HTMLElement);
		await settle();
		expect(r.container.querySelector('textarea.title-input'), 'premise: the title edit opened').not.toBeNull();
		await peek(r);
		expect(r.container.querySelector('textarea.title-input'), 'the title edit stayed open while peeking').toBeNull();
	});

	it('the share dialog closes', async () => {
		const r = await mount();
		await openPaneMenu(r);
		await fireEvent.click(menuItem('Share')!);
		await settle();
		expect(stubWith('targetSlug')?.open, 'premise: the share dialog opened').toBe(true);
		await peek(r);
		expect(stubWith('targetSlug')?.open, 'the share dialog stayed open while peeking').toBe(false);
	});

	it('the edit-collection modal closes', async () => {
		const r = await mount();
		(stubWith('onmanage')!.onmanage as () => void)();
		await settle();
		expect(stubWith('initialSection')?.open, 'premise: the edit-collection modal opened').toBe(true);
		await peek(r);
		expect(stubWith('initialSection')?.open, 'the edit-collection modal stayed open while peeking').toBe(false);
	});

	it('the ⋯ menu closes', async () => {
		const r = await mount();
		await openPaneMenu(r);
		await peek(r);
		expect(menuButton(r).getAttribute('aria-expanded'), 'the ⋯ menu stayed open while peeking').toBe('false');
	});

	it('an armed delete confirmation does not survive the peek', async () => {
		const r = await mount();
		await openPaneMenu(r);
		await fireEvent.click(menuItem('Delete')!);
		await settle();
		expect(menuItem('Cancel'), 'premise: the delete confirmation is armed').toBeDefined();
		await peek(r);
		expect(menuItem('Cancel'), 'the delete confirmation stayed armed while peeking').toBeUndefined();
		// Reopening shows the root view, not the confirmation.
		await openPaneMenu(r);
		expect(menuItem('Cancel'), 'the delete confirmation came back on reopen').toBeUndefined();
		expect(menuItem('Delete')).toBeDefined();
	});

	it('the add-relationship box closes', async () => {
		const r = await mount();
		await fireEvent.click(r.container.querySelector('.add-relationship-btn') as HTMLElement);
		await settle();
		expect(r.container.querySelector('.add-link-close'), 'premise: the add-relationship box opened').not.toBeNull();
		await peek(r);
		expect(r.container.querySelector('.add-link-close'), 'the add-relationship box stayed open while peeking').toBeNull();
		expect(r.container.querySelector('.add-relationship-btn')).not.toBeNull();
	});
});
