import { describe, expect, it, vi, afterEach } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/svelte';

/**
 * BUG-3481: the Playbooks page listed, fetched and created through the
 * default `playbooks` slug, so a RENAMED playbooks collection showed an empty
 * page with Create and Import hidden. It now resolves the collection by its
 * artifact_kind trait, and a load for the slug it superseded cannot land over
 * the newer one (codex r1).
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

type Pending = { slug: string; resolve: (v: unknown) => void };
const lists = vi.hoisted(() => [] as Pending[]);
const gets = vi.hoisted(() => [] as string[]);
const gated = vi.hoisted(() => [] as string[]);
const collectionsFor = vi.hoisted(() => ({ list: [] as unknown[] }));

vi.mock('$lib/api/client', () => ({
	api: {
		items: {
			listByCollection: vi.fn(
				(_ws: string, slug: string) => new Promise((resolve) => lists.push({ slug, resolve }))
			)
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
vi.mock('$lib/collections/canCreateIn', () => ({
	canCreateIn: (slug: string) => {
		gated.push(slug);
		return true;
	}
}));
vi.mock('$lib/scroll/restore.svelte', () => ({
	createScrollRestoration: () => ({ snapshot: { capture: () => null, restore: () => {} } })
}));

import { page } from '$app/state';
import { collectionStore } from '$lib/stores/collections.svelte';
import PlaybooksPage from './+page.svelte';

function coll(slug: string, kind: string) {
	return { id: slug, slug, name: slug, workspace_id: 'w1', schema: '{"fields":[]}', settings: '{}', traits: JSON.stringify({ artifact_kind: { kind } }) };
}
function playbook(title: string) {
	return { id: title, slug: title, title, content: '', fields: '{"status":"active"}', collection_slug: 'x' };
}

afterEach(() => {
	cleanup();
	lists.length = 0;
	gets.length = 0;
	gated.length = 0;
});

describe('the Playbooks page addresses its collection by trait', () => {
	it('lists, fetches and gates a RENAMED collection by its current slug', async () => {
		collectionsFor.list = [coll('procedures', 'playbook'), coll('rules', 'convention')];
		await collectionStore.loadCollections('ws2');
		page.params = { username: 'dave', workspace: 'ws2' };
		page.url = new URL('http://localhost/dave/ws2/playbooks');
		render(PlaybooksPage);
		await waitFor(() => {
			if (!lists.some((l) => l.slug === 'procedures')) throw new Error(`lists ${lists.map((l) => l.slug)}`);
		});
		expect(lists.map((l) => l.slug)).not.toContain('playbooks');
		expect(gets).toContain('procedures');
		// Create and Import render once the list has loaded; then their gate
		// is read, by the current slug.
		lists.find((l) => l.slug === 'procedures')!.resolve([]);
		await waitFor(() => {
			if (!gated.includes('procedures')) throw new Error(`gated ${gated}`);
		});
		expect(gated).not.toContain('playbooks');
	});

	it('a load for the default slug that lands AFTER the renamed one does not overwrite it', async () => {
		// Mount before the store knows the workspace: the first load goes to
		// the default slug. Then the rename resolves and a second load starts.
		page.params = { username: 'dave', workspace: 'ws3' };
		page.url = new URL('http://localhost/dave/ws3/playbooks');
		const { container } = render(PlaybooksPage);
		await waitFor(() => {
			if (!lists.some((l) => l.slug === 'playbooks')) throw new Error('no default-slug load yet');
		});
		collectionsFor.list = [coll('procedures', 'playbook')];
		await collectionStore.loadCollections('ws3');
		await waitFor(() => {
			if (!lists.some((l) => l.slug === 'procedures')) throw new Error('no renamed-slug load yet');
		});
		lists.find((l) => l.slug === 'procedures')!.resolve([playbook('Renamed Ship')]);
		await waitFor(() => {
			if (!container.textContent?.includes('Renamed Ship')) throw new Error('renamed list not rendered');
		});
		lists.find((l) => l.slug === 'playbooks')!.resolve([playbook('Stale Default')]);
		await new Promise((r) => setTimeout(r, 20));
		expect(container.textContent).toContain('Renamed Ship');
		expect(container.textContent).not.toContain('Stale Default');
	});
});
