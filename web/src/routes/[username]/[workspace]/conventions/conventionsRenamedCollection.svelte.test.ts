import { describe, expect, it, vi, afterEach } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';

/**
 * BUG-3481: the Conventions page listed, fetched and CREATED through the
 * default `conventions` slug, so a renamed conventions collection showed an
 * empty page and a create posted to a collection that no longer exists. It now
 * addresses the collection that carries the convention artifact_kind trait.
 */
const auth = vi.hoisted(() => ({
	get identityEpoch() { return 0; },
	get userId() { return 'u1'; },
	get user() { return { id: 'u1', name: 'A', email: 'a@example.com' }; },
	identityFence() { return () => true; },
	onIdentityChange() { return () => {}; },
	clear() {}
}));
vi.mock('$lib/stores/auth.svelte', () => ({ authStore: auth }));

const lists = vi.hoisted(() => [] as string[]);
const gets = vi.hoisted(() => [] as string[]);
const creates = vi.hoisted(() => [] as string[]);
const collectionsFor = vi.hoisted(() => ({ list: [] as unknown[] }));
vi.mock('$lib/api/client', () => ({
	api: {
		items: {
			listByCollection: vi.fn(async (_ws: string, slug: string) => {
				lists.push(slug);
				return [];
			}),
			create: vi.fn(async (_ws: string, slug: string, data: { title: string }) => {
				creates.push(slug);
				return { id: 'n1', slug: 'n1', title: data.title, content: '', fields: '{}', collection_slug: slug };
			})
		},
		collections: {
			list: vi.fn(async () => collectionsFor.list),
			get: vi.fn(async (_ws: string, slug: string) => {
				gets.push(slug);
				return { id: slug, slug, name: slug, schema: '{"fields":[]}', settings: '{}' };
			})
		}
	},
	isPlanLimitError: () => false
}));
vi.mock('$lib/collections/canCreateIn', () => ({ canCreateIn: () => true }));
vi.mock('$lib/scroll/restore.svelte', () => ({
	createScrollRestoration: () => ({ snapshot: { capture: () => null, restore: () => {} } })
}));

import { page } from '$app/state';
import { collectionStore } from '$lib/stores/collections.svelte';
import ConventionsPage from './+page.svelte';

function coll(slug: string, kind: string) {
	return { id: slug, slug, name: slug, workspace_id: 'w1', schema: '{"fields":[]}', settings: '{}', traits: JSON.stringify({ artifact_kind: { kind } }) };
}

afterEach(() => {
	cleanup();
	lists.length = 0;
	gets.length = 0;
	creates.length = 0;
});

describe('the Conventions page addresses its collection by trait', () => {
	it('lists, fetches and creates in a RENAMED collection by its current slug', async () => {
		collectionsFor.list = [coll('rules', 'convention'), coll('procedures', 'playbook')];
		await collectionStore.loadCollections('ws6');
		page.params = { username: 'dave', workspace: 'ws6' };
		page.url = new URL('http://localhost/dave/ws6/conventions');
		render(ConventionsPage);
		await waitFor(() => {
			if (!lists.includes('rules')) throw new Error(`lists ${lists}`);
		});
		expect(lists).not.toContain('conventions');
		expect(gets).toContain('rules');
		expect(gets).not.toContain('conventions');

		await fireEvent.click(await screen.findByRole('button', { name: '+ New Convention' }));
		const title = screen.getByPlaceholderText('Convention title...') as HTMLInputElement;
		await fireEvent.input(title, { target: { value: 'Run make test' } });
		await fireEvent.submit(title.form!);
		await waitFor(() => {
			if (creates.length === 0) throw new Error('no create');
		});
		expect(creates).toEqual(['rules']);
	});
});
