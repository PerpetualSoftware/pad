import { describe, expect, it, vi, beforeEach } from 'vitest';

/**
 * BUG-3005 — `starredStore` holds a PER-USER set and was only ever cleared
 * inside `load()`, which is keyed by workspace slug.
 *
 * A same-route account swap starts no new load, so A's starred ids stayed
 * rendered inside B's session with no request having been made for them. Two
 * halves that fail independently: the identity-change reset (nothing racing),
 * and the identity fence on a settle (something racing).
 */

const api = vi.hoisted(() => ({
	items: {
		starred: vi.fn(),
		star: vi.fn(),
		unstar: vi.fn(),
	},
}));

vi.mock('$lib/api/client', () => ({ api }));

// A REAL epoch and fence, not stubs. A fence stub that always answered "still
// current" would let every assertion below pass against a store with no fence.
const auth = vi.hoisted(() => {
	const listeners = new Set<() => void>();
	let epoch = 0;
	return {
		userId: 'user-1',
		get identityEpoch() { return epoch; },
		identityFence() {
			const captured = epoch;
			return () => epoch === captured;
		},
		onIdentityChange(fn: () => void) {
			listeners.add(fn);
			return () => listeners.delete(fn);
		},
		/** What authStore.clear() or a sign-in as someone else fires. */
		fireIdentityChange() {
			epoch++;
			for (const fn of listeners) fn();
		},
		resetListeners() {
			listeners.clear();
			epoch = 0;
		},
	};
});

vi.mock('./auth.svelte', () => ({ authStore: auth }));

describe('starredStore across an identity change', () => {
	beforeEach(() => {
		vi.resetModules();
		auth.resetListeners();
		api.items.starred.mockReset();
		api.items.star.mockReset();
		api.items.unstar.mockReset();
	});

	it('drops the previous user\'s stars when the identity changes with nothing in flight', async () => {
		const { starredStore } = await import('./starred.svelte');
		api.items.starred.mockResolvedValueOnce([{ id: 'item-a' }]);
		await starredStore.load('ws');

		// PRECONDITION: A's stars are actually loaded, or "they were dropped" is
		// a claim about a set that was always empty.
		expect(starredStore.isStarred('item-a')).toBe(true);
		expect(starredStore.loaded).toBe(true);

		auth.fireIdentityChange();

		expect(starredStore.isStarred('item-a')).toBe(false);
		expect(starredStore.ids.size).toBe(0);
		expect(starredStore.loaded).toBe(false);
	});

	it('refuses a load that RESOLVES after the identity changed', async () => {
		const { starredStore } = await import('./starred.svelte');
		let resolve!: (v: unknown) => void;
		api.items.starred.mockReturnValueOnce(new Promise((r) => { resolve = r; }));

		const inflight = starredStore.load('ws');
		auth.fireIdentityChange();
		resolve([{ id: 'item-a' }]);
		await inflight;

		expect(starredStore.isStarred('item-a')).toBe(false);
		expect(starredStore.loaded).toBe(false);
	});

	it('refuses a FAILED load that rejects after the identity changed', async () => {
		// The catch arm commits too — it publishes an empty set and flips
		// `loaded` — so a fence on the success path alone leaves the store
		// claiming, inside B's session, that B's stars are loaded.
		const { starredStore } = await import('./starred.svelte');
		let reject!: (e: unknown) => void;
		api.items.starred.mockReturnValueOnce(new Promise((_, r) => { reject = r; }));

		const inflight = starredStore.load('ws');
		auth.fireIdentityChange();
		reject(new Error('network'));
		await inflight;

		expect(starredStore.loaded).toBe(false);
	});

	it('does not revert a failed UNSTAR into the NEXT user\'s set', async () => {
		// Direction matters, and the first version of this test had it backwards.
		// Reverting a failed STAR is a delete, which is invisible in an empty
		// set — the test passed with no identity fence at all. Reverting a failed
		// UNSTAR is an ADD, and that is the write that lands one user's starred
		// item in another user's set.
		//
		// B also signs back into the SAME workspace before the revert fires,
		// because the identity-change reset blanks `currentWs` and would
		// otherwise be the guard doing the work. Both halves are needed for this
		// to discriminate.
		const { starredStore } = await import('./starred.svelte');
		api.items.starred.mockResolvedValueOnce([{ id: 'item-a' }]);
		await starredStore.load('ws');
		expect(starredStore.isStarred('item-a')).toBe(true);

		let reject!: (e: unknown) => void;
		api.items.unstar.mockReturnValueOnce(new Promise((_, r) => { reject = r; }));
		const toggling = starredStore.toggle('ws', 'item-a-slug', 'item-a');
		// PRECONDITION: the optimistic REMOVE landed, so the revert is an add.
		expect(starredStore.isStarred('item-a')).toBe(false);

		auth.fireIdentityChange();
		api.items.starred.mockResolvedValueOnce([]);
		await starredStore.load('ws');
		// PRECONDITION: B's set is loaded and empty, and the workspace fence can
		// no longer discriminate.
		expect(starredStore.loaded).toBe(true);
		expect(starredStore.ids.size).toBe(0);

		reject(new Error('nope'));
		await toggling;

		expect(starredStore.isStarred('item-a')).toBe(false);
	});

	it('still commits an ordinary load when the identity holds still', async () => {
		// The counterfactual. A fence that refused everything would empty the
		// starred set for every user permanently, which is worse than the leak.
		const { starredStore } = await import('./starred.svelte');
		api.items.starred.mockResolvedValueOnce([{ id: 'item-a' }]);
		await starredStore.load('ws');

		expect(starredStore.isStarred('item-a')).toBe(true);
		expect(starredStore.loaded).toBe(true);
	});
});
