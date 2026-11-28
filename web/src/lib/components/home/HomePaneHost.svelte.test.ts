import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/svelte';
import { flushSync, tick } from 'svelte';
import type { WorkspaceMembership } from '$lib/types';

// TASK-4902: the item pane on home. A ref opens in the docked PaneHost with
// its own workspace's scope; the URL carries `item_in` beside `?item=`.
const h = vi.hoisted(() => ({
	gotos: [] as Array<{ url: string; opts: Record<string, unknown> | undefined }>,
	after: [] as Array<(nav: unknown) => void>,
	me: vi.fn(),
	list: vi.fn(),
	epoch: 0,
	resets: new Set<() => void>(),
	stack: [] as Array<{ url: string; state: Record<string, unknown> }>,
	index: 0,
}));

vi.mock('$app/state', () => import('./__fixtures__/reactivePage.svelte'));
vi.mock('$app/navigation', async (importOriginal) => {
	const actual = (await importOriginal()) as Record<string, unknown>;
	const { page } = (await import('./__fixtures__/reactivePage.svelte')) as unknown as { page: { url: URL; state: Record<string, unknown> } };
	return {
		...actual,
		afterNavigate: (fn: (nav: unknown) => void) => {
			h.after.push(fn);
		},
		goto: vi.fn(async (url: string, opts?: Record<string, unknown>) => {
			h.gotos.push({ url: String(url), opts });
			const from = new URL(page.url.href);
			const to = new URL(String(url), page.url);
			const state = (opts?.state as Record<string, unknown>) ?? {};
			if (opts?.replaceState) h.stack[h.index] = { url: to.href, state };
			else {
				h.stack = h.stack.slice(0, h.index + 1);
				h.stack.push({ url: to.href, state });
				h.index = h.stack.length - 1;
			}
			page.url = to;
			page.state = state;
			for (const fn of h.after) fn({ type: 'goto', from: { url: from }, to: { url: to }, shallow: false });
		}),
	};
});
vi.mock('$lib/components/items/itemDetailLoader', async () => {
	const Stub = (await import('./__fixtures__/ScopedItemDetailStub.svelte')).default;
	return { loadItemDetailComponent: async () => Stub, prefetchItemDetail: () => {} };
});
vi.mock('$lib/api/client', () => ({ api: { workspaces: { me: h.me }, collections: { list: h.list } } }));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		userId: 'u1',
		identityFence: () => {
			const epoch = h.epoch;
			return () => epoch === h.epoch;
		},
		onIdentityChange: (fn: () => void) => {
			h.resets.add(fn);
			return () => h.resets.delete(fn);
		},
	},
}));
// The shell workspace, which the global stores describe: owned.
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { isOwner: true, currentMembership: { role: 'owner' }, canEditItem: () => true, canEditCollection: () => true },
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: { collections: [], collectionsWorkspace: null, collectionsAreFreshFor: () => false, loadCollections: async () => {} },
}));

import { page } from '$app/state';
import HomePaneHost from './HomePaneHost.svelte';

function membership(role: WorkspaceMembership['role']): WorkspaceMembership {
	return { role, collection_access: 'all', visible_collection_ids: [], full_access_collection_ids: [], collection_grants: [], item_grants: [] };
}
async function settle() {
	for (let i = 0; i < 6; i++) {
		flushSync();
		await Promise.resolve();
		await tick();
	}
}
function at(url: string) {
	page.url = new URL(url, 'http://localhost/');
	page.state = {};
	h.stack = [{ url: page.url.href, state: {} }];
	h.index = 0;
}
/** A same-route navigation, as SvelteKit reports it. */
async function navigate(url: string, type: 'goto' | 'popstate' = 'goto') {
	const from = new URL(page.url.href);
	page.url = new URL(url, 'http://localhost/');
	for (const fn of h.after) fn({ type, from: { url: from }, to: { url: page.url }, shallow: false });
	await settle();
}
const stub = () => screen.queryByTestId('item-detail-stub');
const lastGoto = () => new URL(h.gotos[h.gotos.length - 1].url, 'http://localhost/');

beforeEach(() => {
	vi.clearAllMocks();
	h.gotos = [];
	h.after = [];
	h.epoch++;
	h.resets.clear();
	(globalThis as { __detailMounts?: number }).__detailMounts = 0;
	h.me.mockImplementation(async (slug: string) => membership(slug === 'guest-ws' ? 'guest' : 'viewer'));
	h.list.mockResolvedValue([]);
	at('/');
});
afterEach(() => {
	cleanup();
	document.body.innerHTML = '';
});

describe('HomePaneHost (TASK-4902)', () => {
	it('open writes ?item= and item_in, and the pane gets the item workspace and its scope', async () => {
		const { component } = render(HomePaneHost);
		await settle();
		component.open({ owner: 'jun', workspace: 'guest-ws', collection: 'bugs', ref: 'BUG-7' });
		await settle();
		const url = lastGoto();
		expect(url.pathname).toBe('/');
		expect(url.searchParams.get('item')).toBe('BUG-7');
		expect(url.searchParams.get('item_in')).toBe('jun/guest-ws/bugs');
		await waitFor(() => expect(stub()).not.toBeNull());
		const s = stub()!;
		expect(s.dataset.ref).toBe('BUG-7');
		expect(s.dataset.username).toBe('jun');
		expect(s.dataset.ws).toBe('guest-ws');
		expect(s.dataset.coll).toBe('bugs');
		expect(s.dataset.scopeSlug).toBe('guest-ws');
		expect(h.me).toHaveBeenCalledWith('guest-ws');
		// The shell workspace's store says owner; the pane's scope does not.
		await waitFor(() => expect(h.list).toHaveBeenCalledWith('guest-ws'));
		await settle();
		expect(stub()!.dataset.scopeOwner).toBe('false');
	});

	it('renders no pane for a missing or malformed item_in, and leaves the URL alone', async () => {
		at('/?item=BUG-7&item_in=jun%2Fguest-ws');
		render(HomePaneHost);
		await settle();
		expect(document.querySelector('.item-pane')).toBeNull();
		expect(h.gotos).toEqual([]);
		await navigate('/?item=BUG-7');
		expect(document.querySelector('.item-pane')).toBeNull();
		expect(h.gotos).toEqual([]);
	});

	it('a cross-workspace switch remounts the pane with a new scope; a same-workspace switch does not', async () => {
		at('/?item=TASK-1&item_in=mara%2Fpad%2Ftasks');
		render(HomePaneHost);
		await waitFor(() => expect(stub()).not.toBeNull());
		expect(stub()!.dataset.mount).toBe('1');
		expect(stub()!.dataset.scopeSlug).toBe('pad');

		await navigate('/?item=TASK-2&item_in=mara%2Fpad%2Fbugs');
		expect(stub()!.dataset.ref).toBe('TASK-2');
		expect(stub()!.dataset.coll).toBe('bugs');
		expect(stub()!.dataset.mount).toBe('1');
		expect(h.me).toHaveBeenCalledTimes(1);

		await navigate('/?item=BUG-7&item_in=jun%2Fguest-ws%2Fbugs');
		await waitFor(() => expect(stub()?.dataset.ref).toBe('BUG-7'));
		expect(stub()!.dataset.mount).toBe('2');
		expect(stub()!.dataset.scopeSlug).toBe('guest-ws');
		expect(stub()!.dataset.username).toBe('jun');
		expect(h.me).toHaveBeenLastCalledWith('guest-ws');
	});

	it('ESC closes and strips both params; ESC in a textarea does not', async () => {
		at('/?item=TASK-1&item_in=mara%2Fpad%2Ftasks&other=1');
		render(HomePaneHost);
		await waitFor(() => expect(stub()).not.toBeNull());

		const ta = document.createElement('textarea');
		document.body.appendChild(ta);
		ta.focus();
		ta.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }));
		await settle();
		expect(h.gotos).toEqual([]);

		window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }));
		await settle();
		const url = lastGoto();
		expect(url.searchParams.has('item')).toBe(false);
		expect(url.searchParams.has('item_in')).toBe(false);
		expect(url.searchParams.get('other')).toBe('1');
		expect(h.gotos[0].opts?.replaceState).toBe(true);
		expect(document.querySelector('.item-pane')).toBeNull();
	});

	it('returns focus to the captured trigger, else the ref link, else the agent strip, else the Agents panel', async () => {
		document.body.innerHTML = `
			<section aria-label="Agents" tabindex="-1">
				<ul><li><button data-agent-toggle="impl">Implementer</button>
					<a id="ref" href="/mara/pad/tasks/TASK-1" data-pane-key="mara/pad/TASK-1">TASK-1</a></li></ul>
			</section>
			<button id="other">other</button>`;
		const { component } = render(HomePaneHost);
		await settle();
		const link = document.getElementById('ref')!;

		// 1. The captured trigger.
		link.focus();
		component.open({ owner: 'mara', workspace: 'pad', collection: 'tasks', ref: 'TASK-1' });
		await waitFor(() => expect(stub()).not.toBeNull());
		(document.querySelector('.item-pane') as HTMLElement).focus();
		await navigate('/');
		expect(document.activeElement).toBe(link);

		// 2. A deep link has no trigger: the ref link by its pane key.
		await navigate('/?item=TASK-1&item_in=mara%2Fpad%2Ftasks');
		await waitFor(() => expect(stub()).not.toBeNull());
		document.getElementById('other')!.focus();
		(document.querySelector('.item-pane') as HTMLElement).focus();
		await navigate('/');
		expect(document.activeElement).toBe(link);

		// 3. The agent collapsed: its strip.
		link.focus();
		component.open({ owner: 'mara', workspace: 'pad', collection: 'tasks', ref: 'TASK-1' });
		await waitFor(() => expect(stub()).not.toBeNull());
		link.remove();
		await navigate('/');
		expect(document.activeElement).toBe(document.querySelector('[data-agent-toggle="impl"]'));

		// 4. The agent gone too: the Agents panel.
		await navigate('/?item=TASK-1&item_in=mara%2Fpad%2Ftasks');
		await waitFor(() => expect(stub()).not.toBeNull());
		document.querySelector('[data-agent-toggle]')!.remove();
		await navigate('/');
		expect(document.activeElement).toBe(document.querySelector('section[aria-label="Agents"]'));
	});

	it('an identity change closes the pane in place', async () => {
		at('/?item=TASK-1&item_in=mara%2Fpad%2Ftasks');
		render(HomePaneHost);
		await waitFor(() => expect(stub()).not.toBeNull());
		h.epoch++;
		for (const r of [...h.resets]) r();
		await settle();
		const url = lastGoto();
		expect(url.searchParams.has('item')).toBe(false);
		expect(url.searchParams.has('item_in')).toBe(false);
		expect(h.gotos[0].opts?.replaceState).toBe(true);
		expect(document.querySelector('.item-pane')).toBeNull();
	});

	it('a click on dead space in the home column closes the desktop pane; a click on a control does not', async () => {
		at('/?item=TASK-1&item_in=mara%2Fpad%2Ftasks');
		const { component } = render(HomePaneHost);
		await waitFor(() => expect(stub()).not.toBeNull());
		const col = document.createElement('div');
		col.innerHTML = '<p id="dead">text</p><button id="ctl">x</button>';
		document.body.appendChild(col);
		col.addEventListener('click', (e) => component.dismissOnBackgroundClick(e));
		document.getElementById('ctl')!.click();
		await settle();
		expect(h.gotos).toEqual([]);
		document.getElementById('dead')!.click();
		await settle();
		expect(lastGoto().searchParams.has('item')).toBe(false);
	});
});
