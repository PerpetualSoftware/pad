// BUG-3102, behavioural half: the collection page must RENDER the server's
// reason for a refused bulk restore, not merely have a builder that could.
//
// `$lib/collections/bulkFailureReason.test.ts` owns the sentence — every case,
// every literal. What it cannot say is whether the page calls it, passes it the
// rows it accumulated, and shows the result. A correct builder nobody invokes
// looks identical to a fixed bug from the outside, which is the whole reason
// this file exists rather than relying on the unit legs alone.
//
// The scenario driven here is the one in the report, end to end: archive a
// batch, a teammate fills the workspace, click Undo, and the restore is refused
// at the item cap. Before this fix the user saw "3 failed" and was never told
// about the cap or where to lift it.
import { describe, expect, it, vi, beforeEach } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';

vi.mock('$lib/components/collections/BoardView.svelte', () => import('../../../../test/StubComponent.svelte'));
vi.mock('$lib/components/collections/ListView.svelte', () => import('../../../../test/StubComponent.svelte'));
vi.mock('$lib/components/collections/TableView.svelte', () => import('../../../../test/StubComponent.svelte'));
vi.mock('$lib/components/collections/PaneHost.svelte', () => import('../../../../test/StubComponent.svelte'));
vi.mock('$lib/components/collections/FilterBar.svelte', () => import('../../../../test/StubComponent.svelte'));
vi.mock('$lib/components/collections/EditCollectionModal.svelte', () => import('../../../../test/StubComponent.svelte'));
vi.mock('$lib/components/ShareDialog.svelte', () => import('../../../../test/StubComponent.svelte'));
vi.mock('$lib/components/SSEStatusIndicator.svelte', () => import('../../../../test/StubComponent.svelte'));

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

vi.mock('$app/navigation', async (importOriginal) => {
	const actual = (await importOriginal()) as Record<string, unknown>;
	return { ...actual, goto: vi.fn(async () => {}), beforeNavigate: () => {}, afterNavigate: () => {} };
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
import CollectionPage from './+page.svelte';

const PLAN_LIMIT = "You've reached the 100-item limit on the free plan.";
const COLLECTION = {
	id: 'c1', slug: 'tasks', name: 'Tasks',
	schema: JSON.stringify({ fields: [{ key: 'status', type: 'select', options: ['open', 'done'] }] }),
	settings: '{}', updated_at: '2026-01-01T00:00:00Z',
};

type Props = Record<string, unknown>;
function stubProps(): Props[] { return (globalThis as { __stubProps?: Props[] }).__stubProps ?? []; }
function findProp<T>(name: string): T | undefined {
	for (const p of stubProps()) if (typeof p[name] === 'function') return p[name] as T;
	return undefined;
}
async function waitForBulk(index: number) {
	return waitFor(() => {
		if (deferredBulk.length <= index) throw new Error(`no bulk request ${index} yet`);
		return deferredBulk[index]!;
	});
}
async function mountPage() {
	(globalThis as { __stubProps?: Props[] }).__stubProps = [];
	toasts.length = 0;
	collectionGets.length = 0;
	deferredBulk.length = 0;
	page.params = { username: 'dave', workspace: 'ws', collection: 'tasks' };
	page.url = new URL('http://localhost/dave/ws/tasks');
	render(CollectionPage);
	const get = await waitFor(() => {
		if (collectionGets.length === 0) throw new Error('no collection fetch yet');
		return collectionGets[0]!;
	});
	get.resolve({ ...COLLECTION });
	await waitFor(() => {
		if (!findProp('onArchiveColumn')) throw new Error('views not rendered yet');
	});
}

/** Archive three rows, then click the Undo the success toast carries. */
async function archiveThenUndo(items: Array<{ id: string }>) {
	const archive = findProp<(items: unknown[], status?: string) => void>('onArchiveColumn')!;
	archive(items, 'open');
	const first = await waitForBulk(0);
	first.resolve({ op: 'archive', updated: items.map((i) => ({ id: i.id })), failed: [], total: items.length });
	const undo = await waitFor(() => {
		const t = toasts.find((x) => x.action?.label === 'Undo');
		if (!t) throw new Error('no Undo toast yet');
		return t.action!;
	});
	toasts.length = 0;
	undo.onAction();
	return waitForBulk(1);
}

const ITEMS = [{ id: 'a' }, { id: 'b' }, { id: 'c' }];

describe('BUG-3102: a refused bulk restore tells the user why', () => {
	beforeEach(() => { vi.clearAllMocks(); });

	it('every row refused: the toast names the cap instead of a bare count', async () => {
		await mountPage();
		const restore = await archiveThenUndo(ITEMS);
		restore.resolve({
			op: 'restore',
			updated: [],
			failed: ITEMS.map((i) => ({ ref: i.id, error: PLAN_LIMIT, code: 'plan_limit_exceeded' })),
			total: 3,
		});
		const t = await waitFor(() => {
			if (toasts.length === 0) throw new Error('no toast yet');
			return toasts[toasts.length - 1]!;
		});
		expect(t.message).toBe(`3 items could not be restored — 3 × ${PLAN_LIMIT}`);
		expect(t.type).toBe('error');
		// The pre-fix wording, asserted absent so a regression cannot pass by
		// merely containing the new text somewhere.
		expect(t.message).not.toContain('Failed to');
	});

	it('mixed: says how many came back, how many did not, and why', async () => {
		await mountPage();
		const restore = await archiveThenUndo(ITEMS);
		restore.resolve({
			op: 'restore',
			updated: [{ id: 'a' }],
			failed: [
				{ ref: 'b', error: PLAN_LIMIT, code: 'plan_limit_exceeded' },
				{ ref: 'c', error: PLAN_LIMIT, code: 'plan_limit_exceeded' },
			],
			total: 3,
		});
		const t = await waitFor(() => {
			const last = toasts[toasts.length - 1];
			if (!last || !last.message.startsWith('Restored')) throw new Error('no restore toast yet');
			return last;
		});
		expect(t.message).toBe(`Restored 1 item, 2 failed — 2 × ${PLAN_LIMIT}`);
	});

	it('a thrown chunk is reported as not attempted, carrying its own error', async () => {
		await mountPage();
		const restore = await archiveThenUndo(ITEMS);
		restore.reject(Object.assign(new Error('Network request failed'), { code: 'network' }));
		const t = await waitFor(() => {
			if (toasts.length === 0) throw new Error('no toast yet');
			return toasts[toasts.length - 1]!;
		});
		// No per-row rows came back at all, so there is no reason to attribute
		// to any individual item — only the chunk's own failure.
		expect(t.message).toBe('3 items could not be restored — 3 items not attempted: Network request failed');
		expect(t.message).not.toContain(PLAN_LIMIT);
	});
});
