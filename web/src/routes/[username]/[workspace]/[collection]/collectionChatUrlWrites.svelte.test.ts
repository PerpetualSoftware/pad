// TASK-4315 PR 3b: the board's filter, search and view writes under an open Chat.
// Mounts the real collection page (children stubbed, as collectionIdentityFence
// does) and drives its handlers through the props the views receive. The page
// holds its filter state locally, so a deferred write changes nothing on screen;
// what these assert is WHEN the URL is written: never under an open Chat, once
// after it closes, carrying the last state.
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { bindReactiveEpoch } from '../../../../test/identityEpochMock.svelte';
import { bindReactiveCounter } from '../../../../test/reactiveCounter.svelte';

vi.mock('$lib/components/collections/BoardView.svelte', () => import('../../../../test/StubComponent.svelte'));
vi.mock('$lib/components/collections/ListView.svelte', () => import('../../../../test/StubComponent.svelte'));
vi.mock('$lib/components/collections/TableView.svelte', () => import('../../../../test/StubComponent.svelte'));
vi.mock('$lib/components/collections/PaneHost.svelte', () => import('../../../../test/StubComponent.svelte'));
vi.mock('$lib/components/collections/FilterBar.svelte', () => import('../../../../test/StubComponent.svelte'));
vi.mock('$lib/components/collections/EditCollectionModal.svelte', () => import('../../../../test/StubComponent.svelte'));
vi.mock('$lib/components/ShareDialog.svelte', () => import('../../../../test/StubComponent.svelte'));
vi.mock('$lib/components/SSEStatusIndicator.svelte', () => import('../../../../test/StubComponent.svelte'));

const toasts = vi.hoisted(() => [] as Array<{ message: string; action?: { label: string; onAction: () => void } }>);
vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: {
		show: (
			message: string,
			_t?: string,
			_d?: number,
			_l?: string,
			action?: { label: string; onAction: () => void }
		) => {
			toasts.push({ message, action });
			return 'toast-id';
		},
		dismiss: () => {},
		get toasts() {
			return [];
		},
	},
	quietExternalToasts: () => false,
}));


const deferredBulk = vi.hoisted(() => [] as Array<{ resolve: (v: unknown) => void; reject: (e: unknown) => void }>);
const deferredUpdate = vi.hoisted(() => [] as Array<{ resolve: (v: unknown) => void; reject: (e: unknown) => void }>);
/**
 * The lane-reorder conflict path (BUG-3238). Off by default, so every other leg
 * sees the resolving `collections.update` and the `isConflictOrNotFound: false`
 * it always had.
 */
const reorderConflict = vi.hoisted(() => ({
	on: false,
	updates: [] as Array<{ resolve: (v: unknown) => void; reject: (e: unknown) => void }>,
	lists: 0,
}));
const upserts = vi.hoisted(() => [] as unknown[]);
const sseHandlers = vi.hoisted(() => [] as Array<(e: unknown) => unknown>);
const gotos = vi.hoisted(() => [] as string[]);
const navGuards = vi.hoisted(() => [] as Array<(nav: unknown) => void>);
// `goto` MOVES `page.url` here, unlike the shared no-op mock.
//
// Same lesson as the epoch double one level down: a mock
// that models a dependency's SHAPE but not the behaviour the code depends on
// turns the assertion into a tautology. The per-session reset deletes `?q=` so
// that `loadUrlFilters()` — which the identity reload calls on success — has
// nothing to restore. Against a no-op `goto`, the URL never changes, the query
// comes back, and a leg asserting the end state passes only because the reload
// never completed.
vi.mock('$app/navigation', async (importOriginal) => {
	const actual = (await importOriginal()) as Record<string, unknown>;
	const { page } = (await import('$app/state')) as { page: { url: URL } };
	return {
		...actual,
		beforeNavigate: (fn: (nav: unknown) => void) => {
			navGuards.push(fn);
		},
		goto: vi.fn(async (url: string | URL) => {
			gotos.push(String(url));
			page.url = new URL(String(url), page.url);
		}),
	};
});

const collectionGets = vi.hoisted(() => [] as Array<{ resolve: (v: unknown) => void }>);

/**
 * One row, because the views (and therefore the handler props this suite
 * drives) only render on a NON-EMPTY collection — an empty one shows the
 * empty-state box instead.
 */
const ROW = vi.hoisted(() => ({
	id: 'i1',
	slug: 'i1',
	title: 'Row',
	item_number: 1,
	collection_slug: 'tasks',
	fields: '{"status":"open"}',
	tags: '[]',
	sort_order: 0,
	created_at: '2026-01-01T00:00:00Z',
	updated_at: '2026-01-01T00:00:00Z',
}));

/**
 * The reorder settle (BUG-3699 / BUG-3700). `rows` replaces the one-row
 * collection when a case needs a lane of two; `current` is what the local
 * index holds for an id NOW (a collaborator's newer row, say). Both default
 * to off, so every other leg sees the suite as it was.
 */
const reorderState = vi.hoisted(() => ({
	rows: null as null | Array<Record<string, unknown>>,
	current: new Map<string, Record<string, unknown>>(),
	// LIVE: the lane is the current rows, as the real store's is, so a reorder
	// that starts while another holds a card finds that one's optimistic row.
	live: false,
	ver: { read: null as null | (() => number), write: null as null | ((n: number) => void) },
}));

function defer<T>(sink: Array<{ resolve: (v: unknown) => void; reject: (e: unknown) => void }>): Promise<T> {
	return new Promise<T>((resolve, reject) => {
		sink.push({ resolve: resolve as (v: unknown) => void, reject });
	});
}

vi.mock('$lib/api/client', () => ({
	api: {
		collections: {
			get: vi.fn(() => new Promise((resolve) => { collectionGets.push({ resolve }); })),
			list: vi.fn(async () => {
				reorderConflict.lists++;
				return [];
			}),
			update: vi.fn(() => (reorderConflict.on ? defer(reorderConflict.updates) : Promise.resolve({}))),
		},
		views: { list: vi.fn(async () => []), create: vi.fn(async () => ({})), delete: vi.fn(async () => ({})) },
		members: { list: vi.fn(async () => ({ members: [], invitations: [] })) },
		items: {
			update: vi.fn(() => defer(deferredUpdate)),
			create: vi.fn(async () => ({ id: 'i9', slug: 'i9', title: 'x', item_number: 9 })),
			restore: vi.fn(async () => ({ id: 'i1' })),
			bulk: vi.fn(() => defer(deferredBulk)),
			plansProgress: vi.fn(async () => []),
			reorder: vi.fn(async () => []),
			get: vi.fn(async () => ({})),
		},
		search: vi.fn(async () => ({ results: [] })),
	},
	PadApiError: class PadApiError extends Error { code = ''; },
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
	isConflictOrNotFound: () => reorderConflict.on,
}));

vi.mock('$lib/collections/progressMerge', () => ({
	plansProgressToMap: () => ({}),
	fetchCollectionProgress: vi.fn(async () => ({})),
}));

vi.mock('$lib/services/sse.svelte', () => ({
	sseService: {
		connect: vi.fn(),
		disconnect: vi.fn(),
		onItemEvent: (fn: (e: unknown) => unknown) => {
			sseHandlers.push(fn);
			return () => {};
		},
		get connected() { return true; },
		get state() { return 'open'; },
	},
}));

vi.mock('$lib/services/sync.svelte', () => ({
	syncService: { onSync: () => () => {}, sync: vi.fn() },
}));

vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: {
		bootstrap: vi.fn(async () => {}),
		reconcile: vi.fn(async () => true),
		getByCollection: () => {
			reorderState.ver.read?.();
			if (reorderState.live) return [...reorderState.current.values()];
			return reorderState.rows ?? [ROW];
		},
		findByIdOrSlug: (_ws: string, id: string) => reorderState.current.get(id) ?? null,
		upsert: (_ws: string, row: unknown) => {
			upserts.push(row);
			// For the reorder cases, the real seq guard (localIndex.upsert):
			// a stamped row at or below a stamped row already held is refused.
			if (!reorderState.rows) return;
			const r = row as { id: string; seq?: number };
			const held = reorderState.current.get(r.id) as { seq?: number } | undefined;
			if (held?.seq !== undefined && r.seq !== undefined && r.seq <= held.seq) return;
			reorderState.current.set(r.id, row as Record<string, unknown>);
			if (reorderState.live) reorderState.ver.write?.((reorderState.ver.read?.() ?? 0) + 1);
		},
		scopeEpochFor: () => 1,
		markScopeStale: vi.fn(),
		retagCollection: vi.fn(),
		reset: vi.fn(),
		bootstrapStateFor: () => 'ready',
		accessRevokedFor: () => false,
		pendingResyncFor: () => false,
		includesUnparentedMetadataFor: () => true,
	},
}));

vi.mock('$lib/stores/localSearch.svelte', () => ({
	localSearch: { epoch: () => 0, search: () => [] },
	// `body:` routes to the SERVER FTS path, which is the one that schedules a
	// 200ms timer — the deferred-timer case below needs it.
	// `is:archived` is honoured so the TASK-2864 case below can drive it.
	parseSearchQuery: (q: string) => ({
		text: q.replace(/^is:archived /, '').replace(/^body:/, ''),
		body: q.replace(/^is:archived /, '').startsWith('body:'),
		collection: null,
		archived: q.startsWith('is:archived '),
		ref: null,
		number: null,
	}),
}));

const searchOpeners = vi.hoisted(() => [] as Array<() => void>);
vi.mock('$lib/stores/ui.svelte', () => ({
	uiStore: {
		registerCollectionSearch: (fn: () => void) => { searchOpeners.push(fn); },
		unregisterCollectionSearch: () => {},
	},
}));

vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: {
		setCurrent: vi.fn(async () => {}),
		get isOwner() { return true; },
		get currentRole() { return 'owner'; },
		canEditCollection: () => true,
		canEditItem: () => true,
		get current() { return { id: 'ws1', slug: 'ws', name: 'WS' }; },
	},
}));

/**
 * A FAKE auth store, on #1370's pattern, and the boundary it draws is stated
 * rather than implied: it moves ONLY the epoch, so the real identity-change
 * RELOAD path (`routes/+layout.svelte`, keyed on the user id) never fires here.
 * That is deliberate — the reload is the mechanism that would otherwise tear
 * the page down before a commit could be attempted, and this suite is about
 * what happens when a commit IS attempted, which is precisely the window the
 * reload leaves open on the transitions it declines to fire for. It means
 * these tests say nothing about whether that reload works; its own suites own
 * that.
 *
 * The REAL store would not do here even with a session: it bumps only once an
 * identity has been ESTABLISHED and then changed, so a `clear()` on a store
 * nothing was loaded into sets the baseline and measures nothing. That is not
 * a guess — the first version of this suite did exactly that, and the guard in
 * `flipIdentity` below is what said so.
 */
const auth = vi.hoisted(() => {
	// The epoch is read through a late-bound hook so it can be backed by a REAL
	// `$state` signal (installed below, after the imports). `vi.hoisted` runs
	// before the Svelte runtime is initialised, so `$state` cannot be declared
	// in here — but a module-scope signal can be, and the hook lets this object
	// delegate to it.
	//
	// Why it matters (BUG-3084, the double-load unit): the real store's
	// `identityEpoch` is `$state`, so reading it inside an `$effect` creates a
	// DEPENDENCY. A plain closure variable models the store's values correctly
	// and its reactivity not at all, so a test asking "did this effect re-run
	// when the identity moved?" passes against the defect. That is how the
	// double load survived here while being found on the library page.
	const hook = { read: null as null | (() => number), write: null as null | ((n: number) => void) };
	let fallback = 0;
	const getEpoch = () => (hook.read ? hook.read() : fallback);
	const setEpoch = (n: number) => {
		if (hook.write) hook.write(n);
		else fallback = n;
	};
	// The fake dispatches listeners, because the repair's whole point is what a
	// listener does. Order matches the real store's contract: the epoch is
	// bumped BEFORE listeners run, so a load started from one re-stamps to the
	// NEW value — which is exactly what makes recovery possible.
	const listeners: Array<(previousUserId: string) => void> = [];
	return {
		notifyIdentityChange(previousUserId = 'u0') {
			for (const fn of [...listeners]) fn(previousUserId);
		},
		__hook: hook,
		get identityEpoch() { return getEpoch(); },
		get userId() { return 'u1'; },
		get user() { return { id: 'u1', name: 'A', email: 'a@example.com' }; },
		get session() { return { user: { id: 'u1' } }; },
		bumpEpoch() { setEpoch(getEpoch() + 1); },
		resetEpoch() { setEpoch(0); listeners.length = 0; },
		identityFence() {
			const captured = getEpoch();
			return () => getEpoch() === captured;
		},
		onIdentityChange(fn: (previousUserId: string) => void) {
			listeners.push(fn);
			return () => {
				const i = listeners.indexOf(fn);
				if (i >= 0) listeners.splice(i, 1);
			};
		},
		clear() {},
	};
});
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));

vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: {
		loadCollections: vi.fn(async () => {}),
		cachedCollection: vi.fn(async () => null),
		get collections() { return []; },
		get activeItem() { return null; },
	},
}));

import { page } from '$app/state';
import { chatSurface, __resetChatSurfaceForTests, __checkChatSurfaceForTests } from '$lib/stores/chatSurface.svelte';
import CollectionPage from './+page.svelte';

type Props = Record<string, unknown>;
function stubProps(): Props[] {
	return (globalThis as { __stubProps?: Props[] }).__stubProps ?? [];
}
function findPropsObject(): Props | undefined {
	return stubProps().find((p) => typeof p.onStatusChange === 'function');
}
async function waitForBulk(index: number) {
	return waitFor(() => {
		if (deferredBulk.length <= index) throw new Error('no bulk request yet');
		return deferredBulk[index]!;
	});
}
/**
 * The props OBJECT carrying `onSearchChange` — the filter bar's. `findProp`
 * below returns functions only, so a VALUE prop such as `searchQuery` is
 * invisible to it; that is not a quirk to work around silently but the reason
 * this helper exists and says so.
 */
function searchBarProps(): Record<string, unknown> | undefined {
	return stubProps().find((p) => typeof (p as Record<string, unknown>).onSearchChange === 'function') as
		| Record<string, unknown>
		| undefined;
}

function findProp<T>(name: string): T | undefined {
	for (const p of stubProps()) {
		if (typeof p[name] === 'function') return p[name] as T;
	}
	return undefined;
}

/**
 * Move the signed-in identity. `authStore.clear()` bumps `identityEpoch` only
 * once an identity has been ESTABLISHED and then changed — a `clear()` on a
 * store no session was loaded into establishes the baseline and MEASURES
 * NOTHING (#1370 learned this the hard way, and its own guard on the bump is
 * what caught it). So every flip here asserts the epoch actually moved.
 */
// The reactive backing for `auth.identityEpoch`, from the family helper. See
// its header for why a non-reactive double turns every "did this effect
// re-run?" assertion into "does this compile?".
bindReactiveEpoch(auth.__hook);
bindReactiveCounter(reorderState.ver);

function flipIdentity(): void {
	const before = auth.identityEpoch;
	auth.bumpEpoch();
	if (auth.identityEpoch === before) {
		throw new Error(
			'the identity epoch did not move — this test would pass against an unfenced page'
		);
	}
}

const COLLECTION = {
	id: 'c1',
	slug: 'tasks',
	name: 'Tasks',
	schema: JSON.stringify({ fields: [{ key: 'status', type: 'select', options: ['open', 'done'] }] }),
	settings: '{}',
	updated_at: '2026-01-01T00:00:00Z',
};

async function mountPage() {
	(globalThis as { __stubProps?: Props[] }).__stubProps = [];
	toasts.length = 0;
	upserts.length = 0;
	sseHandlers.length = 0;
	searchOpeners.length = 0;
	deferredBulk.length = 0;
	deferredUpdate.length = 0;
	collectionGets.length = 0;
	page.params = { username: 'dave', workspace: 'ws', collection: 'tasks' };
	page.url = new URL('http://localhost/dave/ws/tasks');
	const rendered = render(CollectionPage);
	// Let the load effect run and the collection metadata land, so the views
	// (and therefore the handler props) are rendered.
	const get = await waitFor(() => {
		if (collectionGets.length === 0) throw new Error('no collection fetch yet');
		return collectionGets[0];
	});
	get.resolve({ ...COLLECTION });
	await waitFor(() => {
		if (!findProp('onStatusChange')) throw new Error('views not rendered yet');
	});
	return rendered;
}
/** A filter-bar handler: the bar only renders once the search box is opened. */
async function prop<T>(name: string): Promise<T> {
	if (!findProp(name)) {
		searchOpeners[0]!();
		await tick();
	}
	return waitFor(() => {
		const fn = findProp<T>(name);
		if (!fn) throw new Error(`${name} not rendered yet`);
		return fn;
	});
}

const asShallow = (path: string) =>
	({ url: new URL(`http://localhost${path}`), route: null, params: null }) as never;
const popstate = () => window.dispatchEvent(new PopStateEvent('popstate'));

function openChat() {
	(page as { shallow: unknown }).shallow = asShallow('/messages');
	page.state = { chatDepth: 1 };
}
/** What Kit does when the Close traversal lands on the board. */
function landOnBoard() {
	popstate();
	(page as { shallow: unknown }).shallow = null;
	page.state = {};
	__checkChatSurfaceForTests();
}

describe('board filter, search and view writes under an open Chat', () => {
	beforeEach(() => {
		auth.resetEpoch();
		gotos.length = 0;
		navGuards.length = 0;
		__resetChatSurfaceForTests();
		vi.spyOn(history, 'go').mockImplementation(() => {});
	});
	afterEach(() => {
		cleanup();
		vi.restoreAllMocks();
		(page as { shallow: unknown }).shallow = null;
		page.state = {};
		__resetChatSurfaceForTests();
	});

	it('CONTROL: with Chat closed a filter change writes the URL at once', async () => {
		await mountPage();
		gotos.length = 0;
		(await prop<(f: Record<string, string>) => void>('onFilterChange'))({ status: 'done' });
		expect(gotos).toHaveLength(1);
		expect(gotos[0]).toContain('status');
	});

	it('a filter change under Chat writes nothing; after Close, one write carrying the filter', async () => {
		await mountPage();
		gotos.length = 0;
		openChat();
		(await prop<(f: Record<string, string>) => void>('onFilterChange'))({ status: 'done' });
		expect(gotos).toEqual([]);
		chatSurface.close();
		expect(gotos).toEqual([]);
		landOnBoard();
		expect(gotos).toHaveLength(1);
		expect(gotos[0]).toMatch(/^\/dave\/ws\/tasks\?/);
		expect(gotos[0]).toContain('done');
	});

	it('two changes under Chat write once, the last state', async () => {
		await mountPage();
		gotos.length = 0;
		openChat();
		const onFilter = (await prop<(f: Record<string, string>) => void>('onFilterChange'));
		onFilter({ status: 'done' });
		onFilter({ status: 'open' });
		chatSurface.close();
		landOnBoard();
		expect(gotos).toHaveLength(1);
		expect(gotos[0]).toContain('open');
		expect(gotos[0]).not.toContain('done');
	});

	it('a typed search under Chat: the debounce fires with no write; Close writes ?q once', async () => {
		await mountPage();
		gotos.length = 0;
		openChat();
		(await prop<(q: string) => void>('onSearchChange'))('zy');
		await new Promise((r) => setTimeout(r, 400));
		expect(gotos).toEqual([]);
		chatSurface.close();
		landOnBoard();
		expect(gotos).toHaveLength(1);
		expect(gotos[0]).toContain('q=zy');
	});

	it('a rename heal under Chat supersedes the pending filter write: no goto to the dead slug, the new slug keeps the filter', async () => {
		await mountPage();
		gotos.length = 0;
		openChat();
		(await prop<(f: Record<string, string>) => void>('onFilterChange'))({ status: 'done' });
		sseHandlers[0]!({ type: 'collection_updated', collection_id: 'c1', collection: 'tasks', new_slug: 'jobs' });
		await tick();
		expect(gotos).toEqual([]);
		landOnBoard();
		expect(gotos.filter((g) => g.startsWith('/dave/ws/tasks'))).toEqual([]);
		expect(gotos).toHaveLength(1);
		expect(gotos[0]).toMatch(/^\/dave\/ws\/jobs\?/);
		expect(gotos[0]).toContain('done');
	});

	it('the deferred write does not trip the leave-draft guard: same pathname, no dialog, not cancelled', async () => {
		await mountPage();
		const board = stubProps().filter((p) => 'draftText' in p).at(-1);
		expect(board, 'no view bound draftText, so the draft precondition below is unproven').toBeTruthy();
		// bind:draftText hands the view the page's own state object: writing a key is the user typing a draft.
		((board as Record<string, unknown>).draftText as Record<string, string>)['open'] = 'unsaved words';
		await tick();
		gotos.length = 0;
		openChat();
		(await prop<(f: Record<string, string>) => void>('onFilterChange'))({ status: 'done' });
		chatSurface.close();
		landOnBoard();
		expect(gotos).toHaveLength(1);
		// Kit would now run beforeNavigate with this navigation. The page's own
		// guard must let a same-pathname replace through without cancelling it.
		const cancel = vi.fn();
		const nav = {
			shallow: false,
			willUnload: false,
			type: 'goto',
			from: { url: new URL('http://localhost/dave/ws/tasks') },
			to: { url: new URL(`http://localhost${gotos[0]}`) },
			cancel
		};
		for (const guard of navGuards) guard(nav);
		expect(cancel).not.toHaveBeenCalled();
		// Control: the same guard DOES cancel a real leave with the draft present.
		const leave = { ...nav, to: { url: new URL('http://localhost/dave/ws/other') }, cancel: vi.fn() };
		for (const guard of navGuards) guard(leave);
		expect(leave.cancel, 'the control never cancelled, so the draft was not seen by the guard').toHaveBeenCalled();
	});
});
