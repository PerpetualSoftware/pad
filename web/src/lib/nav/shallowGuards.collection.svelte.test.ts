// TASK-4315: a shallow navigation (Chat opening over the collection) must not
// cancel for the unsaved-draft guard or open its "Unsaved card" dialog. The
// board stays mounted underneath, so the draft is not at risk. A real
// navigation with the same draft still cancels and asks (control case).
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';

vi.mock('$lib/components/collections/BoardView.svelte', () => import('../../test/DraftBoardStub.svelte'));
vi.mock('$lib/components/collections/ListView.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/collections/TableView.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/collections/PaneHost.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/collections/FilterBar.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/collections/EditCollectionModal.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/ShareDialog.svelte', () => import('../../test/StubComponent.svelte'));
vi.mock('$lib/components/SSEStatusIndicator.svelte', () => import('../../test/StubComponent.svelte'));

const toasts = vi.hoisted(() => [] as Array<{ message: string; type?: string; action?: { label: string; onAction: () => void } }>);
vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: {
		show: (
			message: string,
			type?: string,
			_d?: number,
			_l?: string,
			action?: { label: string; onAction: () => void }
		) => {
			toasts.push({ message, type, action });
			return 'toast-id';
		},
		dismiss: () => {},
		get toasts() { return []; },
	},
	quietExternalToasts: () => false,
}));

const ROW = vi.hoisted(() => ({
	id: 'i1', slug: 'i1', title: 'Row', item_number: 1, collection_slug: 'tasks',
	fields: '{"status":"open"}', tags: '[]', sort_order: 0,
	created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
}));
const collectionGets = vi.hoisted(() => [] as Array<{ resolve: (v: unknown) => void }>);
const deferredBulk = vi.hoisted(() => [] as Array<{ resolve: (v: unknown) => void; reject: (e: unknown) => void }>);
function defer(sink: Array<{ resolve: (v: unknown) => void; reject: (e: unknown) => void }>) {
	return new Promise((resolve, reject) => { sink.push({ resolve, reject }); });
}

type Nav = {
	type: string;
	shallow: boolean;
	willUnload: boolean;
	from: { url: URL } | null;
	to: { url: URL } | null;
	cancel: () => void;
};
const nav = vi.hoisted(() => ({ before: [] as Array<(n: Nav) => void> }));
vi.mock('$app/navigation', async (importOriginal) => {
	const actual = (await importOriginal()) as Record<string, unknown>;
	return {
		...actual,
		goto: vi.fn(async () => {}),
		beforeNavigate: (fn: (n: Nav) => void) => { nav.before.push(fn); },
		afterNavigate: () => {},
	};
});

vi.mock('$lib/api/client', () => ({
	api: {
		collections: {
			get: vi.fn(() => new Promise((resolve) => { collectionGets.push({ resolve }); })),
			list: vi.fn(async () => []),
			update: vi.fn(async () => ({})),
		},
		views: { list: vi.fn(async () => []), create: vi.fn(async () => ({})), delete: vi.fn(async () => ({})) },
		members: { list: vi.fn(async () => ({ members: [], invitations: [] })) },
		items: {
			update: vi.fn(async () => ({})),
			create: vi.fn(async () => ({ id: 'i9', slug: 'i9', title: 'x', item_number: 9 })),
			restore: vi.fn(async () => ({ id: 'i1' })),
			bulk: vi.fn(() => defer(deferredBulk)),
			plansProgress: vi.fn(async () => []),
		},
		search: vi.fn(async () => ({ results: [] })),
	},
	PadApiError: class PadApiError extends Error { code = ''; },
	isPlanLimitError: (e: unknown) => (e as { code?: string })?.code === 'plan_limit_exceeded',
	planLimitMessage: (e: unknown) => (e as { message?: string })?.message ?? '',
	isConflictOrNotFound: () => false,
}));

vi.mock('$lib/collections/progressMerge', () => ({
	plansProgressToMap: () => ({}),
	fetchCollectionProgress: vi.fn(async () => ({})),
}));
vi.mock('$lib/services/sse.svelte', () => ({
	sseService: {
		connect: vi.fn(), disconnect: vi.fn(), onItemEvent: () => () => {},
		get connected() { return true; }, get state() { return 'open'; },
	},
}));
vi.mock('$lib/services/sync.svelte', () => ({ syncService: { onSync: () => () => {}, sync: vi.fn(async () => true) } }));
vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: {
		bootstrap: vi.fn(async () => {}), reconcile: vi.fn(async () => true),
		getByCollection: () => [ROW], upsert: () => {}, scopeEpochFor: () => 1,
		retagCollection: vi.fn(), reset: vi.fn(), bootstrapStateFor: () => 'ready',
		accessRevokedFor: () => false, pendingResyncFor: () => false,
		includesUnparentedMetadataFor: () => true,
	},
}));
vi.mock('$lib/stores/localSearch.svelte', () => ({
	localSearch: { epoch: () => 0, search: () => [] },
	parseSearchQuery: (q: string) => ({ text: q, body: false, collection: null, archived: false, ref: null, number: null }),
}));
vi.mock('$lib/stores/ui.svelte', () => ({
	uiStore: { registerCollectionSearch: () => {}, unregisterCollectionSearch: () => {} },
}));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: {
		setCurrent: vi.fn(async () => {}), get isOwner() { return true; },
		get currentRole() { return 'owner'; }, canEditCollection: () => true,
		get current() { return { id: 'ws1', slug: 'ws', name: 'WS' }; },
	},
}));
// Identity never moves here — this suite is about the toast's CONTENT, and the
// fence's behaviour is owned by collectionIdentityFence.svelte.test.ts. A fence
// that always holds is the right double for that: it takes the fence out of the
// set of things that could explain a missing toast.
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get identityEpoch() { return 0; },
		get userId() { return 'u1'; },
		get user() { return { id: 'u1', name: 'A', email: 'a@example.com' }; },
		get session() { return { user: { id: 'u1' } }; },
		identityFence() { return () => true; },
		onIdentityChange() { return () => {}; },
		clear() {},
	},
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: {
		loadCollections: vi.fn(async () => {}), cachedCollection: vi.fn(async () => null),
		get collections() { return []; }, get activeItem() { return null; },
	},
}));

import { page } from '$app/state';
import CollectionPage from '../../routes/[username]/[workspace]/[collection]/+page.svelte';

const COLLECTION = {
	id: 'c1', slug: 'tasks', name: 'Tasks',
	schema: JSON.stringify({ fields: [{ key: 'status', type: 'select', options: ['open', 'done'] }] }),
	settings: '{}', updated_at: '2026-01-01T00:00:00Z',
};

async function mountWithDraft() {
	(globalThis as { __draftBoard?: unknown }).__draftBoard = undefined;
	collectionGets.length = 0;
	nav.before.length = 0;
	page.params = { username: 'dave', workspace: 'ws', collection: 'tasks' };
	page.url = new URL('http://localhost/dave/ws/tasks');
	render(CollectionPage);
	const get = await waitFor(() => {
		if (collectionGets.length === 0) throw new Error('no collection fetch yet');
		return collectionGets[0]!;
	});
	get.resolve({ ...COLLECTION });
	const board = await waitFor(() => {
		const b = (globalThis as { __draftBoard?: { setDraft: (v: Record<string, string>) => void } }).__draftBoard;
		if (!b) throw new Error('board not rendered yet');
		return b;
	});
	// The board's `bind:draftText`: writing through the binding is the lane draft.
	board.setDraft({ open: 'unsaved card' });
	await tick();
}

function go(over: Partial<Nav>) {
	let cancelled = false;
	for (const g of nav.before) {
		g({
			type: 'link',
			shallow: false,
			willUnload: false,
			from: { url: new URL('http://localhost/dave/ws/tasks') },
			to: { url: new URL('http://localhost/messages') },
			cancel: () => (cancelled = true),
			...over,
		});
	}
	return cancelled;
}

beforeEach(() => { vi.clearAllMocks(); });
afterEach(() => { cleanup(); });

describe('TASK-4315: the collection draft guard and shallow navigation', () => {
	it('control: a real navigation away with a draft cancels and opens the Unsaved card dialog', async () => {
		await mountWithDraft();
		expect(go({ shallow: false })).toBe(true);
		await screen.findByText(/You have an unsaved card/);
	});

	it('a shallow navigation neither cancels nor opens the pending-nav dialog', async () => {
		await mountWithDraft();
		expect(go({ shallow: true })).toBe(false);
		await tick();
		expect(screen.queryByText(/You have an unsaved card/)).toBeNull();
	});
});
