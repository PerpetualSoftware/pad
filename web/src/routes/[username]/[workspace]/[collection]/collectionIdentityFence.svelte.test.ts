import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { bindReactiveEpoch, isEpochReactive } from '../../../../test/identityEpochMock.svelte';

/**
 * BUG-3084 surface 1 — the BEHAVIOURAL half. `collectionIdentityFence.test.ts`
 * beside this file owns the POPULATION (all 21 commit points are fenced, and an
 * unrecognised one fails the build); this file owns the SEMANTICS: that a fence
 * actually stops a commit when the signed-in identity moves mid-flight.
 *
 * Neither is redundant. A source guard checks spellings, so it cannot tell a
 * fence comparing the right two values from one comparing the wrong ones, or
 * one placed after the commit it guards. A behavioural suite covers only the
 * handlers someone wrote a case for, so it cannot say a twenty-second arrived
 * unfenced.
 *
 * THE MOVE EACH TEST MAKES: start a handler, hold its request open, bump the
 * identity epoch, then resolve, and assert the commit did not land.
 *
 * ONE CONTROL PER CASE. "Nothing was committed" is satisfied just as well by a
 * handler that never ran, a mock that was never called, or a mount that failed
 * — so every case asserts the same sequence WITHOUT the identity move and sees
 * the commit land. A leg without its control measures nothing. That is not a
 * precaution taken in the abstract: the first version of the SSE case awaited
 * the callback before releasing the request the callback awaits, and the LEG
 * passed while the CONTROL timed out.
 *
 * WHAT THIS FILE DOES NOT COVER, stated rather than implied. It drives FOUR of
 * the twenty-one commit points the source guard enumerates, chosen as one per
 * TIMING CLASS rather than as a sample of handlers:
 *
 *   1. await-then-commit — `handleStatusChange`, via the `onStatusChange` prop;
 *   2. a callback that ARRIVES after the change — the SSE subscription;
 *   3. a commit that OUTLIVES the page — the bulk Undo toast;
 *   4. a DEFERRED timer — the 200ms debounced body search.
 *
 * The other seventeen share a class with one of these, and the source guard is
 * the instrument that speaks about all twenty-one at once. A behavioural case
 * per handler would be seventeen more mounts measuring the same four shapes.
 *
 * Measured against the page as it stands on `origin/main`: all four legs FAIL
 * and all four controls PASS. A suite that cannot be shown to go red for the
 * case it is named for is not evidence.
 */

// ---------------------------------------------------------------------------
// Mocks. The heavy children are stubbed because the subject is this PAGE's
// async handlers: they reach the children as props, so the page's own script
// runs unchanged while BoardView's drag machinery and PaneHost's pane stack
// stay out of the suite.
// ---------------------------------------------------------------------------
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
const upserts = vi.hoisted(() => [] as unknown[]);
const sseHandlers = vi.hoisted(() => [] as Array<(e: unknown) => unknown>);
const gotos = vi.hoisted(() => [] as string[]);
// `goto` MOVES `page.url` here, unlike the shared no-op mock.
//
// Same lesson as the epoch double one level down (codex round 1 [P1]): a mock
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

function defer<T>(sink: Array<{ resolve: (v: unknown) => void; reject: (e: unknown) => void }>): Promise<T> {
	return new Promise<T>((resolve, reject) => {
		sink.push({ resolve: resolve as (v: unknown) => void, reject });
	});
}

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
			update: vi.fn(() => defer(deferredUpdate)),
			create: vi.fn(async () => ({ id: 'i9', slug: 'i9', title: 'x', item_number: 9 })),
			restore: vi.fn(async () => ({ id: 'i1' })),
			bulk: vi.fn(() => defer(deferredBulk)),
			plansProgress: vi.fn(async () => []),
		},
		search: vi.fn(async () => ({ results: [] })),
	},
	PadApiError: class PadApiError extends Error { code = ''; },
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
	isConflictOrNotFound: () => false,
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
		getByCollection: () => [ROW],
		upsert: (_ws: string, row: unknown) => { upserts.push(row); },
		scopeEpochFor: () => 1,
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
	parseSearchQuery: (q: string) => ({
		text: q.replace(/^body:/, ''),
		body: q.startsWith('body:'),
		collection: null,
		archived: false,
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
import { goto } from '$app/navigation';
import { api } from '$lib/api/client';
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

describe('the collection page stops a commit when the identity moves mid-flight', () => {
	it('PRECONDITION: the faked identity epoch is reactive', () => {
		// Without this the suite runs, passes, and measures nothing about
		// re-running — which is how the search-effect defect survived every
		// instrument on this page (BUG-3084 checkpoint 18).
		expect(
			// The MOCK's own getter and bump, not the module signal: the first
			// version of this helper read its own state and returned true with
			// both hooks disconnected, so every leg below passed against the
			// non-reactive double it exists to detect (codex round 1 [P2]).
			isEpochReactive(
				() => auth.identityEpoch,
				() => auth.bumpEpoch()
			),
			'the mocked identityEpoch is not reactive: every "did this effect re-run?" assertion in ' +
				'this file is measuring whether the code compiles'
		).toBe(true);
	});

	beforeEach(() => {
		auth.resetEpoch();
	});
	afterEach(() => {
		cleanup();
		vi.useRealTimers();
	});

	it('CONTROL: a status change commits when the identity holds', async () => {
		await mountPage();
		const onStatusChange = findProp<(i: unknown, v: string) => Promise<void>>('onStatusChange')!;
		const call = onStatusChange({ id: 'i1', slug: 'i1', fields: '{}' }, 'done').catch(() => {});
		const d = await waitFor(() => {
			if (deferredUpdate.length === 0) throw new Error('no update yet');
			return deferredUpdate[0];
		});
		d.resolve({ id: 'i1', slug: 'i1' });
		await call;
		await tick();
		expect(upserts, 'the control did not commit — the leg below would measure nothing').toHaveLength(1);
		expect(toasts.map((t) => t.message)).toContain('Moved to Done');
	});

	it('a status change resolving after an identity move commits nothing', async () => {
		await mountPage();
		const onStatusChange = findProp<(i: unknown, v: string) => Promise<void>>('onStatusChange')!;
		const call = onStatusChange({ id: 'i1', slug: 'i1', fields: '{}' }, 'done').catch(() => {});
		const d = await waitFor(() => {
			if (deferredUpdate.length === 0) throw new Error('no update yet');
			return deferredUpdate[0];
		});
		flipIdentity();
		d.resolve({ id: 'i1', slug: 'i1' });
		await call;
		await tick();
		expect(upserts, 'the previous session\'s row was written into the local index').toHaveLength(0);
		expect(toasts, 'the previous session\'s result was reported to the new user').toHaveLength(0);
	});

	// ── Timing class 2: a callback that ARRIVES after the change ──────────
	//
	// An SSE event is delivered when the server says so. `pageIdentityHeld()`
	// rather than an entry capture is the point: by the time the callback runs
	// the epoch has already moved, so a capture taken at its entry is the NEW
	// value and detects nothing.

	it('CONTROL: an SSE collection_updated refreshes the collection snapshot', async () => {
		await mountPage();
		const before = findPropsObject()!.collection as { name: string };
		expect(before.name).toBe('Tasks');
		// NOT awaited here: the callback cannot settle until the request below
		// is released, so awaiting it first deadlocks. The first version of this
		// suite did exactly that, and the identity LEG passed while the control
		// timed out — a leg that passes because nothing ran.
		const cb = sseHandlers[0]!({ type: 'collection_updated', collection_id: 'c1', collection: 'tasks' });
		const get = await waitFor(() => {
			if (collectionGets.length < 2) throw new Error('no refresh fetch yet');
			return collectionGets[1]!;
		});
		get.resolve({ ...COLLECTION, name: 'Renamed' });
		await cb;
		await waitFor(() => {
			expect((findPropsObject()!.collection as { name: string }).name).toBe('Renamed');
		});
	});

	it('an SSE refresh landing after an identity move does not commit', async () => {
		await mountPage();
		const call = sseHandlers[0]!({ type: 'collection_updated', collection_id: 'c1', collection: 'tasks' });
		const get = await waitFor(() => {
			if (collectionGets.length < 2) throw new Error('no refresh fetch yet');
			return collectionGets[1]!;
		});
		flipIdentity();
		get.resolve({ ...COLLECTION, name: 'Renamed' });
		await call;
		await tick();
		expect(
			(findPropsObject()!.collection as { name: string }).name,
			"the previous session's collection snapshot was written into the page"
		).toBe('Tasks');
	});

	it('an SSE event ARRIVING after an identity move is not acted on at all', () => {
		// The case `pageIdentityHeld()` exists for, and the one an entry capture
		// cannot see: the identity moved BEFORE the event was delivered, so a
		// capture taken at the callback's entry is already the new epoch and
		// every later comparison against it passes. Distinguished from the leg
		// above by WHEN the move happens — there, mid-fetch; here, before the
		// callback runs at all. Mutant M2 of this unit's matrix (deleting the
		// `pageIdentityHeld()` call) SURVIVED until this leg existed, because
		// the leg above was still caught by the entry capture further down.
		//
		// Observed at the REQUEST: the callback must not even ask.
		return (async () => {
			await mountPage();
			const before = collectionGets.length;
			flipIdentity();
			await sseHandlers[0]!({ type: 'collection_updated', collection_id: 'c1', collection: 'tasks' });
			expect(
				collectionGets.length,
				"the page fetched the previous session's collection after the identity had already moved"
			).toBe(before);
		})();
	});

	it('RECOVERS after a sign-in: the page re-loads and the subscriptions resume', () => {
		// THE REPAIR (BUG-3084, codex P1). Without it the fence is a one-way
		// door: `identityEpochAtLoad` is re-stamped only by the page load, that
		// load is driven by a ROUTE-keyed effect, and `routes/+layout.svelte`
		// does not reload the tab on an anonymous -> signed-in transition. So
		// the page keeps a stale epoch and every `pageIdentityHeld()` is false
		// FOR EVER — the SSE and sync callbacks go inert and never come back.
		//
		// This drives the real sequence rather than the epoch alone: bump, THEN
		// notify, which is the order `notifyIdentityChange` uses and the reason
		// a load started from a listener re-stamps to the new value.
		return (async () => {
			await mountPage();

			// Sign in: the epoch moves and listeners run.
			flipIdentity();
			auth.notifyIdentityChange('');

			// The listener re-loads, which is what re-stamps the epoch.
			const reload = await waitFor(() => {
				if (collectionGets.length < 2) throw new Error('the identity change did not re-load the page');
				return collectionGets[1]!;
			});
			// And it CLEARED first: the previous user's collection is not still
			// on screen while the new one is being fetched (codex [P1]). Asserted
			// at this point deliberately — after the listener ran, before the
			// reload resolves, which is the whole window in question.
			expect(
				findPropsObject()?.collection ?? null,
				"the previous user's collection stayed rendered for the length of the re-load"
			).toBeNull();
			reload.resolve({ ...COLLECTION });
			await tick();

			// And the subscription callback works again. Without the repair this
			// fetch never happens: `pageIdentityHeld()` is still comparing
			// against the pre-sign-in epoch and returns at the first line.
			const before = collectionGets.length;
			const cb = sseHandlers[0]!({ type: 'collection_updated', collection_id: 'c1', collection: 'tasks' });
			const refresh = await waitFor(() => {
				if (collectionGets.length <= before) {
					throw new Error('the SSE callback is still inert after the sign-in');
				}
				return collectionGets[collectionGets.length - 1]!;
			});
			refresh.resolve({ ...COLLECTION, name: 'Renamed' });
			await cb;
			await waitFor(() => {
				expect((findPropsObject()!.collection as { name: string }).name).toBe('Renamed');
			});
		})();
	});

	// ── Timing class 3: a commit that OUTLIVES the page ───────────────────
	//
	// The bulk Undo button is handed to the global toast store and the page may
	// be gone by the time it is clicked. This is the worst case on the surface:
	// B clicking A's Undo re-mutates A's items.

	it('CONTROL: the bulk Undo button re-runs the action when the identity holds', async () => {
		await mountPage();
		const archive = findProp<(items: unknown[]) => Promise<unknown>>('onArchiveColumn')!;
		const run = archive([{ id: 'i1', slug: 'i1', fields: '{}' }]).catch(() => {});
		(await waitForBulk(0)).resolve({ updated: [{ id: 'i1' }] });
		await run;
		await tick();
		const undo = toasts.find((t) => t.action)?.action;
		expect(undo, 'no Undo action was offered — the leg below would measure nothing').toBeDefined();
		undo!.onAction();
		await waitFor(() => expect(vi.mocked(api.items.bulk).mock.calls.length).toBe(2));
	});

	it('the bulk Undo button does nothing once the identity has moved', async () => {
		await mountPage();
		const archive = findProp<(items: unknown[]) => Promise<unknown>>('onArchiveColumn')!;
		const run = archive([{ id: 'i1', slug: 'i1', fields: '{}' }]).catch(() => {});
		(await waitForBulk(0)).resolve({ updated: [{ id: 'i1' }] });
		await run;
		await tick();
		const undo = toasts.find((t) => t.action)?.action;
		expect(undo).toBeDefined();
		const callsBefore = vi.mocked(api.items.bulk).mock.calls.length;
		flipIdentity();
		undo!.onAction();
		await tick();
		expect(
			vi.mocked(api.items.bulk).mock.calls.length,
			"the new user's click re-ran the previous user's bulk action"
		).toBe(callsBefore);
	});

	// ── Timing class 4: a DEFERRED timer ──────────────────────────────────
	//
	// The body runs 200ms after it was scheduled, so a check at the scheduling
	// site says nothing about who is signed in when it fires. Observed at the
	// REQUEST rather than at the page state it would write: the fence sits at
	// the top of the timer body, so the fetch itself must not happen.

	it('CONTROL: the debounced body search fires when the identity holds', async () => {
		await mountPage();
		searchOpeners[0]!();
		await tick();
		const onSearchChange = await waitFor(() => {
			const fn = findProp<(q: string) => void>('onSearchChange');
			if (!fn) throw new Error('filter bar not rendered yet');
			return fn;
		});
		// The fake clock goes in BEFORE the timer is scheduled. A `setTimeout`
		// armed while real timers are live is not on the fake clock, so
		// advancing it moves nothing — and the test then passes while measuring
		// the wrong thing (#1370 shipped that mistake once).
		vi.useFakeTimers();
		onSearchChange('body:foo');
		await vi.advanceTimersByTimeAsync(250);
		expect(vi.mocked(api.search).mock.calls.length).toBe(1);
	});

	it('the identity-change listener clears the previous session\'s search text', async () => {
		// DRIVEN, not asserted about (the refusal-vs-absence rule in the shared
		// core's header). The source guard proves `resetPerSessionState()`
		// contains the clear; only this proves the listener reaches it and the
		// page actually forgets what the previous user typed.
		//
		// It matters beyond tidiness: `searchQuery` is synced to the URL, so an
		// inherited query is the previous session's text sitting in the address
		// bar of whoever signs in next.
		await mountPage();
		searchOpeners[0]!();
		await tick();
		const onSearchChange = await waitFor(() => {
			const fn = findProp<(q: string) => void>('onSearchChange');
			if (!fn) throw new Error('filter bar not rendered yet');
			return fn;
		});
		onSearchChange('body:secret');
		await tick();
		expect(
			searchBarProps()?.searchQuery,
			'the query never reached the filter bar — this leg would measure nothing'
		).toBe('body:secret');

		// The query is in the ADDRESS too, which is the half the first version of
		// this leg could not see (codex round 1 [P1]).
		page.url = new URL(`${page.url.origin}${page.url.pathname}?q=body%3Asecret`);
		expect(page.url.searchParams.get('q'), 'precondition: the query is in the URL').toBe('body:secret');

		flipIdentity();
		auth.notifyIdentityChange('u0');
		await tick();
		await tick();

		// Asserted on the ADDRESS, not on a goto COUNT. The count coupled this
		// leg to the page's own URL-sync effect, which the faithful `goto` mock
		// makes reactive — so it could pass or fail for reasons that have
		// nothing to do with the reset. What the fix owes is that `q` is gone.
		expect(
			page.url.searchParams.get('q'),
			"the previous session's query is still in the address bar"
		).toBeNull();

		// AND it must survive the reload completing. `loadUrlFilters()` runs on
		// the reload's success path and restores `q` from the URL — so a leg
		// that stops before the reload lands measures nothing about the defect.
		const reload = await waitFor(() => {
			if (collectionGets.length < 2) throw new Error('the identity change did not re-load');
			return collectionGets[collectionGets.length - 1]!;
		});
		reload.resolve({ id: 'c1', slug: 'tasks', name: 'Tasks', schema: '{}', settings: '{}' });
		await tick();
		await tick();

		expect(
			searchBarProps()?.searchQuery,
			"the reload restored the previous session's query from the URL after the reset cleared it"
		).toBe('');
	});

	it('the reload does not restore the query even when the address rewrite has not landed', async () => {
		// ISOLATES THE LATCH from the URL rewrite. Both mechanisms clear the
		// query, and in this harness `goto` resolves immediately, so the rewrite
		// always wins and the latch is never exercised — a mutant deleting it
		// SURVIVED the whole suite. In a browser the ordering is not guaranteed:
		// the rewrite races the page's own URL-sync effect, which is why the
		// deterministic mechanism exists at all.
		//
		// So this leg suppresses the rewrite and asserts the query still does
		// not come back. Without it, the latch is code no instrument can speak
		// for — which is the thing this whole unit exists to stop shipping.
		await mountPage();
		searchOpeners[0]!();
		await tick();
		const onSearchChange = await waitFor(() => {
			const fn = findProp<(q: string) => void>('onSearchChange');
			if (!fn) throw new Error('filter bar not rendered yet');
			return fn;
		});
		onSearchChange('body:secret');
		await tick();
		page.url = new URL(`${page.url.origin}${page.url.pathname}?q=body%3Asecret`);

		// The rewrite lands nowhere: `goto` records the call and leaves the URL
		// exactly as it is, which is the state a real navigation has not reached.
		vi.mocked(goto).mockImplementationOnce(async () => {});

		flipIdentity();
		auth.notifyIdentityChange('u0');
		await tick();
		await tick();

		expect(
			page.url.searchParams.get('q'),
			'precondition: this leg is about the case where the address still carries the query'
		).toBe('body:secret');

		const reload = await waitFor(() => {
			if (collectionGets.length < 2) throw new Error('the identity change did not re-load');
			return collectionGets[collectionGets.length - 1]!;
		});
		reload.resolve({ id: 'c1', slug: 'tasks', name: 'Tasks', schema: '{}', settings: '{}' });
		await tick();
		await tick();

		expect(
			searchBarProps()?.searchQuery,
			"the reload restored the previous session's query from an address the rewrite had not " +
				'reached yet — the case the latch exists for'
		).toBe('');
	});

	it("a query the NEW user navigates to is honoured, not swallowed by the latch", async () => {
		// The latch suppresses ONE value — the one belonging to the session that
		// ended — not "the next load" (codex round 2 [P2]). A bare flag was
		// consumed by whichever load ran next, including a navigation the new
		// user makes to a different `?q=` link, whose query was then discarded
		// although it was theirs.
		await mountPage();
		searchOpeners[0]!();
		await tick();
		const onSearchChange = await waitFor(() => {
			const fn = findProp<(q: string) => void>('onSearchChange');
			if (!fn) throw new Error('filter bar not rendered yet');
			return fn;
		});
		onSearchChange('body:secret');
		await tick();
		page.url = new URL(`${page.url.origin}${page.url.pathname}?q=body%3Asecret`);

		vi.mocked(goto).mockImplementationOnce(async () => {});
		flipIdentity();
		auth.notifyIdentityChange('u0');
		await tick();

		// The new user goes somewhere with a query of their own BEFORE the
		// identity reload lands.
		page.url = new URL(`${page.url.origin}${page.url.pathname}?q=body%3Amine`);

		const reload = await waitFor(() => {
			if (collectionGets.length < 2) throw new Error('the identity change did not re-load');
			return collectionGets[collectionGets.length - 1]!;
		});
		reload.resolve({ id: 'c1', slug: 'tasks', name: 'Tasks', schema: '{}', settings: '{}' });
		await tick();
		await tick();

		expect(
			searchBarProps()?.searchQuery,
			"the new user's own query was swallowed by a latch meant for the previous session's"
		).toBe('body:mine');
	});

	it('the debounced body search does not fire after an identity move', async () => {
		await mountPage();
		searchOpeners[0]!();
		await tick();
		const onSearchChange = await waitFor(() => {
			const fn = findProp<(q: string) => void>('onSearchChange');
			if (!fn) throw new Error('filter bar not rendered yet');
			return fn;
		});
		vi.useFakeTimers();
		onSearchChange('body:foo');
		// The effect that ARMS the timer runs on the next flush, and it is where
		// the epoch is captured. Flipping before that flush captures the NEW
		// epoch and the timer fires — which is what the first version of this
		// leg measured, and why it failed rather than passing vacuously.
		await vi.advanceTimersByTimeAsync(0);
		flipIdentity();
		await vi.advanceTimersByTimeAsync(250);
		expect(
			vi.mocked(api.search).mock.calls.length,
			"the previous session's query was sent under the new identity"
		).toBe(0);
	});
});
