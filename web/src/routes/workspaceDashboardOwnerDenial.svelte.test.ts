import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, unmount, tick } from 'svelte';
import { page } from '$app/state';
import { workspaceStore } from '$lib/stores/workspace.svelte';
import { authStore } from '$lib/stores/auth.svelte';

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

const sessionGet = vi.hoisted(() => vi.fn<() => Promise<unknown>>());

vi.mock('$lib/api/client', () => ({
	api: {
		auth: { session: () => sessionGet() },
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
function dashboard(over: { needs_onboarding?: boolean; active_items?: unknown[] } = {}) {
	return {
		summary: { total_items: 0, by_collection: {} },
		active_items: over.active_items ?? [],
		starred_items: [],
		active_plans: [],
		attention: [],
		recent_activity: [],
		suggested_next: [],
		has_agent_activity: false,
		needs_onboarding: over.needs_onboarding ?? false,
		degraded: false,
		degraded_sections: []
	};
}

const ITEM = {
	slug: 'a-thing',
	title: 'A thing',
	collection_slug: 'tasks',
	status: 'open',
	priority: 'medium',
	updated_at: new Date().toISOString(),
};

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
	sessionGet.mockReset();
	sessionGet.mockResolvedValue({
		authenticated: true,
		user: { id: 'u1', email: 'u1@example.com' },
	});
});

afterEach(() => {
	if (app) unmount(app as never);
	app = null;
	host.remove();
	// The auth store is a module singleton too, and an identity left over from
	// one test would silently change the reset key the next one starts from.
	authStore.clear();
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

	it('drops the card the moment a DIFFERENT user signs in, before their /me lands', async () => {
		// BUG-2991, codex round 2 — the hole `membershipKnown` alone leaves open.
		//
		// An identity change drops membership to UNKNOWN, and unknown
		// deliberately does not update this cache; that is what the gate is
		// for. The slug did not change either, so the reset effect keyed on
		// `wsSlug` alone never fired. Net: the previous owner's `true` survived
		// a sign-in as somebody else on the same route, and the new user saw
		// owner-only chrome until their own `/me` landed. The cache is keyed on
		// (user, workspace) now.
		//
		// This is the UNKNOWN window, not a settled denial — the other test
		// covers the settled case, and neither subsumes the other.
		await authStore.load();
		await mountAsOwner();

		sessionGet.mockResolvedValue({
			authenticated: true,
			user: { id: 'u2', email: 'u2@example.com' },
		});
		await authStore.load();
		await settle();

		// Membership really is unknown here — no answer has arrived for u2. If
		// this read `true` the assertion below would be about a settled state
		// and would prove something else.
		expect(workspaceStore.membershipKnown).toBe(false);
		expect(ownerCard()).toBeNull();
	});

	it('stops showing the previous user\'s board the moment a different user signs in', async () => {
		// BUG-2991, codex round 3 — the half the store's reset could not reach.
		//
		// The store drops `current`, `workspaces` and membership on an identity
		// change, but this page's `dashboard` and `collections` are LOCAL state
		// and survived it, and its load effect is `untrack`ed and keyed on the
		// route slug — which does not change when someone else signs in on the
		// same route. So user B sat looking at A's items, counts and activity
		// while the store already said "unknown". Nothing about server
		// authorization was involved: the bytes were already in the tab.
		await authStore.load();
		await mountAsOwner();

		// `/me` is the observable for "a load was issued", NOT `dashboard.get`:
		// `load()` awaits `workspaceStore.setCurrent` first, and that awaits the
		// membership request, which this suite holds open. Asserting on
		// `dashboard.get` measured a call that cannot have happened yet, and it
		// failed for that reason rather than for the product's.
		const meBefore = meCalls.length;

		sessionGet.mockResolvedValue({
			authenticated: true,
			user: { id: 'u2', email: 'u2@example.com' },
		});
		await authStore.load();
		await settle();

		// A's board is off screen...
		expect(host.querySelector('.coll-grid')).toBeNull();
		// ...and a fresh load was issued for B rather than the page simply
		// going blank forever.
		expect(meCalls.length).toBeGreaterThan(meBefore);
	});

	it('does not bring the previous user\'s board back when the new user\'s load FAILS', async () => {
		// BUG-2991, codex round 5. Hiding A's board behind `loading` was a
		// fail-open: `loading` goes false when the new user's request settles
		// EITHER WAY, and on a failure `dashboard` still held A's data with the
		// template rendering it ahead of the `dashError` branch. So A's items,
		// counts and activity came back on a 500 or a 403 — the worst moment
		// for them to. The data is dropped at the transition now.
		await authStore.load();
		await mountAsOwner();
		expect(host.querySelector('.coll-grid')).not.toBeNull();

		// B signs in, and everything B asks for fails.
		dashboardGet.mockRejectedValue(new Error('500'));
		sessionGet.mockResolvedValue({
			authenticated: true,
			user: { id: 'u2', email: 'u2@example.com' },
		});
		await authStore.load();
		await settle();

		// Answer B's membership request so the load gets past `setCurrent` and
		// reaches the failing dashboard fetch, then settles.
		const i = await nextMe(meCalls.length - 1 < 0 ? 0 : meCalls.length - 1);
		meCalls[i]!(OWNER);
		await settle();

		// NON-VACUITY (codex round 6). "The grid is absent" is also true of a
		// page still sitting in its loading branch, which would prove nothing
		// about whether the old board was restored. Pin the state precisely:
		// the load has SETTLED into its error branch, with the Retry affordance
		// up and the previous user's board gone.
		expect(host.querySelector('.dash-error')).not.toBeNull();
		expect(host.textContent).toContain('Retry');
		expect(host.querySelector('.coll-grid')).toBeNull();
	});

	it('does not highlight the new user\'s items as if they had just created them', async () => {
		// codex round 9. The aha-highlight track is keyed on `dashboardSlug`,
		// which does NOT change on a same-route identity change, so the previous
		// user's `needs_onboarding: true` was still standing when the new user's
		// first response arrived with `false`. That reads as the true→false
		// edge the highlight exists for, and the NEW user's existing items were
		// badged as though they had just created them.
		await authStore.load();
		dashboardGet.mockResolvedValue(dashboard({ needs_onboarding: true }));
		app = mount(DashboardPage, { target: host, props: {} }) as Record<string, unknown>;
		flushSync();
		const i0 = await nextMe(0);
		meCalls[i0]!(OWNER);
		await settle();

		// u2 signs in and their board is a normal, already-onboarded one.
		dashboardGet.mockResolvedValue(
			dashboard({ needs_onboarding: false, active_items: [ITEM] }),
		);
		sessionGet.mockResolvedValue({
			authenticated: true,
			user: { id: 'u2', email: 'u2@example.com' },
		});
		await authStore.load();
		await settle();
		const i1 = await nextMe(meCalls.length - 1);
		meCalls[i1]!(OWNER);
		await settle();

		// PRECONDITION: u2's item really did render, or "no highlight" is true
		// of an empty page and proves nothing.
		expect(host.querySelector('.active-card')).not.toBeNull();
		expect(host.querySelector('.just-created')).toBeNull();
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
