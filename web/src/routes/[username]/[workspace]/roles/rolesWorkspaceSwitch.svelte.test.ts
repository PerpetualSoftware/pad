import { describe, expect, it, vi, afterEach } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';

/**
 * BUG-3701: a param-only navigation /A/roles -> /B/roles keeps this page
 * mounted (SvelteKit reuses the component), and the board used to load only
 * on mount, so A's lanes stayed under B's heading. The page's data belongs to
 * a workspace as well as an identity.
 *
 * REACTIVE `page`, unlike the identity suites beside this file: the defect is
 * an effect that never re-ran on the route parameter, and a plain-object page
 * mock cannot make any effect re-run.
 */
vi.mock('$app/state', async () => ({ page: (await import('../../../../test/mocks/reactivePage.svelte')).page }));
const toasts = vi.hoisted(() => [] as Array<{ message: string }>);
vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: {
		show: (message: string) => { toasts.push({ message }); return 'id'; },
		dismiss: () => {},
		get toasts() { return []; },
	},
	quietExternalToasts: () => false,
}));

const auth = vi.hoisted(() => {
	// Epoch read through a late-bound hook so it can be backed by the family's
	// REAL `$state` signal (bound below). See `identityEpochMock.svelte.ts`:
	// a non-reactive double turns every "did this effect re-run?" assertion
	// into "does this compile?", which is how a defect survived every
	// instrument on the collection page (BUG-3084 checkpoint 18).
	const hook = { read: null as null | (() => number), write: null as null | ((n: number) => void) };
	let fallback = 0;
	const getEpoch = () => (hook.read ? hook.read() : fallback);
	const setEpoch = (n: number) => {
		if (hook.write) hook.write(n);
		else fallback = n;
	};
	return {
		__hook: hook,
		get identityEpoch() { return getEpoch(); },
		get userId() { return 'u1'; },
		get user() { return { id: 'u1', name: 'A', email: 'a@example.com' }; },
		get session() { return { user: { id: 'u1' } }; },
		bumpEpoch() { setEpoch(getEpoch() + 1); },
		resetEpoch() { setEpoch(0); },
		identityFence() { const c = getEpoch(); return () => getEpoch() === c; },
		// A REAL registry, not a no-op (BUG-3084 checkpoint 13). The reload leg
		// below has to drive the page's own identity-change listener, because
		// the window it is about only exists WHILE that reload is in flight.
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

const roleUpdates = vi.hoisted(() => [] as Array<{ resolve: (v: unknown) => void; reject: (e: unknown) => void }>);
const itemUpdates = vi.hoisted(() => [] as Array<{ args: unknown[]; resolve: (v: unknown) => void }>);
const boardCalls = vi.hoisted(() => [] as Array<(v: unknown) => void>);
// Which user `/auth/session` answers with. The resume leg flips it so the
// reload lands under a DIFFERENT identity than the mount did — without that,
// "the page works again" and "the page never changed hands" are the same
// observation (CONVE-12).
const sessionUser = vi.hoisted(() => ({ id: 'u1' }));

vi.mock('$lib/api/client', () => ({
	api: {
		agentRoles: {
			board: vi.fn(() => new Promise((resolve) => { boardCalls.push(resolve as (v: unknown) => void); })),
			update: vi.fn(() => new Promise((resolve, reject) => {
				roleUpdates.push({ resolve: resolve as (v: unknown) => void, reject });
			})),
			create: vi.fn(async () => ({})),
			delete: vi.fn(async () => ({})),
			reorder: vi.fn(async () => ({})),
			reorderLanes: vi.fn(async () => ({})),
		},
		items: {
			create: vi.fn(async () => ({})),
			update: vi.fn((...args: unknown[]) => new Promise((resolve) => {
				itemUpdates.push({ args, resolve: resolve as (v: unknown) => void });
			})),
		},
		auth: { session: vi.fn(async () => ({ authenticated: true, user: { id: sessionUser.id } })) },
	},
	isPlanLimitError: () => false,
	planLimitMessage: () => '',
}));

vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: {
		get isOwner() { return true; },
		get currentRole() { return 'owner'; },
		get current() { return { id: 'ws1', slug: 'ws', name: 'WS' }; },
		get currentMembership() { return { role: 'owner' }; },
		canEditCollection: () => true,
		canEditItem: () => true,
		setCurrent: vi.fn(async () => {}),
	},
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: { get collections() { return []; }, loadCollections: vi.fn(async () => {}) },
}));
vi.mock('$lib/stores/ui.svelte', () => ({
	uiStore: {
		registerCollectionSearch: () => {},
		unregisterCollectionSearch: () => {},
		onNavigate: () => () => {},
	},
}));
vi.mock('svelte-dnd-action', () => ({
	dndzone: () => ({ destroy: () => {} }),
	TRIGGERS: { DROPPED_INTO_ZONE: 'droppedIntoZone' },
	SHADOW_ITEM_MARKER_PROPERTY_NAME: '__dndShadow',
	DRAGGED_ELEMENT_ID: 'dnd-action-dragged-el',
}));

import { api } from '$lib/api/client';
import { page } from '$app/state';
import RolesPage from './+page.svelte';

const roleNamed = (name: string) => ({ id: `r-${name}`, name, slug: name.toLowerCase(), icon: '🔨', description: '', tools: '' });
const boardFor = (name: string) => ({ lanes: [{ role: null, items: [] }, { role: roleNamed(name), items: [] }] });

async function nextBoardCall(n: number): Promise<(v: unknown) => void> {
	return waitFor(() => {
		if (boardCalls.length < n) throw new Error(`board fetch ${n} not issued yet`);
		return boardCalls[n - 1]!;
	});
}

function mountAt(ws: string) {
	toasts.length = 0;
	boardCalls.length = 0;
	vi.mocked(api.agentRoles.board).mockClear();
	page.params = { username: 'dave', workspace: ws };
	page.url = new URL(`http://localhost/dave/${ws}/roles`);
	return render(RolesPage);
}

async function navigateTo(ws: string) {
	page.params = { username: 'dave', workspace: ws };
	page.url = new URL(`http://localhost/dave/${ws}/roles`);
	await tick();
}

describe('the roles board follows the workspace in the route (BUG-3701)', () => {
	afterEach(cleanup);

	it('reloads for the new workspace and stops showing the old one', async () => {
		mountAt('alpha');
		(await nextBoardCall(1))(boardFor('AlphaRole'));
		await waitFor(() => expect(document.body.textContent).toContain('AlphaRole'));

		await navigateTo('beta');
		const resolveBeta = await nextBoardCall(2);
		expect(vi.mocked(api.agentRoles.board).mock.calls.map((c) => c[0])).toEqual(['alpha', 'beta']);
		// While B loads, A's board is not on screen under B's route.
		expect(document.body.textContent).not.toContain('AlphaRole');

		resolveBeta(boardFor('BetaRole'));
		await waitFor(() => expect(document.body.textContent).toContain('BetaRole'));
		expect(document.body.textContent).not.toContain('AlphaRole');
	});

	it("a slow load for the old workspace does not paint over the new one's", async () => {
		mountAt('alpha');
		const resolveAlpha = await nextBoardCall(1);

		await navigateTo('beta');
		const resolveBeta = await nextBoardCall(2);
		resolveBeta(boardFor('BetaRole'));
		await waitFor(() => expect(document.body.textContent).toContain('BetaRole'));

		resolveAlpha(boardFor('AlphaRole'));
		await tick();
		await new Promise((r) => setTimeout(r, 20));
		expect(document.body.textContent).toContain('BetaRole');
		expect(document.body.textContent).not.toContain('AlphaRole');
	});

	it('CONTROL: the same route does not reload', async () => {
		mountAt('alpha');
		(await nextBoardCall(1))(boardFor('AlphaRole'));
		await waitFor(() => expect(document.body.textContent).toContain('AlphaRole'));
		await navigateTo('alpha');
		await new Promise((r) => setTimeout(r, 20));
		expect(vi.mocked(api.agentRoles.board)).toHaveBeenCalledTimes(1);
	});
});
