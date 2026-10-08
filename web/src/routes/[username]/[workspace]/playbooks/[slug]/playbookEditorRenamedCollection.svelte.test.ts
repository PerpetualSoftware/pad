import { describe, expect, it, vi, afterEach } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/svelte';

/**
 * BUG-3481: the playbook editor refused any item outside the literal
 * `playbooks` collection, so every playbook in a RENAMED playbooks collection
 * opened as "Not a playbook". It now compares against the collection that
 * carries the playbook artifact_kind trait, loading the workspace's
 * collections first so the check never runs on the default by accident.
 */
const auth = vi.hoisted(() => ({
	get identityEpoch() { return 0; },
	get userId() { return 'u1'; },
	identityFence() { return () => true; },
	onIdentityChange() { return () => {}; },
}));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));
vi.mock('$lib/stores/workspace.svelte', () => ({ workspaceStore: { canEditItem: () => true } }));

const toasts = vi.hoisted(() => [] as string[]);
vi.mock('$lib/stores/toast.svelte', () => ({
	toastStore: { show: (m: string) => { toasts.push(m); return 'id'; }, dismiss: () => {}, get toasts() { return []; } },
	quietExternalToasts: () => false,
}));

const loadedItem = vi.hoisted(() => ({ current: null as unknown }));
const collectionsFor = vi.hoisted(() => ({ list: [] as unknown[] }));
vi.mock('$lib/api/client', () => ({
	PadApiError: class extends Error {},
	api: {
		items: {
			get: vi.fn(async () => loadedItem.current),
			listByCollection: vi.fn(async () => []),
		},
		collections: {
			list: vi.fn(async () => collectionsFor.list),
			get: vi.fn(async (_ws: string, slug: string) => ({ id: slug, slug, schema: '{"fields":[]}' })),
		},
	},
}));

import { page } from '$app/state';
import PlaybookEditor from './+page.svelte';

function coll(slug: string, kind?: string) {
	return {
		id: slug, slug, name: slug, workspace_id: 'w1', schema: '{"fields":[]}', settings: '{}',
		traits: kind ? JSON.stringify({ artifact_kind: { kind } }) : '{}',
	};
}
function item(collection: string) {
	return {
		id: 'p1', slug: 'ship-it', title: 'Ship it', content: 'body', collection_slug: collection,
		collection_prefix: 'PROC', item_number: 7, seq: 1,
		fields: JSON.stringify({ status: 'active', trigger: 'manual', scope: 'all' }),
	};
}

afterEach(() => {
	cleanup();
	toasts.length = 0;
});

describe('the playbook editor accepts a playbook by its collection trait', () => {
	it('opens a playbook in a RENAMED playbooks collection', async () => {
		// The store has NOT loaded ws4 yet: the editor must load it before the check.
		collectionsFor.list = [coll('procedures', 'playbook')];
		loadedItem.current = item('procedures');
		page.params = { username: 'dave', workspace: 'ws4', slug: 'PROC-7' };
		page.url = new URL('http://localhost/dave/ws4/playbooks/PROC-7');
		render(PlaybookEditor);
		await waitFor(() => screen.getByPlaceholderText('Playbook title'));
		expect(toasts.filter((t) => t.startsWith('Not a playbook'))).toEqual([]);
	});

	it('CONTROL: still refuses an item from a collection without the trait', async () => {
		collectionsFor.list = [coll('procedures', 'playbook'), coll('playbooks')];
		loadedItem.current = item('playbooks');
		page.params = { username: 'dave', workspace: 'ws5', slug: 'PROC-7' };
		page.url = new URL('http://localhost/dave/ws5/playbooks/PROC-7');
		render(PlaybookEditor);
		await waitFor(() => {
			if (!toasts.some((t) => t.startsWith('Not a playbook'))) throw new Error(`toasts ${toasts}`);
		});
		expect(screen.queryByPlaceholderText('Playbook title')).toBeNull();
	});
});
