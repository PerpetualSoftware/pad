import { describe, expect, it, vi, afterEach } from 'vitest';
import { cleanup, render, waitFor } from '@testing-library/svelte';

/**
 * BUG-3481: the web half of BUG-2702. A workspace that RENAMED its conventions
 * or playbooks collection (a documented onboarding step) got a Library page
 * that listed nothing Active and hid Activate, because the page addressed both
 * collections by their default slugs. It now resolves them by the
 * `artifact_kind` trait the server already uses.
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

const listed = vi.hoisted(() => [] as string[]);
const gated = vi.hoisted(() => [] as string[]);
const collectionsFor = vi.hoisted(() => ({ list: [] as unknown[] }));

vi.mock('$lib/api/client', () => ({
	api: {
		// One entry each, so an Activate button renders and its gate is read.
		library: {
			get: vi.fn(async () => ({
				categories: [{ name: 'git', conventions: [{ title: 'Use conventional commits', description: 'd', content: 'b', category: 'git', trigger: 'on-commit', priority: 'should', surfaces: ['all'] }] }]
			})),
			getPlaybooks: vi.fn(async () => ({
				categories: [{ name: 'agent-workflows', playbooks: [{ title: 'Ship', description: 'd', content: 'b', category: 'agent-workflows', trigger: 'manual', scope: 'all', invocation_slug: 'ship', arguments: [] }] }]
			}))
		},
		items: {
			listByCollection: vi.fn(async (_ws: string, slug: string) => {
				listed.push(slug);
				return [];
			})
		},
		builtins: { list: vi.fn(async () => []) },
		collections: { list: vi.fn(async () => collectionsFor.list) }
	}
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
import LibraryPage from './+page.svelte';

function coll(slug: string, kind: string) {
	return {
		id: slug,
		slug,
		name: slug,
		workspace_id: 'w1',
		schema: '{"fields":[]}',
		settings: '{}',
		traits: JSON.stringify({ artifact_kind: { kind } })
	};
}

afterEach(() => {
	cleanup();
	listed.length = 0;
	gated.length = 0;
});

async function mountWith(collections: unknown[]) {
	collectionsFor.list = collections;
	await collectionStore.loadCollections('ws');
	page.params = { username: 'dave', workspace: 'ws' };
	page.url = new URL('http://localhost/dave/ws/library');
	render(LibraryPage);
}

describe('the Library page addresses the conventions and playbooks collections by trait', () => {
	it('lists and gates a RENAMED pair by their current slugs', async () => {
		await mountWith([coll('rules', 'convention'), coll('procedures', 'playbook')]);
		await waitFor(() => {
			if (!listed.includes('rules') || !listed.includes('procedures')) throw new Error(`listed ${listed}`);
		});
		expect(listed).not.toContain('conventions');
		expect(listed).not.toContain('playbooks');
		await waitFor(() => {
			if (!gated.includes('rules')) throw new Error(`gated ${gated}`);
		});
		expect(gated).not.toContain('conventions');
	});

	it('CONTROL: the default slugs are used, once, when nothing was renamed', async () => {
		await mountWith([coll('conventions', 'convention'), coll('playbooks', 'playbook')]);
		await waitFor(() => {
			if (!listed.includes('conventions')) throw new Error(`listed ${listed}`);
		});
		expect(listed.filter((s) => s === 'conventions')).toHaveLength(1);
	});
});
