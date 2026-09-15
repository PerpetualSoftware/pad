import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import {
	bindReactiveEpoch,
	isEpochReactive,
	bindReactiveUserId,
	isUserIdReactive,
} from '../../../test/identityEpochMock.svelte';

/**
 * BUG-3084 surface 4 — the BEHAVIOURAL half. `dashboardIdentityFence.test.ts`
 * beside this file owns the POPULATION; this owns the SEMANTICS.
 *
 * THE MOVE EACH TEST MAKES: start a load, hold its request open, move the
 * identity, then resolve, and assert the commit did not land.
 *
 * ONE CONTROL PER CASE, because "nothing was committed" is satisfied just as
 * well by a load that never ran or a mount that failed.
 *
 * TWO KINDS OF IDENTITY MOVE, and the difference is the whole page:
 *   - `flipIdentity()` bumps the EPOCH only. The real store bumps the epoch and
 *     changes `userId` in one synchronous step, and the page's keyed effect
 *     reacts to `userId` at the NEXT flush — so between the bump and the flush
 *     there is a window in which a queued continuation commits under the old
 *     identity. An epoch-only flip holds the page IN that window: the effect
 *     does not run, and the fence is the only thing standing.
 *   - `signInAs(id)` moves both and notifies, which is what the store does. It
 *     drives the RECOVERY legs: the page must drop the board, reload for the
 *     new user, and keep polling afterwards — the leg every instrument set in
 *     this family was missing until #1374.
 *
 * Every waiter here spins MICROTASKS, never timers: the 30 s poll is driven
 * with fake timers, and a timer-based `waitFor` under fake timers waits for
 * ever.
 */

const auth = vi.hoisted(() => {
	const hook = {
		read: null as null | (() => number),
		write: null as null | ((n: number) => void),
		// The user id is a SIGNAL too (bound below): the page's recovery is a
		// `(sessionUserId, wsSlug)`-keyed effect, and a plain variable here
		// would leave it never re-running — the recovery legs would then fail
		// against a correct page, or worse, be weakened until they passed.
		readUser: null as null | (() => string),
		writeUser: null as null | ((id: string) => void),
	};
	let fallback = 0;
	let userFallback = 'u1';
	const getEpoch = () => (hook.read ? hook.read() : fallback);
	const getUser = () => (hook.readUser ? hook.readUser() : userFallback);
	const setUser = (id: string) => {
		if (hook.writeUser) hook.writeUser(id);
		else userFallback = id;
	};
	const setEpoch = (n: number) => {
		if (hook.write) hook.write(n);
		else fallback = n;
	};
	return {
		__hook: hook,
		get identityEpoch() { return getEpoch(); },
		get userId() { return getUser(); },
		setUserId(id: string) { setUser(id); },
		get user() { return { id: getUser(), name: 'A', email: 'a@example.com' }; },
		get mcpPublicUrl() { return ''; },
		bumpEpoch() { setEpoch(getEpoch() + 1); },
		resetEpoch() { setEpoch(0); },
		identityFence() { const c = getEpoch(); return () => getEpoch() === c; },
		listeners: [] as Array<() => void>,
		onIdentityChange(cb: () => void) {
			this.listeners.push(cb);
			return () => { this.listeners = this.listeners.filter((l) => l !== cb); };
		},
		notify() { for (const l of [...this.listeners]) l(); },
		clear() {},
	};
});
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));

type Held = { slug: string; resolve: (v: unknown) => void; reject: (e: unknown) => void };
const dashboardCalls = vi.hoisted(() => [] as Held[]);
const setCurrentCalls = vi.hoisted(() => [] as Array<() => void>);
const holdSetCurrent = vi.hoisted(() => ({ on: false }));
const syncCallbacks = vi.hoisted(() => [] as Array<(r: unknown) => void>);

vi.mock('$lib/api/client', () => ({
	api: {
		dashboard: {
			get: vi.fn((slug: string) => new Promise((resolve, reject) => {
				dashboardCalls.push({ slug, resolve: resolve as (v: unknown) => void, reject });
			})),
		},
		collections: { list: vi.fn(async () => []) },
		workspaces: { claimCode: vi.fn(async () => ({ code: 'x', expires_at: '' })) },
	},
	PadApiError: class PadApiError extends Error {
		code: string;
		constructor(code: string) { super(code); this.code = code; }
	},
}));

vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: {
		setCurrent: vi.fn(() => {
			if (!holdSetCurrent.on) return Promise.resolve();
			return new Promise<void>((resolve) => { setCurrentCalls.push(resolve); });
		}),
		get current() { return { id: 'w1', slug: 'ws', name: 'WS' }; },
		get isOwner() { return false; },
		get membershipKnown() { return true; },
		get currentRole() { return 'owner'; },
		get workspaces() { return []; },
		loadAll: vi.fn(async () => {}),
	},
}));

vi.mock('$lib/services/sync.svelte', () => ({
	syncService: {
		onSync: (cb: (r: unknown) => void) => { syncCallbacks.push(cb); return () => {}; },
		start: () => {},
		stop: () => {},
	},
}));

vi.mock('$lib/stores/ui.svelte', () => ({
	uiStore: {
		get connectAfterNavigateSlug() { return null; },
		consumeConnectAfterNavigate() {},
		requestQuickAdd() {},
	},
}));
vi.mock('$lib/stores/collections.svelte', () => ({ collectionStore: { loadCollections: vi.fn() } }));
vi.mock('$lib/stores/title.svelte', () => ({ titleStore: { setPageTitle() {} } }));
vi.mock('$lib/scroll/restore.svelte', () => ({
	createScrollRestoration: () => ({ snapshot: { capture: () => null, restore: () => {} } }),
}));

import { api, PadApiError } from '$lib/api/client';
import { page } from '$app/state';
import DashboardPage from './+page.svelte';

/**
 * A complete DashboardResponse. Every array is present because the page reads
 * `.length` on them unconditionally; a payload missing one throws during
 * render and leaves the page blank, which reads exactly like "nothing was
 * committed" and would make every refusal leg pass vacuously.
 */
function board(title: string, over: { needs_onboarding?: boolean } = {}) {
	return {
		summary: { total_items: 1, by_collection: {} },
		active_items: [{
			slug: title.toLowerCase().replace(/\s+/g, '-'),
			title,
			collection_slug: 'tasks',
			status: 'open',
			priority: 'medium',
			updated_at: new Date().toISOString(),
		}],
		starred_items: [],
		active_plans: [],
		attention: [],
		recent_activity: [],
		suggested_next: [],
		has_agent_activity: false,
		needs_onboarding: over.needs_onboarding ?? false,
		degraded: false,
		degraded_sections: [],
	};
}

/** Spin microtasks + Svelte flushes until `until` holds. Never a timer. */
async function spin(until: () => boolean, what: string, rounds = 60): Promise<void> {
	for (let i = 0; i < rounds; i++) {
		if (until()) return;
		await Promise.resolve();
		await tick();
	}
	if (!until()) throw new Error(what);
}

async function nextDashboardCall(from: number): Promise<Held> {
	await spin(() => dashboardCalls.length > from, `no dashboard fetch beyond #${from}`);
	return dashboardCalls[from]!;
}

async function mountPage(
	first = board('Live board'),
	ready: () => boolean = () => document.body.textContent?.includes(first.active_items[0]!.title) ?? false
) {
	page.params = { username: 'dave', workspace: 'ws' };
	page.url = new URL('http://localhost/dave/ws');
	const r = render(DashboardPage);
	const call = await nextDashboardCall(0);
	call.resolve(first);
	await spin(ready, 'the first board never rendered — the legs below would measure nothing');
	return r;
}

bindReactiveEpoch(auth.__hook);
bindReactiveUserId(auth.__hook);

function flipIdentity(): void {
	const before = auth.identityEpoch;
	auth.bumpEpoch();
	if (auth.identityEpoch === before) {
		throw new Error('the identity epoch did not move — this test would pass against an unfenced page');
	}
}

/** What the real store does: user changes, epoch bumps, listeners run. */
function signInAs(id: string): void {
	auth.setUserId(id);
	flipIdentity();
	auth.notify();
}

function triggerSync(): void {
	expect(syncCallbacks.length, 'the page did not subscribe to sync').toBeGreaterThan(0);
	for (const cb of syncCallbacks) cb({ workspace: 'ws', type: 'changed' });
}

const shown = (text: string) => document.body.textContent?.includes(text) ?? false;

describe('the dashboard stops a commit when the identity moves mid-flight', () => {
	it('PRECONDITION: the faked identity epoch is reactive', () => {
		expect(
			isEpochReactive(() => auth.identityEpoch, () => auth.bumpEpoch()),
			'the mocked identityEpoch is not reactive: every "did this effect re-run?" assertion in ' +
				'this file is measuring whether the code compiles'
		).toBe(true);
	});

	it('PRECONDITION: the faked user id is reactive', () => {
		// The page's RECOVERY is keyed on `authStore.userId`, not on the epoch.
		// Without this, the recovery legs below would be asking whether a
		// plain variable can re-run an effect — it cannot — and the first
		// version of this suite did exactly that: the reload never came.
		expect(
			isUserIdReactive(() => auth.userId, (id) => auth.setUserId(id)),
			'the mocked userId is not reactive: the keyed load effect can never re-run, so every ' +
				'recovery assertion in this file is measuring whether the code compiles'
		).toBe(true);
	});

	beforeEach(() => {
		auth.resetEpoch();
		auth.setUserId('u1');
		auth.listeners.length = 0;
		dashboardCalls.length = 0;
		setCurrentCalls.length = 0;
		syncCallbacks.length = 0;
		holdSetCurrent.on = false;
		vi.mocked(api.dashboard.get).mockClear();
	});
	afterEach(() => { cleanup(); vi.useRealTimers(); });

	it('CONTROL: a load under an unchanged identity paints the board', async () => {
		await mountPage();
		expect(shown('Live board')).toBe(true);
		expect(document.querySelector('.skeleton-dashboard')).toBeNull();
	});

	it('a LOAD in flight when the identity moves paints none of the previous data', async () => {
		// THE GAP: epoch bumped, keyed effect not yet flushed. The board on
		// screen is still the previous user's (that is the effect's job, and it
		// has not run); what must NOT happen is a NEW commit under the old
		// identity.
		await mountPage();
		triggerSync();
		const reload = await nextDashboardCall(1);
		flipIdentity();
		reload.resolve(board('Stale board'));
		await tick();
		await tick();
		expect(shown('Stale board'), "a load that resolved after the identity moved painted the previous session's board").toBe(false);
		// The premise of this leg, checked rather than assumed: an epoch-only
		// flip must NOT have re-run the keyed effect. If a third fetch was
		// issued, the page is not in the gap this leg models and the refusal
		// above could be the reload's sequence token rather than the fence.
		expect(dashboardCalls.length, 'an epoch-only flip re-ran the keyed load effect — the leg is not modelling the gap').toBe(2);
	});

	it('CONTROL: a sync-triggered reload under an unchanged identity paints', async () => {
		await mountPage();
		triggerSync();
		const reload = await nextDashboardCall(1);
		reload.resolve(board('Synced board'));
		await spin(() => shown('Synced board'), 'the sync reload never painted — the refusal leg above measures nothing');
	});

	it('a load that has lost its identity between its awaits issues no fetch', async () => {
		// The check between `await setCurrent` and the board fetch. Held on the
		// FIRST await; the identity moves; the continuation must not go on to
		// request the board on the previous user's behalf.
		holdSetCurrent.on = true;
		page.params = { username: 'dave', workspace: 'ws' };
		page.url = new URL('http://localhost/dave/ws');
		render(DashboardPage);
		await spin(() => setCurrentCalls.length > 0, 'load() never awaited setCurrent');
		flipIdentity();
		setCurrentCalls[0]!();
		await tick();
		await tick();
		await tick();
		expect(dashboardCalls.length, 'the board was fetched on behalf of an identity that had already moved').toBe(0);
	});

	it('CONTROL: with the identity unchanged the fetch follows setCurrent', async () => {
		holdSetCurrent.on = true;
		page.params = { username: 'dave', workspace: 'ws' };
		page.url = new URL('http://localhost/dave/ws');
		render(DashboardPage);
		await spin(() => setCurrentCalls.length > 0, 'load() never awaited setCurrent');
		setCurrentCalls[0]!();
		await spin(() => dashboardCalls.length > 0, 'the board fetch never followed setCurrent — the leg above measures nothing');
	});

	it("a FAILED load after the identity moved shows no Retry state to the new user", async () => {
		page.params = { username: 'dave', workspace: 'ws' };
		page.url = new URL('http://localhost/dave/ws');
		render(DashboardPage);
		const first = await nextDashboardCall(0);
		flipIdentity();
		first.reject(new Error('boom'));
		await tick();
		await tick();
		await tick();
		expect(document.querySelector('.dash-error'), "the previous session's load failure was shown to whoever is signed in now").toBeNull();
	});

	it('CONTROL: a failed load under an unchanged identity shows the Retry state', async () => {
		page.params = { username: 'dave', workspace: 'ws' };
		page.url = new URL('http://localhost/dave/ws');
		render(DashboardPage);
		const first = await nextDashboardCall(0);
		first.reject(new Error('boom'));
		await spin(() => document.querySelector('.dash-error') !== null, 'the Retry state never rendered — the refusal leg above measures nothing');
	});

	it("a stale not_found does not empty the board the new user is looking at", async () => {
		await mountPage();
		triggerSync();
		const reload = await nextDashboardCall(1);
		flipIdentity();
		reload.reject(new PadApiError('not_found'));
		await tick();
		await tick();
		await tick();
		expect(shown('Live board'), "a not_found that resolved after the identity moved emptied the board").toBe(true);
	});

	it('CONTROL: a not_found under an unchanged identity empties the board', async () => {
		await mountPage();
		triggerSync();
		const reload = await nextDashboardCall(1);
		reload.reject(new PadApiError('not_found'));
		await spin(() => !shown('Live board'), 'the not_found never emptied the board — the leg above measures nothing');
		expect(shown('No dashboard data available.')).toBe(true);
	});

	it('the 30 s POLL: a poll in flight when the identity moves paints nothing', async () => {
		// The deferred member. Fake timers are installed BEFORE mount because
		// the interval is armed in onMount.
		vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'setTimeout', 'clearTimeout'] });
		await mountPage();
		vi.advanceTimersByTime(30_000);
		const poll = await nextDashboardCall(1);
		flipIdentity();
		poll.resolve(board('Stale board'));
		await tick();
		await tick();
		expect(shown('Stale board'), "a poll that resolved after the identity moved painted the previous session's board").toBe(false);
	});

	it('CONTROL: a poll under an unchanged identity paints', async () => {
		vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'setTimeout', 'clearTimeout'] });
		await mountPage();
		vi.advanceTimersByTime(30_000);
		const poll = await nextDashboardCall(1);
		poll.resolve(board('Polled board'));
		await spin(() => shown('Polled board'), 'the poll never painted — the refusal leg above measures nothing');
	});

	it('recovers after an identity change: drops the board, reloads for the NEW user, and keeps polling', async () => {
		// "Did it ever resume?" — the opposite of every leg above. A fence that
		// latches shut and never lifts is an outage with good intentions.
		vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'setTimeout', 'clearTimeout'] });
		await mountPage();

		signInAs('u2');
		const reload = await nextDashboardCall(1);
		expect(shown('Live board'), "the previous user's board is still on screen after the identity changed").toBe(false);
		reload.resolve(board('New board'));
		await spin(() => shown('New board'), 'the board never came back after the identity change — the fence latched shut');

		// AND THE PAGE KEEPS WORKING: the poll still issues loads under the new
		// identity, and they still paint.
		vi.advanceTimersByTime(30_000);
		const poll = await nextDashboardCall(2);
		poll.resolve(board('Polled again'));
		await spin(() => shown('Polled again'), 'the poll stopped painting after the identity change');
	});

	it("Retry's load, overtaken by an identity change, does not paint over the new user's board", async () => {
		// Codex round 2's probe, kept as a driven leg: Retry issues a load, the
		// identity moves and the keyed effect reloads for the new user, and THEN
		// the Retry load resolves with the previous user's board. Retry
		// delegates to load(), so both the fence and the sequence token refuse
		// it; the source guard holds Retry to delegating and nothing else.
		page.params = { username: 'dave', workspace: 'ws' };
		page.url = new URL('http://localhost/dave/ws');
		render(DashboardPage);
		const first = await nextDashboardCall(0);
		first.reject(new Error('boom'));
		await spin(() => document.querySelector('.dash-error') !== null, 'CONTROL: the Retry state never rendered');
		const retry = [...document.querySelectorAll('button')].find((b) => b.textContent?.trim() === 'Retry');
		expect(retry, 'no Retry button — nothing to click').toBeDefined();
		retry!.click();
		const retried = await nextDashboardCall(1);

		signInAs('u2');
		const reload = await nextDashboardCall(2);
		reload.resolve(board('New board'));
		await spin(() => shown('New board'), 'the reload for the new user never painted');

		retried.resolve(board('Stale board'));
		await tick();
		await tick();
		expect(shown('Stale board'), "Retry's load painted the previous user's board over the new one").toBe(false);
		expect(shown('New board')).toBe(true);
	});

	it('the identity change closes a modal the previous session opened', async () => {
		// The Connect modal mints a claim code for the signed-in user; one left
		// open across a sign-in hands the previous user's code to whoever is
		// signed in now. The launchpad's Connect button is the way in.
		if (typeof HTMLDialogElement.prototype.showModal !== 'function') {
			HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', ''); };
			HTMLDialogElement.prototype.close = function () { this.removeAttribute('open'); };
		}
		await mountPage(
			board('Setup board', { needs_onboarding: true }),
			() => document.querySelector('.lp-connect-btn') !== null
		);
		const connect = document.querySelector('.lp-connect-btn') as HTMLButtonElement | null;
		expect(connect, 'the launchpad did not render its Connect button — nothing to open').not.toBeNull();
		connect!.click();
		const dialog = () => document.querySelector('dialog[aria-labelledby="connect-ws-title"]') as HTMLDialogElement | null;
		await spin(() => dialog()?.open === true, 'CONTROL: the Connect modal never opened — the assertion below measures nothing');

		signInAs('u2');
		await tick();
		await tick();
		expect(dialog()?.open, "the previous session's Connect modal is still open for whoever is signed in now").toBe(false);
	});
});
