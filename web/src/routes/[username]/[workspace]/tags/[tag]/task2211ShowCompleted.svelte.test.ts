// TASK-2211 (audit C105): a tag page mixed completed items in with no way to
// leave them out. "Show completed" defaults ON, matching the collection list,
// is remembered per workspace like the view mode, and OFF lists open items
// only.
//
// TASK-2231: the list is built from the local index now, so the rules the
// server query used to apply are this page's to get right, and are pinned
// here: the tag compared exactly, open judged by each collection's done field,
// and the server's order (pinned first, then most recently updated).
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/svelte';

vi.mock('$app/state', async () => ({ page: (await import('../../../../../test/mocks/reactivePage.svelte')).page }));
vi.mock('$app/environment', () => ({ browser: true }));
vi.mock('$lib/api/client', () => ({ api: {} }));
vi.mock('$lib/stores/localIndex.svelte', async () => ({
	localIndex: (await import('../../../../../test/mocks/indexedPageStores.svelte')).localIndex
}));
vi.mock('$lib/stores/collections.svelte', async () => ({
	collectionStore: (await import('../../../../../test/mocks/indexedPageStores.svelte')).collectionStore
}));
vi.mock('$lib/stores/workspaceIndexEntry', async () => ({
	enterWorkspaceIndex: (await import('../../../../../test/mocks/indexedPageStores.svelte')).enterWorkspaceIndex
}));
vi.mock('$lib/stores/auth.svelte', () => ({
	authStore: {
		get identityEpoch() { return 0; },
		get userId() { return 'u1'; },
		identityFence: () => () => true,
		onIdentityChange: () => () => {}
	}
}));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { get current() { return { name: 'WS' }; } } }));
vi.mock('$lib/scroll/restore.svelte', () => ({
	createScrollRestoration: () => ({ snapshot: { capture: () => null, restore: () => {} } })
}));

import { page } from '$app/state';
import { fake, releaseCollections, resetFake } from '../../../../../test/mocks/indexedPageStores.svelte';
import TagPage from './+page.svelte';

// Its done field is `stage` (board_group_by), with `shipped` terminal; `status`
// means nothing here, so a rule that read `status` would get these wrong.
const TASKS = {
	id: 'c1', slug: 'tasks', name: 'Tasks', icon: '✅', sort_order: 1, prefix: 'T',
	schema: JSON.stringify({ fields: [{ key: 'stage', type: 'select', options: ['doing', 'shipped'], terminal_options: ['shipped'] }] }),
	settings: JSON.stringify({ board_group_by: 'stage' })
};

function row(id: string, title: string, over: Record<string, unknown> = {}) {
	return {
		id, slug: id, title, collection_id: 'c1', collection_slug: 'tasks', workspace_id: 'w1',
		fields: '{"stage":"doing"}', tags: '["release"]', pinned: false,
		created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', ...over
	};
}

beforeEach(() => {
	resetFake();
	fake.collections = [TASKS];
	localStorage.clear();
	page.params = { username: 'dave', workspace: 'ws', tag: 'release' };
	page.url = new URL('http://localhost/dave/ws/tags/release');
});
afterEach(() => cleanup());

describe('TASK-2211: Show completed on a tag page', () => {
	it('defaults on, as the collection list does: completed items are listed', async () => {
		fake.rows = [row('i1', 'Done thing', { fields: '{"stage":"Shipped"}' })];
		render(TagPage);
		await screen.findByText('Done thing');
		expect((screen.getByLabelText('Show completed') as HTMLInputElement).checked).toBe(true);
	});

	it('off lists open items only, says so when none are left, and is remembered', async () => {
		// "Shipped" against terminal "shipped": the comparison folds case, as
		// the server's does.
		fake.rows = [row('i1', 'Done thing', { fields: '{"stage":"Shipped"}' })];
		render(TagPage);
		await screen.findByText('Done thing');
		(screen.getByLabelText('Show completed') as HTMLInputElement).click();
		await waitFor(() => expect(screen.getByText(/No open items tagged/)).toBeInTheDocument());
		expect(screen.queryByText('Done thing')).toBeNull();
		// The switch stays reachable with nothing listed.
		expect(screen.getByLabelText('Show completed')).toBeInTheDocument();
		expect(localStorage.getItem('pad-tag-completed-ws')).toBe('hide');
	});

	it('a remembered "hide" applies on the next visit, and an open item stays', async () => {
		localStorage.setItem('pad-tag-completed-ws', 'hide');
		fake.rows = [row('i1', 'Done thing', { fields: '{"stage":"shipped"}' }), row('i2', 'Open thing')];
		render(TagPage);
		await screen.findByText('Open thing');
		expect(screen.queryByText('Done thing')).toBeNull();
	});
});

describe('TASK-2231: the list the index builds', () => {
	it('matches the tag exactly: a case variant or a prefix is a different tag', async () => {
		fake.rows = [
			row('i1', 'Exact'),
			row('i2', 'Upper case', { tags: '["Release"]' }),
			row('i3', 'Longer tag', { tags: '["release-notes"]' }),
			row('i4', 'Untagged', { tags: '[]' })
		];
		render(TagPage);
		await screen.findByText('Exact');
		for (const other of ['Upper case', 'Longer tag', 'Untagged']) expect(screen.queryByText(other)).toBeNull();
		expect(screen.getByText('1 item')).toBeInTheDocument();
	});

	it('orders pinned first, then most recently updated', async () => {
		fake.rows = [
			row('old', 'Old', { updated_at: '2026-01-01T00:00:00Z' }),
			row('new', 'New', { updated_at: '2026-03-01T00:00:00Z' }),
			row('pin', 'Pinned but old', { pinned: true, updated_at: '2025-01-01T00:00:00Z' })
		];
		render(TagPage);
		await screen.findByText('New');
		const order = ['Pinned but old', 'New', 'Old'].map((t) => screen.getByText(t));
		for (let i = 1; i < order.length; i++) {
			expect(order[i - 1].compareDocumentPosition(order[i]) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
		}
	});

	it('follows the index: an item tagged after the page rendered appears without a reload', async () => {
		fake.rows = [row('i1', 'First')];
		render(TagPage);
		await screen.findByText('First');
		fake.rows = [...fake.rows, row('i2', 'Tagged later')];
		await screen.findByText('Tagged later');
	});

	it('waits for the collection list before listing, so the done field is the collection\'s', async () => {
		localStorage.setItem('pad-tag-completed-ws', 'hide');
		fake.collectionsFresh = false;
		fake.holdCollections = true;
		// By the default list `shipped` is open; only the collection's schema
		// says it is done. Judged before the list lands, it would be listed.
		fake.rows = [row('i1', 'Done thing', { fields: '{"stage":"shipped"}' }), row('i2', 'Open thing')];
		render(TagPage);
		await waitFor(() => expect(fake.ensures).toBe(1));
		expect(screen.queryByText('Done thing')).toBeNull();
		expect(screen.queryByText('Open thing')).toBeNull();
		releaseCollections();
		await screen.findByText('Open thing');
		expect(screen.queryByText('Done thing')).toBeNull();
	});
});

