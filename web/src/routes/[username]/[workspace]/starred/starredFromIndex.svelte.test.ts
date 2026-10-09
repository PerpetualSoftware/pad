// TASK-2231: the starred page lists the starred store's ids against the
// local index's rows, and makes no request of its own. What the server's
// starred query used to decide is the page's now, and is pinned here: star
// order (most recently starred first), completed items left out unless asked
// for (judged by each collection's done field), an id the index does not hold
// left out, and an unstar removing the card at once.
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/svelte';

const starred = vi.hoisted(() => ({ ids: [] as string[] }));

vi.mock('$lib/api/client', () => ({
	api: {
		items: {
			// Most recently starred first, as the server answers; summary-shaped.
			starred: vi.fn(async () => starred.ids.map((id) => ({ id }))),
			star: vi.fn(async () => ({})),
			unstar: vi.fn(async () => ({})),
		},
	},
}));
vi.mock('$lib/stores/localIndex.svelte', async () => ({
	localIndex: (await import('../../../../test/mocks/indexedPageStores.svelte')).localIndex,
}));
vi.mock('$lib/stores/collections.svelte', async () => ({
	collectionStore: (await import('../../../../test/mocks/indexedPageStores.svelte')).collectionStore,
}));
vi.mock('$lib/stores/workspaceIndexEntry', async () => ({
	enterWorkspaceIndex: (await import('../../../../test/mocks/indexedPageStores.svelte')).enterWorkspaceIndex,
}));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { current: null, setCurrent: vi.fn(async () => {}) } }));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get identityEpoch() { return 0; },
		get userId() { return 'u1'; },
		identityFence: () => () => true,
		onIdentityChange: () => () => {},
	},
}));
vi.mock('$lib/scroll/restore.svelte', () => ({
	createScrollRestoration: () => ({ snapshot: { capture: () => null, restore: () => {} } }),
}));

import { page } from '$app/state';
import { api } from '$lib/api/client';
import { fake, resetFake } from '../../../../test/mocks/indexedPageStores.svelte';
import { starredStore } from '$lib/stores/starred.svelte';
import StarredPage from './+page.svelte';

const TASKS = {
	id: 'c1', slug: 'tasks', name: 'Tasks', icon: '✅', sort_order: 1, prefix: 'T',
	schema: JSON.stringify({ fields: [{ key: 'stage', type: 'select', options: ['doing', 'shipped'], terminal_options: ['shipped'] }] }),
	settings: JSON.stringify({ board_group_by: 'stage' }),
};

function row(id: string, title: string, over: Record<string, unknown> = {}) {
	return {
		id, slug: id, title, collection_id: 'c1', collection_slug: 'tasks', workspace_id: 'w1',
		fields: '{"stage":"doing"}', tags: '[]', pinned: false,
		created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', ...over,
	};
}

function inOrder(...titles: string[]) {
	const els = titles.map((t) => screen.getByText(t));
	for (let i = 1; i < els.length; i++) {
		expect(els[i - 1].compareDocumentPosition(els[i]) & Node.DOCUMENT_POSITION_FOLLOWING, `${titles[i - 1]} before ${titles[i]}`).toBeTruthy();
	}
}

beforeEach(() => {
	resetFake();
	fake.collections = [TASKS];
	starred.ids = [];
	vi.mocked(api.items.starred).mockClear();
	page.params = { username: 'dave', workspace: 'ws' };
	page.url = new URL('http://localhost/dave/ws/starred');
});
afterEach(() => {
	cleanup();
	starredStore.clear();
});

describe('starred page from the index (TASK-2231)', () => {
	it('lists in star order, not in the index order, and asks only for ids', async () => {
		// The index holds Alpha first (and it is the newer edit); Beta was
		// starred more recently, so star order puts it first.
		fake.rows = [row('a', 'Alpha', { updated_at: '2026-03-01T00:00:00Z' }), row('b', 'Beta')];
		starred.ids = ['b', 'a'];
		await starredStore.load('ws');
		render(StarredPage);
		await screen.findByText('Alpha');
		inOrder('Beta', 'Alpha');
		expect(api.items.starred).toHaveBeenCalledWith('ws', { include_terminal: true, summary: true });
		expect(api.items.starred).toHaveBeenCalledTimes(1);
	});

	it('a new star goes first', async () => {
		fake.rows = [row('a', 'Alpha'), row('b', 'Beta')];
		starred.ids = ['a'];
		await starredStore.load('ws');
		render(StarredPage);
		await screen.findByText('Alpha');
		await starredStore.toggle('ws', 'b', 'b');
		await screen.findByText('Beta');
		inOrder('Beta', 'Alpha');
	});

	it('leaves completed items out until asked, judged by the collection\'s done field', async () => {
		fake.rows = [row('a', 'Shipped one', { fields: '{"stage":"Shipped"}' }), row('b', 'Open one')];
		starred.ids = ['a', 'b'];
		await starredStore.load('ws');
		render(StarredPage);
		await screen.findByText('Open one');
		expect(screen.queryByText('Shipped one')).toBeNull();
		(screen.getByLabelText('Show completed') as HTMLInputElement).click();
		await screen.findByText('Shipped one');
	});

	it('leaves out an id the index does not hold (an item this caller cannot see)', async () => {
		fake.rows = [row('a', 'Alpha')];
		starred.ids = ['hidden', 'a'];
		await starredStore.load('ws');
		render(StarredPage);
		await screen.findByText('Alpha');
		expect(document.querySelector('.count')?.textContent).toBe('1');
	});

	it('an unstar removes the card at once', async () => {
		fake.rows = [row('a', 'Alpha'), row('b', 'Beta')];
		starred.ids = ['a', 'b'];
		await starredStore.load('ws');
		render(StarredPage);
		await screen.findByText('Alpha');
		void starredStore.toggle('ws', 'a', 'a');
		await waitFor(() => expect(screen.queryByText('Alpha')).toBeNull());
		expect(screen.getByText('Beta')).toBeInTheDocument();
	});
});
