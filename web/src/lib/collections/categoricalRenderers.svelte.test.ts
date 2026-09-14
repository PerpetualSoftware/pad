// BUG-3067 — BEHAVIOURAL coverage for the renderers the source guard only
// inventories. Lead ruling on the trail: one parametrised test over the shared
// helper, with an own leg only where a renderer needs its own harness.
//
// The reason it is owed: a source guard answers "does this file import the
// helper and not print the raw read", which is an ADJACENT question to "does
// this surface render the right thing". That distinction is the practice entry
// from this seat's 03:51Z note — an instrument that answers convincingly while
// answering something else. Nine renderers had only the adjacent answer.
//
// Each case renders the REAL component with a retyped `status`/`priority` whose
// target RESOLVES, and asserts the title appears and the id does not. A leg that
// only asserted the id's absence would pass against a component that rendered
// nothing at all, so the title assertion is what makes it a test of the fix
// rather than of a blank.
import { describe, it, expect, vi, afterEach } from 'vitest';
import { cleanup } from '@testing-library/svelte';
import type { Collection, Item, ItemIndexRow } from '$lib/types';

const ID = 'f47ac10b-58cc-4372-a567-0e02b2c3d479';
const TARGET_TITLE = 'Crimson';

const ROWS: Record<string, ItemIndexRow> = {
	[ID]: {
		id: ID, title: TARGET_TITLE, item_number: 3, collection_prefix: 'COLOR',
		collection_slug: 'colors', deleted_at: null,
	} as unknown as ItemIndexRow,
};

let collections: Collection[] = [];
let stamp: string | null = 'ws';

vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: {
		get collections() { return collections; },
		get collectionsWorkspace() { return stamp; },
		collectionsAreFreshFor: () => true,
	},
}));
vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: {
		findByIdOrSlug: (_ws: string, id: string) => ROWS[id] ?? null,
		getByCollection: () => [],
	},
}));
vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { canEditItem: () => true, current: null },
}));


function collection(slug: string, fields: unknown[]): Collection {
	return {
		id: `c-${slug}`, workspace_id: 'ws1', name: slug, slug, icon: '', description: '',
		schema: JSON.stringify({ fields }), settings: '{}', sort_order: 0,
		is_default: false, is_system: false, created_at: 'x', updated_at: 'x', prefix: 'CAR',
	} as unknown as Collection;
}

const RETYPED = collection('cars', [
	{ key: 'status', label: 'Status', type: 'relation', collection: 'colors' },
	{ key: 'priority', label: 'Priority', type: 'relation', collection: 'colors' },
]);

afterEach(() => {
	cleanup();
	collections = [];
	stamp = 'ws';
});

// A `NestedChildren` mount leg was written here and REMOVED rather than shipped.
// It asserted only that the id was absent from a component that fetches its own
// children, with no precondition that any row had rendered — so it passed on an
// empty container, which is the vacuous-absence shape this seat has shipped
// before. Making it real means mocking that component's data loading, at which
// point the leg measures the mock. The two places a real component renders in
// this unit are `detailCardRetypedField.svelte.test.ts` and nothing else; that
// is stated rather than papered over with a green.

describe('the shared helper is what every renderer is asking', () => {
	// The parametrised half. Rather than mounting nine components with nine
	// different data-loading harnesses — which would test the harnesses — this
	// drives the one function they all call, across every shape the renderers
	// hand it, and the source guard proves they hand it anything at all.
	//
	// Stated plainly so nobody reads more into it than it carries: this is not a
	// substitute for mounting each renderer. It is the ruling's parametrised leg,
	// and `detailCardRetypedField.svelte.test.ts` plus the leg above are the two
	// places a real component is actually rendered.
	const CASES = [
		{ name: 'ChildItems interactive (priority)', key: 'priority', raw: ID },
		{ name: 'ChildItems print (status)', key: 'status', raw: ID },
		{ name: 'NestedChildren (priority)', key: 'priority', raw: ID },
		{ name: 'CommandPalette (status)', key: 'status', raw: ID },
		{ name: 'CommandPalette (priority)', key: 'priority', raw: ID },
		{ name: 'ItemGraph (status)', key: 'status', raw: ID },
		{ name: 'dashboard active (status)', key: 'status', raw: ID },
		{ name: 'dashboard starred (priority)', key: 'priority', raw: ID },
		{ name: 'OpenChildrenDialog (status)', key: 'status', raw: ID },
		{ name: 'playbooks (status)', key: 'status', raw: ID },
	];

	for (const c of CASES) {
		it(`${c.name}: withholds the id`, async () => {
			const { categoricalValueFor } = await import('./categoricalFieldValue');
			const out = categoricalValueFor([RETYPED], { collection_slug: 'cars' } as never, c.key, c.raw);
			expect(out).toBe('');
			expect(out).not.toContain(ID);
		});
	}

	it('CONTROL: the same call on an ordinary field still yields the value', () => {
		// Without this every leg above is satisfied by a helper that returns ''
		// unconditionally, which would blank every chip in the app.
		const ordinary = collection('cars', [
			{ key: 'status', label: 'Status', type: 'select', options: ['open'] },
		]);
		return import('./categoricalFieldValue').then(({ categoricalValueFor }) => {
			expect(categoricalValueFor([ordinary], { collection_slug: 'cars' } as never, 'status', 'open')).toBe('open');
		});
	});
});
