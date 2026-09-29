import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

/**
 * TASK-3271 (PLAN-3002 U2): the web tabs store.
 *
 * Every request replaces the whole open set, so the store's two fences are
 * ORDER (the answer with the highest server revision wins, BUG-3285, so an
 * earlier-processed one never erases a later write) and IDENTITY (an answer issued as one identity never commits into
 * another's store). Since TASK-3279 (U5) a landing opens an ephemeral tab,
 * a write in a workspace keeps its ephemeral tab, and last route lives on the
 * tab row only: the localStorage fallback is retired and its keys swept.
 */

const api = vi.hoisted(() => {
	class PadApiError extends Error {
		code: string;
		constructor(e: { code: string; message: string }) {
			super(e.message);
			this.code = e.code;
		}
	}
	return {
		PadApiError,
		api: {
			workspaces: {
				tabs: {
					list: vi.fn(),
					open: vi.fn(),
					close: vi.fn(),
					reorder: vi.fn(),
					update: vi.fn(),
				},
			},
		},
	};
});

vi.mock('$lib/api/client', () => api);

// A REAL epoch and fence: a stub that always answered "still current" would let
// the identity assertions pass against a store with no fence at all.
const auth = vi.hoisted(() => {
	const listeners = new Set<() => void>();
	let epoch = 0;
	return {
		userId: 'user-1',
		identityFence() {
			const captured = epoch;
			return () => epoch === captured;
		},
		onIdentityChange(fn: () => void) {
			listeners.add(fn);
			return () => listeners.delete(fn);
		},
		fireIdentityChange() {
			epoch++;
			for (const fn of listeners) fn();
		},
		reset() {
			listeners.clear();
			epoch = 0;
		},
	};
});

vi.mock('./auth.svelte', () => ({ authStore: auth }));

const tabsApi = api.api.workspaces.tabs;

type Tab = { slug: string; ephemeral: boolean; position: number; last_route?: string };

function tab(slug: string, extra: Partial<Tab> = {}): Tab {
	return { slug, ephemeral: false, position: 0, ...extra };
}

// Every answer carries a revision (BUG-3285). By default each answer BUILT
// is numbered after the last one, which models a server processing requests
// in the order a test builds their answers; a test about crossing requests
// states its revisions with answerAt.
let nextRevision = 1;
function answer(...tabs: Tab[]) {
	return answerAt(nextRevision++, ...tabs);
}
function answerAt(revision: number, ...tabs: Tab[]) {
	return {
		revision,
		tabs: tabs.map((t, i) => ({ name: t.slug, owner_username: 'alice', is_guest: false, created_at: '', updated_at: '', ...t, position: i })),
	};
}

function deferred<T>() {
	let resolve!: (v: T) => void;
	let reject!: (e: unknown) => void;
	const promise = new Promise<T>((res, rej) => {
		resolve = res;
		reject = rej;
	});
	return { promise, resolve, reject };
}

const slugs = (tabs: { slug: string }[]) => tabs.map((t) => t.slug);
const key = (slug: string) => `pad-last-route-${slug}`;

async function loadStore() {
	return (await import('./tabs.svelte')).tabsStore;
}

// Let the store's fire-and-forget PATCHes run.
async function settle() {
	for (let i = 0; i < 5; i++) await Promise.resolve();
}

beforeEach(() => {
	vi.resetModules();
	auth.reset();
	nextRevision = 1;
	localStorage.clear();
	for (const fn of Object.values(tabsApi)) fn.mockReset();
});

// What the API client does after a successful write: the REAL registry, the
// same module instance the store subscribed to after `vi.resetModules()`.
async function reportWrite(slug: string) {
	const { reportWorkspaceWrite } = await import('$lib/api/workspaceWrites');
	reportWorkspaceWrite(`/workspaces/${slug}/items/TASK-1`, 'PATCH');
}

afterEach(() => {
	vi.useRealTimers();
});

describe('which response commits', () => {
	it('drops a load that resolves after an identity change', async () => {
		const store = await loadStore();
		const first = deferred<ReturnType<typeof answer>>();
		tabsApi.list.mockReturnValueOnce(first.promise);
		const load = store.load();

		auth.fireIdentityChange();
		first.resolve(answer(tab('alphas-private')));
		await load;

		expect(store.tabs).toEqual([]);
		expect(store.loaded).toBe(false);
	});

	it('drops a load from the SAME user\'s previous session (epoch, not user id)', async () => {
		const store = await loadStore();
		const first = deferred<ReturnType<typeof answer>>();
		tabsApi.list.mockReturnValueOnce(first.promise);
		const load = store.load();

		// Out as user-1 and back in as user-1: the user id is unchanged, the
		// session is not.
		auth.fireIdentityChange();
		auth.fireIdentityChange();
		first.resolve(answer(tab('stale')));
		await load;

		expect(store.tabs).toEqual([]);
	});

	it('keeps a tab opened while a load is in flight', async () => {
		const store = await loadStore();
		const list = deferred<ReturnType<typeof answer>>();
		tabsApi.list.mockReturnValueOnce(list.promise);
		const load = store.load();

		tabsApi.open.mockResolvedValueOnce(answerAt(2, tab('a'), tab('b')));
		await store.open('b');
		expect(slugs(store.tabs)).toEqual(['a', 'b']);

		// The list was processed before the open and answers without it.
		list.resolve(answerAt(1, tab('a')));
		await load;

		expect(slugs(store.tabs)).toEqual(['a', 'b']);
	});

	it('commits the later-processed write when the earlier one answers last', async () => {
		const store = await loadStore();
		const olderOpen = deferred<ReturnType<typeof answer>>();
		tabsApi.open.mockReturnValueOnce(olderOpen.promise);
		const open = store.open('b');

		tabsApi.close.mockResolvedValueOnce(answerAt(2, tab('a')));
		await store.close('b');

		olderOpen.resolve(answerAt(1, tab('a'), tab('b')));
		await open;

		expect(slugs(store.tabs)).toEqual(['a']);
	});

	it('does not let a FAILED newer request lock out an older answer', async () => {
		const store = await loadStore();
		const list = deferred<ReturnType<typeof answer>>();
		tabsApi.list.mockReturnValueOnce(list.promise);
		const load = store.load();

		tabsApi.reorder.mockRejectedValueOnce(new TypeError('network down'));
		await expect(store.reorder(['a'])).rejects.toThrow('network down');

		list.resolve(answer(tab('a')));
		await load;

		expect(slugs(store.tabs)).toEqual(['a']);
	});

	it('a second ephemeral open replaces the first, as the server does', async () => {
		// A server double implementing U1's rule: an ephemeral open replaces
		// the current ephemeral tab and takes its position.
		let open: Tab[] = [tab('kept')];
		tabsApi.open.mockImplementation(async (slug: string, ephemeral: boolean) => {
			const at = open.findIndex((t) => t.ephemeral);
			const next = tab(slug, { ephemeral });
			if (ephemeral && at >= 0) open = open.map((t, i) => (i === at ? next : t));
			else open = [...open, next];
			return answer(...open);
		});
		const store = await loadStore();

		await store.open('landing-1', true);
		await store.open('landing-2', true);

		expect(store.tabs.map((t) => [t.slug, t.ephemeral])).toEqual([
			['kept', false],
			['landing-2', true],
		]);
	});
});

describe('crossing requests commit by revision, not by send order (BUG-3285)', () => {
	it('the later-SENT request processed first cannot put a closed tab back', async () => {
		// The bug's trace: DELETE c sent, then a user action on a sent 22 ms
		// later; the server ran the second one FIRST, and its answer (still
		// holding c) arrived first. A send-order ticket committed it last.
		tabsApi.list.mockResolvedValueOnce(answerAt(1, tab('a'), tab('b'), tab('c')));
		const store = await loadStore();
		await store.load();

		const closing = deferred<ReturnType<typeof answer>>();
		tabsApi.close.mockReturnValueOnce(closing.promise);
		const reordering = deferred<ReturnType<typeof answer>>();
		tabsApi.reorder.mockReturnValueOnce(reordering.promise);

		const close = store.close('c');
		const reorder = store.reorder(['b', 'a', 'c']);
		reordering.resolve(answerAt(2, tab('b'), tab('a'), tab('c')));
		await reorder;
		closing.resolve(answerAt(3, tab('b'), tab('a')));
		await close;

		expect(slugs(store.tabs)).toEqual(['b', 'a']);
	});

	it('the same crossing answered in the other order keeps the later-processed list', async () => {
		tabsApi.list.mockResolvedValueOnce(answerAt(1, tab('a'), tab('b'), tab('c')));
		const store = await loadStore();
		await store.load();

		const closing = deferred<ReturnType<typeof answer>>();
		tabsApi.close.mockReturnValueOnce(closing.promise);
		const reordering = deferred<ReturnType<typeof answer>>();
		tabsApi.reorder.mockReturnValueOnce(reordering.promise);

		const close = store.close('c');
		const reorder = store.reorder(['b', 'a', 'c']);
		// The close was processed second and answers first; the reorder's
		// earlier-processed answer, still holding c, arrives after it.
		closing.resolve(answerAt(3, tab('b'), tab('a')));
		await close;
		reordering.resolve(answerAt(2, tab('b'), tab('a'), tab('c')));
		await reorder;

		expect(slugs(store.tabs)).toEqual(['b', 'a']);
	});

	it('a read at the committed revision still commits (a visibility change the rows do not record)', async () => {
		tabsApi.list.mockResolvedValueOnce(answerAt(4, tab('a'), tab('lost')));
		const store = await loadStore();
		await store.load();
		tabsApi.list.mockResolvedValueOnce(answerAt(4, tab('a')));
		await store.load();

		expect(slugs(store.tabs)).toEqual(['a']);
	});

	it('a route PATCH processed before a close cannot put the closed tab back', async () => {
		vi.useFakeTimers();
		tabsApi.list.mockResolvedValueOnce(answerAt(1, tab('a'), tab('c')));
		const store = await loadStore();
		await store.load();

		const closing = deferred<ReturnType<typeof answer>>();
		tabsApi.close.mockReturnValueOnce(closing.promise);
		const patching = deferred<ReturnType<typeof answer>>();
		tabsApi.update.mockReturnValueOnce(patching.promise);

		store.noteRoute('a', '/alice/a/tasks');
		const close = store.close('c');
		await vi.advanceTimersByTimeAsync(1_000);
		expect(tabsApi.update).toHaveBeenCalledWith('a', { last_route: '/alice/a/tasks' });

		patching.resolve(answerAt(2, tab('a', { last_route: '/alice/a/tasks' }), tab('c')));
		await vi.runAllTimersAsync();
		closing.resolve(answerAt(3, tab('a', { last_route: '/alice/a/tasks' })));
		await close;

		expect(slugs(store.tabs)).toEqual(['a']);
		expect(store.routeFor('a')).toBe('/alice/a/tasks');
	});

	it('a route PATCH commits its row, and a list processed before it does not undo the route', async () => {
		vi.useFakeTimers();
		tabsApi.list.mockResolvedValueOnce(answerAt(1, tab('a', { last_route: '/alice/a/old' }), tab('b')));
		const store = await loadStore();
		await store.load();

		tabsApi.update.mockResolvedValueOnce(answerAt(3, tab('a', { last_route: '/alice/a/new' }), tab('b')));
		store.noteRoute('a', '/alice/a/new');
		const reordering = deferred<ReturnType<typeof answer>>();
		tabsApi.reorder.mockReturnValueOnce(reordering.promise);
		const reorder = store.reorder(['b', 'a']);
		await vi.runAllTimersAsync();
		expect(store.routeFor('a')).toBe('/alice/a/new');

		// The reorder was processed before the PATCH, so its list still
		// carries the old route and the old order: it does not commit.
		reordering.resolve(answerAt(2, tab('b'), tab('a', { last_route: '/alice/a/old' })));
		await reorder;

		expect(store.routeFor('a')).toBe('/alice/a/new');
		expect(slugs(store.tabs)).toEqual(['a', 'b']);
	});

	it('a route another device set after this one wins at the next read', async () => {
		vi.useFakeTimers();
		tabsApi.list.mockResolvedValueOnce(answerAt(1, tab('a')));
		const store = await loadStore();
		await store.load();
		tabsApi.update.mockResolvedValueOnce(answerAt(2, tab('a', { last_route: '/alice/a/mine' })));
		store.noteRoute('a', '/alice/a/mine');
		await vi.runAllTimersAsync();
		expect(store.routeFor('a')).toBe('/alice/a/mine');

		tabsApi.list.mockResolvedValueOnce(answerAt(3, tab('a', { last_route: '/alice/a/elsewhere' })));
		await store.load();

		expect(store.routeFor('a')).toBe('/alice/a/elsewhere');
	});

	it('a pin a write triggers cannot put a closed tab back either', async () => {
		tabsApi.list.mockResolvedValueOnce(answerAt(1, tab('a'), tab('b', { ephemeral: true }), tab('c')));
		const store = await loadStore();
		await store.load();

		const closing = deferred<ReturnType<typeof answer>>();
		tabsApi.close.mockReturnValueOnce(closing.promise);
		const pinning = deferred<ReturnType<typeof answer>>();
		tabsApi.update.mockReturnValueOnce(pinning.promise);

		const close = store.close('c');
		await reportWrite('b');
		// The pin was processed first (c still open), the close second.
		closing.resolve(answerAt(3, tab('a'), tab('b')));
		await close;
		pinning.resolve(answerAt(2, tab('a'), tab('b'), tab('c')));
		await settle();

		expect(slugs(store.tabs)).toEqual(['a', 'b']);
		expect(store.tabs.find((t) => t.slug === 'b')?.ephemeral).toBe(false);
	});

	it('a pin commits its answer, so the kept tab reads durable', async () => {
		tabsApi.list.mockResolvedValueOnce(answerAt(1, tab('a'), tab('b', { ephemeral: true })));
		const store = await loadStore();
		await store.load();
		tabsApi.update.mockResolvedValueOnce(answerAt(2, tab('a'), tab('b')));

		await reportWrite('b');
		await settle();

		expect(store.tabs.find((t) => t.slug === 'b')?.ephemeral).toBe(false);
	});

	it('the next identity\'s first list commits whatever its revision', async () => {
		tabsApi.list.mockResolvedValueOnce(answerAt(40, tab('first-users')));
		const store = await loadStore();
		await store.load();

		auth.fireIdentityChange();
		tabsApi.list.mockResolvedValueOnce(answerAt(3, tab('second-users')));
		await store.load();

		expect(slugs(store.tabs)).toEqual(['second-users']);
	});
});

describe('last route', () => {
	it('sweeps leftover pre-U5 keys at the first commit and never reads them', async () => {
		localStorage.setItem(key('ws'), '/alice/ws/tasks');
		localStorage.setItem(key('elsewhere'), '/alice/elsewhere/docs');
		localStorage.setItem('pad-theme', 'dark');
		tabsApi.list.mockResolvedValueOnce(answer(tab('ws')));
		const store = await loadStore();

		// Before any commit, a key is not a route either.
		expect(store.routeFor('elsewhere')).toBeNull();
		await store.load();
		await settle();

		expect(localStorage.getItem(key('ws'))).toBeNull();
		expect(localStorage.getItem(key('elsewhere'))).toBeNull();
		expect(localStorage.getItem('pad-theme')).toBe('dark');
		expect(store.routeFor('ws')).toBeNull();
		// Swept, not migrated: no key is moved to a row.
		expect(tabsApi.update).not.toHaveBeenCalled();
	});

	it('writes a navigation in a workspace with a row to the server only, once per burst', async () => {
		vi.useFakeTimers();
		tabsApi.list.mockResolvedValueOnce(answer(tab('ws')));
		const store = await loadStore();
		await store.load();
		tabsApi.update.mockResolvedValue(answer(tab('ws', { last_route: '/alice/ws/c' })));

		store.noteRoute('ws', '/alice/ws/a');
		store.noteRoute('ws', '/alice/ws/b');
		store.noteRoute('ws', '/alice/ws/c');

		// Read immediately, before the write goes out.
		expect(store.routeFor('ws')).toBe('/alice/ws/c');
		expect(localStorage.getItem(key('ws'))).toBeNull();
		expect(tabsApi.update).not.toHaveBeenCalled();

		await vi.runAllTimersAsync();
		expect(tabsApi.update).toHaveBeenCalledTimes(1);
		expect(tabsApi.update).toHaveBeenCalledWith('ws', { last_route: '/alice/ws/c' });
		expect(store.routeFor('ws')).toBe('/alice/ws/c');
	});

	it('never writes a navigation to localStorage, with or without a row', async () => {
		vi.useFakeTimers();
		tabsApi.list.mockResolvedValueOnce(answer(tab('ws')));
		tabsApi.update.mockResolvedValue(answer(tab('ws')));
		const store = await loadStore();
		await store.load();

		store.noteRoute('ws', '/alice/ws/a');
		store.noteRoute('elsewhere', '/alice/elsewhere/b');
		await vi.runAllTimersAsync();

		expect(localStorage.length).toBe(0);
		// A workspace with no row in the committed list still goes to the
		// server, which decides: its landing may have opened one since.
		expect(tabsApi.update).toHaveBeenCalledWith('elsewhere', { last_route: '/alice/elsewhere/b' });
	});

	it('writes a route noted during a landing only after the landing answers', async () => {
		vi.useFakeTimers();
		tabsApi.list.mockResolvedValueOnce(answer(tab('ws')));
		const store = await loadStore();
		await store.load();

		const opening = deferred<ReturnType<typeof answer>>();
		tabsApi.open.mockReturnValueOnce(opening.promise);
		const landing = store.land('new-ws');
		store.noteRoute('new-ws', '/alice/new-ws/tasks');
		await vi.advanceTimersByTimeAsync(5_000);

		// The row the PATCH needs does not exist yet.
		expect(tabsApi.update).not.toHaveBeenCalled();

		opening.resolve(answer(tab('ws'), tab('new-ws', { ephemeral: true })));
		tabsApi.update.mockResolvedValueOnce(answer(tab('ws'), tab('new-ws', { ephemeral: true, last_route: '/alice/new-ws/tasks' })));
		await landing;
		await vi.runAllTimersAsync();

		expect(tabsApi.update).toHaveBeenCalledWith('new-ws', { last_route: '/alice/new-ws/tasks' });
		expect(store.routeFor('new-ws')).toBe('/alice/new-ws/tasks');
	});

	it('drops the route when the row went away before the write', async () => {
		vi.useFakeTimers();
		tabsApi.list.mockResolvedValue(answer(tab('ws')));
		const store = await loadStore();
		await store.load();
		tabsApi.update.mockRejectedValueOnce(new api.PadApiError({ code: 'not_found', message: 'This workspace is not open in a tab' }));

		store.noteRoute('ws', '/alice/ws/tasks');
		await vi.runAllTimersAsync();

		expect(localStorage.length).toBe(0);
		expect(store.routeFor('ws')).toBeNull();
	});

	it('clears a repaired route on the row', async () => {
		tabsApi.list.mockResolvedValueOnce(answer(tab('ws', { last_route: '/alice/ws/tasks/TASK-9' })));
		tabsApi.update.mockResolvedValueOnce(answer(tab('ws')));
		const store = await loadStore();
		await store.load();

		store.setRoute('ws', null);
		await settle();

		expect(tabsApi.update).toHaveBeenCalledWith('ws', { last_route: '' });
		expect(store.routeFor('ws')).toBeNull();
	});

	it('drops routes noted by the previous identity', async () => {
		vi.useFakeTimers();
		tabsApi.list.mockResolvedValueOnce(answer(tab('ws')));
		const store = await loadStore();
		await store.load();

		store.noteRoute('ws', '/alice/ws/private-item');
		auth.fireIdentityChange();
		await vi.runAllTimersAsync();

		expect(tabsApi.update).not.toHaveBeenCalled();
		expect(store.routeFor('ws')).toBeNull();
	});
});

describe('landings (TASK-3279)', () => {
	it('opens a workspace outside the open set as an ephemeral tab', async () => {
		tabsApi.list.mockResolvedValueOnce(answer(tab('ws')));
		tabsApi.open.mockResolvedValueOnce(answer(tab('ws'), tab('deep', { ephemeral: true })));
		const store = await loadStore();
		await store.load();

		await store.land('deep');

		expect(tabsApi.open).toHaveBeenCalledWith('deep', true);
		expect(store.tabs.map((t) => [t.slug, t.ephemeral])).toEqual([
			['ws', false],
			['deep', true],
		]);
	});

	it('sends nothing for a workspace a committed list shows open', async () => {
		tabsApi.list.mockResolvedValueOnce(answer(tab('ws')));
		const store = await loadStore();
		await store.load();

		await store.land('ws');

		expect(tabsApi.open).not.toHaveBeenCalled();
	});

	it('waits for the first list before deciding, and sends nothing when it shows the tab', async () => {
		const list = deferred<ReturnType<typeof answer>>();
		tabsApi.list.mockReturnValueOnce(list.promise);
		const store = await loadStore();
		const load = store.load();

		const landing = store.land('ws');
		await settle();
		expect(tabsApi.open).not.toHaveBeenCalled();

		list.resolve(answer(tab('ws')));
		await Promise.all([load, landing]);
		expect(tabsApi.open).not.toHaveBeenCalled();
		// It rode the list in flight rather than starting a second one.
		expect(tabsApi.list).toHaveBeenCalledTimes(1);
	});

	it('starts the list itself when none is in flight, then opens a missing workspace', async () => {
		tabsApi.list.mockResolvedValueOnce(answer(tab('ws')));
		tabsApi.open.mockResolvedValueOnce(answer(tab('ws'), tab('deep', { ephemeral: true })));
		const store = await loadStore();

		await store.land('deep');

		expect(tabsApi.list).toHaveBeenCalledTimes(1);
		expect(tabsApi.open).toHaveBeenCalledWith('deep', true);
	});

	it('sends no open for a workspace the first list shows open, so a later close stays closed', async () => {
		// The e2e failure was a blind open that reached the server after the
		// close and put the tab back. With no open sent, there is nothing to
		// cross the close.
		const list = deferred<ReturnType<typeof answer>>();
		tabsApi.list.mockReturnValueOnce(list.promise);
		const store = await loadStore();
		const load = store.load();
		const landing = store.land('b');
		list.resolve(answer(tab('a'), tab('b')));
		await Promise.all([load, landing]);

		tabsApi.close.mockResolvedValueOnce(answer(tab('a')));
		await store.close('b');

		expect(tabsApi.open).not.toHaveBeenCalled();
		expect(slugs(store.tabs)).toEqual(['a']);
	});

	it('opens anyway when the first list fails, since the server decides', async () => {
		tabsApi.list.mockRejectedValueOnce(new TypeError('network down'));
		tabsApi.open.mockResolvedValueOnce(answer(tab('deep', { ephemeral: true })));
		const store = await loadStore();

		await store.land('deep');

		expect(tabsApi.open).toHaveBeenCalledWith('deep', true);
	});

	it('drops a landing whose identity changed while it waited', async () => {
		const list = deferred<ReturnType<typeof answer>>();
		tabsApi.list.mockReturnValueOnce(list.promise);
		const store = await loadStore();
		const landing = store.land('deep');
		auth.fireIdentityChange();
		list.resolve(answer());
		await landing;

		expect(tabsApi.open).not.toHaveBeenCalled();
	});

	it('sends one POST for two landings on the same workspace in flight', async () => {
		tabsApi.list.mockResolvedValueOnce(answer(tab('ws')));
		const store = await loadStore();
		await store.load();
		const opening = deferred<ReturnType<typeof answer>>();
		tabsApi.open.mockReturnValueOnce(opening.promise);

		const a = store.land('deep');
		const b = store.land('deep');
		opening.resolve(answer(tab('ws'), tab('deep', { ephemeral: true })));
		await Promise.all([a, b]);

		expect(tabsApi.open).toHaveBeenCalledTimes(1);
	});

	it('leaves the open set alone when the workspace is not visible', async () => {
		tabsApi.list.mockResolvedValueOnce(answer(tab('ws')));
		tabsApi.open.mockRejectedValueOnce(new api.PadApiError({ code: 'not_found', message: 'Workspace not found' }));
		const store = await loadStore();
		await store.load();

		await expect(store.land('hidden')).rejects.toThrow('Workspace not found');

		expect(slugs(store.tabs)).toEqual(['ws']);
		// Not remembered as in flight: a later landing tries again.
		tabsApi.open.mockResolvedValueOnce(answer(tab('ws')));
		await store.land('hidden');
		expect(tabsApi.open).toHaveBeenCalledTimes(2);
	});
});

describe('a write keeps an ephemeral tab (PLAN-3002 Q9)', () => {
	it('pins the ephemeral tab of the workspace written to, once per burst', async () => {
		tabsApi.list.mockResolvedValueOnce(answer(tab('ws'), tab('deep', { ephemeral: true })));
		const store = await loadStore();
		await store.load();
		const pinning = deferred<ReturnType<typeof answer>>();
		tabsApi.update.mockReturnValueOnce(pinning.promise);

		await reportWrite('deep');
		await reportWrite('deep');
		await reportWrite('deep');
		pinning.resolve(answer(tab('ws'), tab('deep')));
		await settle();

		expect(tabsApi.update).toHaveBeenCalledTimes(1);
		expect(tabsApi.update).toHaveBeenCalledWith('deep', { pin: true });
		expect(store.tabs.find((t) => t.slug === 'deep')?.ephemeral).toBe(false);
	});

	it('sends nothing for a write to a durable tab, or to a workspace with no tab', async () => {
		tabsApi.list.mockResolvedValueOnce(answer(tab('ws'), tab('deep', { ephemeral: true })));
		const store = await loadStore();
		await store.load();

		await reportWrite('ws');
		await reportWrite('reorder');
		await reportWrite('elsewhere');
		await settle();

		expect(tabsApi.update).not.toHaveBeenCalled();
	});
});

// TASK-3306: reorders are SENT one at a time, in call order, because the server
// stores whichever full order it processes last and the revision then commits
// it faithfully. A queued reorder is fenced by the identity that asked for it.
describe('reorder is sent in call order', () => {
	it('does not send the second reorder until the first has answered', async () => {
		const store = await loadStore();
		const first = deferred<ReturnType<typeof answer>>();
		// Each answer is numbered when its request is made, as the server does.
		tabsApi.reorder.mockReturnValueOnce(first.promise).mockImplementationOnce(async () => answer(tab('b'), tab('a')));
		const p1 = store.reorder(['a', 'b']);
		const p2 = store.reorder(['b', 'a']);
		await settle();
		expect(tabsApi.reorder).toHaveBeenCalledTimes(1);
		first.resolve(answer(tab('a'), tab('b')));
		await p1;
		await p2;
		expect(tabsApi.reorder).toHaveBeenCalledTimes(2);
		expect(tabsApi.reorder.mock.calls.map((c) => c[0])).toEqual([['a', 'b'], ['b', 'a']]);
		expect(slugs(store.tabs)).toEqual(['b', 'a']);
	});

	it('a failed reorder does not wedge the ones queued behind it', async () => {
		const store = await loadStore();
		tabsApi.reorder.mockRejectedValueOnce(new TypeError('network down')).mockImplementationOnce(async () => answer(tab('b'), tab('a')));
		const p1 = store.reorder(['a', 'b']);
		const p2 = store.reorder(['b', 'a']);
		await expect(p1).rejects.toThrow('network down');
		await p2;
		expect(tabsApi.reorder).toHaveBeenCalledTimes(2);
		expect(slugs(store.tabs)).toEqual(['b', 'a']);
	});

	it('drops a queued reorder whose identity changed while it waited', async () => {
		const store = await loadStore();
		const first = deferred<ReturnType<typeof answer>>();
		tabsApi.reorder.mockReturnValueOnce(first.promise).mockResolvedValue(answer(tab('x')));
		const p1 = store.reorder(['a', 'b']);
		const p2 = store.reorder(['b', 'a']);
		await settle();
		auth.fireIdentityChange();
		first.resolve(answer(tab('a'), tab('b')));
		await p1;
		await p2;
		// Only the first went out; the old user's queued order did not go out
		// with the new identity.
		expect(tabsApi.reorder).toHaveBeenCalledTimes(1);
	});

	it("an old identity's reorder that never settles does not block the next identity's", async () => {
		const store = await loadStore();
		const hung = deferred<ReturnType<typeof answer>>();
		tabsApi.reorder.mockReturnValueOnce(hung.promise).mockImplementationOnce(async () => answer(tab('b'), tab('a')));
		void store.reorder(['a', 'b']).catch(() => {});
		await settle();
		auth.fireIdentityChange();
		await store.reorder(['b', 'a']);
		expect(tabsApi.reorder).toHaveBeenCalledTimes(2);
		expect(slugs(store.tabs)).toEqual(['b', 'a']);
	});
});
