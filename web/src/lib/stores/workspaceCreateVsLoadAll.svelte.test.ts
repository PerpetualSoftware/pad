import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { Workspace } from '$lib/types';

/**
 * BUG-2981 — a `loadAll` response that PREDATES a `create` must not erase it.
 *
 * `loadAll` commits its list wholesale (`workspaces = list`) and `create`
 * appends (`workspaces = [...workspaces, ws]`). The two fence themselves
 * independently and not against each other: `loadAll`'s `isLatest()` orders
 * list calls against other LIST calls, and `create`'s membership token orders
 * selection against navigation. Neither pair spans this one.
 *
 * So a list request issued BEFORE a create legitimately answers without the new
 * workspace — the server is right about what existed when it answered — and
 * committing that answer after the append drops a workspace that really exists.
 * `current` is set separately and survives, which is what makes the symptom
 * quiet: the workspace is open and usable while absent from the switcher.
 *
 * THE FIRST LEG FAILS AGAINST THE UNFIXED STORE. Verified by reverting the
 * reconciliation in `loadAll` to a bare `workspaces = list`: it then reports
 * `['old']` and the created workspace is gone.
 *
 * The other three legs are the fix's own failure modes, which the first leg
 * cannot see: duplicating a workspace the server DID report, resurrecting one
 * that was created and then deleted, and carrying one user's create into the
 * next user's list.
 */

const api = vi.hoisted(() => ({
	workspaces: { list: vi.fn(), get: vi.fn(), me: vi.fn(), create: vi.fn() },
}));
vi.mock('$lib/api/client', () => ({ api }));

const auth = vi.hoisted(() => {
	const listeners = new Set<() => void>();
	let epoch = 0;
	let id = 'user-1';
	return {
		get userId() { return id; },
		get identityEpoch() { return epoch; },
		identityFence() {
			const captured = epoch;
			return () => epoch === captured;
		},
		onIdentityChange(fn: () => void) {
			listeners.add(fn);
			return () => listeners.delete(fn);
		},
		fireIdentityChange(nextId?: string) {
			if (nextId !== undefined) id = nextId;
			epoch++;
			for (const fn of listeners) fn();
		},
		reset() {
			listeners.clear();
			epoch = 0;
			id = 'user-1';
		},
	};
});
vi.mock('./auth.svelte', () => ({ authStore: auth }));

function ws(slug: string): Workspace {
	return { id: `id-${slug}`, slug, name: slug, owner_username: 'dave' } as unknown as Workspace;
}

/** A promise plus the handle to settle it, so a test can choose the order. */
function deferred<T>() {
	let resolve!: (value: T) => void;
	const promise = new Promise<T>((res) => { resolve = res; });
	return { promise, resolve };
}

async function load() {
	const { workspaceStore } = await import('./workspace.svelte');
	return workspaceStore;
}

beforeEach(() => {
	// Fresh module per test: the store holds module-level rune state with no reset.
	vi.resetModules();
	auth.reset();
	api.workspaces.list.mockReset();
	api.workspaces.create.mockReset();
	api.workspaces.me.mockReset();
	api.workspaces.me.mockResolvedValue({ role: 'owner' });
});

describe('BUG-2981 — loadAll vs create', () => {
	it('keeps a workspace created while an older list request was in flight', async () => {
		const store = await load();

		// A list request is issued and does not answer yet.
		const list = deferred<Workspace[]>();
		api.workspaces.list.mockReturnValueOnce(list.promise);
		const loading = store.loadAll();

		// The create completes WHILE that request is open, so the response
		// already on its way cannot contain the new workspace.
		api.workspaces.create.mockResolvedValueOnce(ws('fresh'));
		await store.create({ name: 'fresh' } as never);
		expect(store.workspaces.map((w) => w.slug)).toEqual(['fresh']);

		// The server answers with what existed when it was asked. It is not
		// stale in the ordering sense — it is simply older than the create.
		list.resolve([ws('old')]);
		await loading;

		expect(store.workspaces.map((w) => w.slug)).toEqual(['old', 'fresh']);
	});

	it('does not duplicate a created workspace the response DOES contain', async () => {
		const store = await load();

		const list = deferred<Workspace[]>();
		api.workspaces.list.mockReturnValueOnce(list.promise);
		const loading = store.loadAll();

		api.workspaces.create.mockResolvedValueOnce(ws('fresh'));
		await store.create({ name: 'fresh' } as never);

		// This response was issued before the create but answers after the
		// workspace exists server-side, so it reports it. Re-appending here is
		// the reconciliation's own failure mode.
		list.resolve([ws('old'), ws('fresh')]);
		await loading;

		expect(store.workspaces.map((w) => w.slug)).toEqual(['old', 'fresh']);
	});

	it('does not resurrect a created workspace that a LATER list omits', async () => {
		const store = await load();

		api.workspaces.create.mockResolvedValueOnce(ws('fresh'));
		await store.create({ name: 'fresh' } as never);

		// A request issued AFTER the create is authoritative about it: an
		// omission now means the workspace is gone (deleted, or no longer
		// visible), not that the response predates it.
		api.workspaces.list.mockResolvedValueOnce([ws('old')]);
		await store.loadAll();

		expect(store.workspaces.map((w) => w.slug)).toEqual(['old']);
	});

	// WHAT THIS LEG DOES AND DOES NOT PIN. It pins the end-to-end property — A's
	// create must not reach B's list — which is worth having whichever mechanism
	// delivers it. It does NOT test the reset's `pendingCreates = []`: removing
	// that line leaves this leg green, because the run in flight at the reset
	// cannot commit (`invalidate()` strips its `isLatest`) and the later run
	// drops the entry by the termination rule. No leg can discriminate that
	// clear, because nothing can reach it; see the comment beside it for why it
	// is kept regardless.
	it('does not carry one user\'s create into the next user\'s list', async () => {
		const store = await load();

		const list = deferred<Workspace[]>();
		api.workspaces.list.mockReturnValueOnce(list.promise);
		const loading = store.loadAll();

		api.workspaces.create.mockResolvedValueOnce(ws('a-private'));
		await store.create({ name: 'a-private' } as never);

		// A signs out, B signs in.
		auth.fireIdentityChange('user-2');
		list.resolve([ws('old')]);
		await loading;

		api.workspaces.list.mockResolvedValueOnce([ws('b-own')]);
		await store.loadAll();

		expect(store.workspaces.map((w) => w.slug)).toEqual(['b-own']);
	});
});
