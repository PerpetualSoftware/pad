import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import { page } from '$app/state';
import { workspaceStore } from '$lib/stores/workspace.svelte';

/**
 * BUG-2990 — the dashboard's sticky owner cache gated on
 * `currentMembership !== null`, and a DENIAL is delivered as null.
 *
 * So once the cache held `true` it never cleared: an owner removed from the
 * workspace, or a `/me` that 403s, kept the owner-only "New Collection" card on
 * screen for the life of the page. BUG-2978 had already fixed the settings page
 * this way (`membershipKnown`, which is true for an answer of EITHER sign); the
 * dashboard predates it and was never brought across.
 *
 * Asserted through the PAGE (CONVE-19). The store's own suites cover
 * `membershipKnown`; what is untested without this file is the BINDING — that
 * this page's cache reads the flag rather than the nullness, which is invisible
 * to every test of the store.
 *
 * Bound: UI only. `handlers_collections.go:48` is the enforcement boundary and
 * the CTA 403s on click either way. The defect is that the UI lies, and that
 * the lie does not expire.
 */

const meCalls: Array<(value: unknown) => void> = [];
const meRejects: Array<(reason: unknown) => void> = [];

const dashboardGet = vi.hoisted(() => vi.fn<(slug: string) => Promise<unknown>>());

vi.mock('$lib/services/sync.svelte', () => ({
	syncService: { onSync: () => () => {}, start: () => {}, stop: () => {} }
}));

vi.mock('$lib/api/client', () => ({
	api: {
		dashboard: { get: (slug: string) => dashboardGet(slug) },
		collections: { list: vi.fn().mockResolvedValue([]) },
		workspaces: {
			get: vi.fn().mockResolvedValue({ id: 'w1', slug: 'ws', name: 'WS' }),
			me: vi.fn(() => new Promise((resolve, reject) => {
				meCalls.push(resolve);
				meRejects.push(reject);
			})),
			list: vi.fn().mockResolvedValue([])
		}
	},
	PadApiError: class extends Error {}
}));

const { default: DashboardPage } = await import('./[username]/[workspace]/+page.svelte');

const OWNER = { role: 'owner', collection_grants: [], item_grants: [] };

/**
 * A complete DashboardResponse. Every array is present because the page reads
 * `.length` on them unconditionally in its section guards — a payload missing
 * one throws during render and leaves the page blank, which would read exactly
 * like "the owner card did not render" and make the denial leg pass vacuously.
 */
function dashboard() {
	return {
		summary: { total_items: 0, by_collection: {} },
		active_items: [],
		starred_items: [],
		active_plans: [],
		attention: [],
		recent_activity: [],
		suggested_next: [],
		has_agent_activity: false,
		needs_onboarding: false,
		degraded: false,
		degraded_sections: []
	};
}

let host: HTMLElement;
let app: Record<string, unknown> | null = null;

/** Let mount effects, the dashboard fetch and the store's settles all land. */
async function settle(): Promise<void> {
	for (let i = 0; i < 6; i++) {
		await Promise.resolve();
		await tick();
	}
	flushSync();
}

/**
 * Spin the microtask/effect loop until a `/me` beyond `from` has been ISSUED,
 * and return its index.
 *
 * Indices are captured rather than assumed: mounting the page issues a `/me` of
 * its own before any test touches the store, so `meCalls[0]` is NOT the call a
 * test just started. Resolving by a hardcoded index resolves someone else's
 * promise and the test hangs on its own — which is how this helper came to
 * exist.
 *
 * Deliberately not `vi.waitFor`: its polling is timer-based, and everything
 * this suite waits on is a microtask plus a Svelte flush.
 */
async function nextMe(from: number): Promise<number> {
	for (let i = 0; i < 50 && meCalls.length <= from; i++) {
		await Promise.resolve();
		await tick();
	}
	expect(meCalls.length).toBeGreaterThan(from);
	return from;
}

/** The owner-only "New Collection" card in the Collections grid. */
function ownerCard(): Element | null {
	return host.querySelector('.coll-card-new');
}

beforeEach(async () => {
	meCalls.length = 0;
	meRejects.length = 0;
	host = document.createElement('div');
	document.body.appendChild(host);
	page.params.workspace = 'ws';
	page.params.username = 'alice';
	dashboardGet.mockReset();
	dashboardGet.mockResolvedValue(dashboard());
});

afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	host.remove();
	// The store is a module-scoped singleton shared with every other suite in
	// this file's worker. Deliberately NOT `vi.resetModules()` — see the note in
	// settingsPermissionFlicker: a fresh module graph brings a second copy of
	// the Svelte runtime and the remount dies with `effect_orphan`.
});

describe('BUG-2990: dashboard owner chrome expires on a definitive denial', () => {
	/**
	 * Mount the page and answer the `/me` its own `load()` issues.
	 *
	 * `load()` AWAITS `workspaceStore.setCurrent(slug)` before fetching the
	 * dashboard, so leaving that first `/me` open leaves `dashboard` null and
	 * the Collections grid unrendered — a state in which "the owner card is
	 * absent" is true for a reason that has nothing to do with permissions.
	 */
	async function mountAsOwner(): Promise<void> {
		app = mount(DashboardPage, { target: host, props: {} }) as Record<string, unknown>;
		flushSync();
		const i = await nextMe(0);
		meCalls[i]!(OWNER);
		await settle();
		// The grid is the precondition for every assertion below: without it the
		// card cannot render for reasons unrelated to this bug.
		expect(host.querySelector('.coll-grid')).not.toBeNull();
		expect(ownerCard()).not.toBeNull();
	}

	it('drops the owner-only New Collection card when membership becomes a denial', async () => {
		await mountAsOwner();

		// The answer CHANGES to "no access", delivered the way the store
		// delivers it — a rejected `/me`, which settles membership to null.
		// Same user and slug, so TASK-2988 replays the previous OWNER answer
		// while the refetch runs: the card correctly stays up through the
		// window, and it is the SETTLE that must take it down.
		const at = meCalls.length;
		const second = workspaceStore.setCurrent('ws');
		const i = await nextMe(at);
		meRejects[i]!(new Error('403'));
		await second;
		await settle();

		// A denial is an ANSWER: known, and null.
		expect(workspaceStore.membershipKnown).toBe(true);
		expect(workspaceStore.currentMembership).toBeNull();
		// The assertion this file exists for. Gated on `currentMembership !==
		// null` the cache still reads true here and the card is still on screen.
		expect(ownerCard()).toBeNull();
	});

	it('keeps the card up across the window a refetch opens', async () => {
		// The complement, and the reason the sticky cache exists at all: the
		// fix must not turn "unknown" into "not an owner".
		await mountAsOwner();

		// A refetch begins and does NOT resolve. The card must stay.
		const at = meCalls.length;
		const second = workspaceStore.setCurrent('ws');
		const i = await nextMe(at);
		await settle();
		expect(ownerCard()).not.toBeNull();

		meCalls[i]!(OWNER);
		await second;
		await settle();
		expect(ownerCard()).not.toBeNull();
	});
});
