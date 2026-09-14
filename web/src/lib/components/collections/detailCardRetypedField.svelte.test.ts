// BUG-3067 — the behavioural half for the graph detail card.
//
// The source guard in `schemalessReaders.test.ts` proves every surface in the
// class routes through the shared question; it cannot prove a surface renders
// the ANSWER correctly. This drives one surface end to end, chosen because it
// reads BOTH keys and reads them from two different places — `priority` from the
// fetched item's fields, `status` from the graph NODE, which is a server
// projection that arrives before the item does.
import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import type { Collection, Item } from '$lib/types';

const ID = 'f47ac10b-58cc-4372-a567-0e02b2c3d479';

let collections: Collection[] = [];
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: {
		get collections() {
			return collections;
		},
	},
}));

import DetailCard from '../../../routes/[username]/[workspace]/graph/DetailCard.svelte';

function collection(slug: string, fields: unknown[]): Collection {
	return {
		id: `c-${slug}`, workspace_id: 'ws1', name: slug, slug, icon: '', description: '',
		schema: JSON.stringify({ fields }), settings: '{}', sort_order: 0,
		is_default: false, is_system: false, created_at: 'x', updated_at: 'x', prefix: 'X',
	} as unknown as Collection;
}

const ORDINARY = collection('tasks', [
	{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'] },
	{ key: 'priority', label: 'Priority', type: 'select', options: ['low', 'high'] },
]);
const RETYPED = collection('cars', [
	{ key: 'status', label: 'Status', type: 'relation', collection: 'colors' },
	{ key: 'priority', label: 'Priority', type: 'relation', collection: 'colors' },
]);

function renderCard(collSlug: string, storedStatus: string, storedPriority: string) {
	const item = {
		id: 'i1', workspace_id: 'ws1', collection_id: 'c1', slug: 'i1', title: 'An item',
		content: '', fields: JSON.stringify({ status: storedStatus, priority: storedPriority }),
		tags: '[]', collection_slug: collSlug, created_at: 'x', updated_at: 'x',
	} as unknown as Item;
	return render(DetailCard, {
		props: {
			node: {
				ref: 'CAR-1', title: 'An item', collection: collSlug, status: storedStatus,
				is_terminal: false, child_count: 0, updated_at: '2026-01-01T00:00:00Z',
			},
			color: '#888', item, itemLoading: false,
			blockedBy: [], blocksCount: 0, chainDepth: 0,
			onjump: vi.fn(), onopen: vi.fn(), onclose: vi.fn(),
		} as never,
	});
}

afterEach(() => {
	cleanup();
	collections = [];
});

describe('the graph detail card and a retyped field', () => {
	it('prints neither id when `status` and `priority` are relations', () => {
		collections = [ORDINARY, RETYPED];
		const screen = renderCard('cars', ID, ID);
		// PRECONDITION: the card rendered, so "no id" is not "no card".
		expect(screen.container.querySelector('.detail-card')).not.toBeNull();
		expect(screen.container.textContent).toContain('An item');
		expect(screen.container.textContent).not.toContain(ID);
	});

	it('CONTROL: an ordinary collection still shows both', () => {
		// Without this, withholding every value would satisfy the leg above while
		// blanking the card for every workspace that never retyped anything.
		collections = [ORDINARY, RETYPED];
		const screen = renderCard('tasks', 'open', 'high');
		expect(screen.container.textContent).toContain('open');
		expect(screen.container.textContent).toContain('high');
	});

	it('withholds both while the collections are still loading', () => {
		// The fail-closed case the helper documents: an unloaded store cannot
		// vouch for a value, and a momentarily missing chip is recoverable where a
		// printed id is the bug.
		collections = [];
		const screen = renderCard('tasks', 'open', 'high');
		expect(screen.container.querySelector('.detail-card')).not.toBeNull();
		expect(screen.container.textContent).not.toContain('open');
		expect(screen.container.textContent).not.toContain('high');
	});
});
