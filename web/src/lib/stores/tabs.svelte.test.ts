import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

/**
 * TASK-3271 (PLAN-3002 U2): the web tabs store.
 *
 * Every request replaces the whole open set, so the store's two fences are
 * ORDER (the newest-issued answer wins, and an older one never erases a newer
 * write) and IDENTITY (an answer issued as one identity never commits into
 * another's store). Last route follows the lead's option-(b) ruling: the tab
 * row when the workspace has one, the pre-U2 localStorage key when it does
 * not, and a key moves to its row only once that row exists.
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

function answer(...tabs: Tab[]) {
	return { tabs: tabs.map((t, i) => ({ name: t.slug, owner_username: 'alice', is_guest: false, created_at: '', updated_at: '', ...t, position: i })) };
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
	localStorage.clear();
	for (const fn of Object.values(tabsApi)) fn.mockReset();
});

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

		tabsApi.open.mockResolvedValueOnce(answer(tab('a'), tab('b')));
		await store.open('b');
		expect(slugs(store.tabs)).toEqual(['a', 'b']);

		// The list was issued before the open and answers without it.
		list.resolve(answer(tab('a')));
		await load;

		expect(slugs(store.tabs)).toEqual(['a', 'b']);
	});

	it('commits the newer write when an older one answers last', async () => {
		const store = await loadStore();
		const olderOpen = deferred<ReturnType<typeof answer>>();
		tabsApi.open.mockReturnValueOnce(olderOpen.promise);
		const open = store.open('b');

		tabsApi.close.mockResolvedValueOnce(answer(tab('a')));
		await store.close('b');

		olderOpen.resolve(answer(tab('a'), tab('b')));
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

describe('last route', () => {
	it('migrates a key to its row once, uses it, and removes the key', async () => {
		localStorage.setItem(key('ws'), '/alice/ws/tasks');
		tabsApi.list.mockResolvedValueOnce(answer(tab('ws')));
		tabsApi.update.mockResolvedValueOnce(answer(tab('ws', { last_route: '/alice/ws/tasks' })));
		const store = await loadStore();

		await store.load();
		await settle();

		expect(tabsApi.update).toHaveBeenCalledTimes(1);
		expect(tabsApi.update).toHaveBeenCalledWith('ws', { last_route: '/alice/ws/tasks' });
		expect(localStorage.getItem(key('ws'))).toBeNull();
		expect(store.routeFor('ws')).toBe('/alice/ws/tasks');

		// Used once: a later commit finds no key and sends nothing.
		tabsApi.list.mockResolvedValueOnce(answer(tab('ws', { last_route: '/alice/ws/tasks' })));
		await store.load();
		await settle();
		expect(tabsApi.update).toHaveBeenCalledTimes(1);
	});

	it('reads the row\'s own route and discards the key when the row already has one', async () => {
		localStorage.setItem(key('ws'), '/alice/ws/old');
		tabsApi.list.mockResolvedValueOnce(answer(tab('ws', { last_route: '/alice/ws/newer' })));
		const store = await loadStore();

		await store.load();
		await settle();

		expect(tabsApi.update).not.toHaveBeenCalled();
		expect(localStorage.getItem(key('ws'))).toBeNull();
		expect(store.routeFor('ws')).toBe('/alice/ws/newer');
	});

	it('keeps the localStorage route for a workspace with no row (the fallback)', async () => {
		localStorage.setItem(key('elsewhere'), '/alice/elsewhere/docs');
		tabsApi.list.mockResolvedValueOnce(answer(tab('ws')));
		const store = await loadStore();

		await store.load();
		await settle();

		expect(store.routeFor('elsewhere')).toBe('/alice/elsewhere/docs');
		expect(localStorage.getItem(key('elsewhere'))).toBe('/alice/elsewhere/docs');

		// And a navigation there writes localStorage, not the server.
		store.noteRoute('elsewhere', '/alice/elsewhere/ideas');
		expect(localStorage.getItem(key('elsewhere'))).toBe('/alice/elsewhere/ideas');
		expect(tabsApi.update).not.toHaveBeenCalled();
	});

	it('migrates a key when its workspace\'s row is opened later in the session', async () => {
		localStorage.setItem(key('later'), '/alice/later/tasks');
		tabsApi.list.mockResolvedValueOnce(answer(tab('ws')));
		const store = await loadStore();
		await store.load();
		await settle();
		expect(localStorage.getItem(key('later'))).toBe('/alice/later/tasks');

		tabsApi.open.mockResolvedValueOnce(answer(tab('ws'), tab('later')));
		tabsApi.update.mockResolvedValueOnce(answer(tab('ws'), tab('later', { last_route: '/alice/later/tasks' })));
		await store.open('later');
		await settle();

		expect(tabsApi.update).toHaveBeenCalledWith('later', { last_route: '/alice/later/tasks' });
		expect(localStorage.getItem(key('later'))).toBeNull();
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

	it('moves a route noted before the first load answers to the row, over the row\'s older value', async () => {
		const store = await loadStore();
		store.noteRoute('ws', '/alice/ws/just-now');
		expect(localStorage.getItem(key('ws'))).toBe('/alice/ws/just-now');

		tabsApi.list.mockResolvedValueOnce(answer(tab('ws', { last_route: '/alice/ws/yesterday' })));
		tabsApi.update.mockResolvedValueOnce(answer(tab('ws', { last_route: '/alice/ws/just-now' })));
		await store.load();
		await settle();

		expect(tabsApi.update).toHaveBeenCalledWith('ws', { last_route: '/alice/ws/just-now' });
		expect(store.routeFor('ws')).toBe('/alice/ws/just-now');
		expect(localStorage.getItem(key('ws'))).toBeNull();
	});

	it('keeps a key noted before the first load winning when its row opens several commits later', async () => {
		// Codex round 1: the "written here" mark used to be dropped at the
		// first commit, even for a workspace that had no row yet.
		const store = await loadStore();
		store.noteRoute('later', '/alice/later/just-now');

		tabsApi.list.mockResolvedValueOnce(answer(tab('ws')));
		await store.load();
		await settle();
		expect(localStorage.getItem(key('later'))).toBe('/alice/later/just-now');

		// Opened on another device meanwhile, with an older route of its own.
		tabsApi.open.mockResolvedValueOnce(answer(tab('ws'), tab('later', { last_route: '/alice/later/older' })));
		tabsApi.update.mockResolvedValueOnce(answer(tab('ws'), tab('later', { last_route: '/alice/later/just-now' })));
		await store.open('later');
		await settle();

		expect(tabsApi.update).toHaveBeenCalledWith('later', { last_route: '/alice/later/just-now' });
		expect(store.routeFor('later')).toBe('/alice/later/just-now');
		expect(localStorage.getItem(key('later'))).toBeNull();
	});

	it('lets a row\'s route win over a key this session did not write', async () => {
		// A key left by an earlier session predates anything a row holds now.
		localStorage.setItem(key('later'), '/alice/later/last-week');
		tabsApi.list.mockResolvedValueOnce(answer(tab('ws')));
		const store = await loadStore();
		await store.load();

		tabsApi.open.mockResolvedValueOnce(answer(tab('ws'), tab('later', { last_route: '/alice/later/today' })));
		await store.open('later');
		await settle();

		expect(tabsApi.update).not.toHaveBeenCalled();
		expect(store.routeFor('later')).toBe('/alice/later/today');
		expect(localStorage.getItem(key('later'))).toBeNull();
	});

	it('spends a key the server refuses, and keeps one a failure did not reach', async () => {
		localStorage.setItem(key('bad'), '/mallory/other/tasks');
		localStorage.setItem(key('flaky'), '/alice/flaky/tasks');
		tabsApi.list.mockResolvedValueOnce(answer(tab('bad'), tab('flaky')));
		tabsApi.update.mockImplementation(async (slug: string) => {
			if (slug === 'bad') throw new api.PadApiError({ code: 'validation_error', message: 'last_route must be a path inside this workspace' });
			throw new TypeError('network down');
		});
		const store = await loadStore();

		await store.load();
		await settle();

		expect(localStorage.getItem(key('bad'))).toBeNull();
		expect(localStorage.getItem(key('flaky'))).toBe('/alice/flaky/tasks');
	});

	it('falls back to localStorage when the row went away before the write', async () => {
		vi.useFakeTimers();
		tabsApi.list.mockResolvedValue(answer(tab('ws')));
		const store = await loadStore();
		await store.load();
		tabsApi.update.mockRejectedValueOnce(new api.PadApiError({ code: 'not_found', message: 'This workspace is not open in a tab' }));
		tabsApi.list.mockResolvedValue(answer());

		store.noteRoute('ws', '/alice/ws/tasks');
		await vi.runAllTimersAsync();

		expect(localStorage.getItem(key('ws'))).toBe('/alice/ws/tasks');
		expect(store.routeFor('ws')).toBe('/alice/ws/tasks');
	});

	it('clears a repaired route on the row, not in localStorage', async () => {
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
