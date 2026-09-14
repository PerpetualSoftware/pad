// BUG-3053 — grouping a list by a field whose value is `0` or `false` DROPS the
// item. No lane, no Uncategorized, nothing.
//
// The two passes disagree about what "no group" means. `displayGroups` tests the
// raw value for FALSINESS, so `0` and `false` are read as "ungrouped" and only
// the `''` lane is created for them. `propGroupData` buckets under the
// STRINGIFIED value, so those items land under `'0'` / `'false'` — keys no lane
// points at. Rendering reads `groupData['']`, which is empty.
//
// A falsiness test standing in for an emptiness test. `0` is an ordinary value
// for a number field (a count, a score, a version) and `false` is what half of
// every checkbox field holds.
//
// BoardView is NOT affected and that is not luck: `bucketByColumn` stringifies
// before testing, so `0` becomes the truthy `'0'` and lands in a real lane or in
// Uncategorized — visible either way. The conflation is specific to the view
// that DISCOVERS its groups from the items.
//
// Written before the fix, per CONVE-29, and confirmed RED against the unfixed
// tree — see the BUG-3053 trail for the run.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import type { Collection, Item } from '$lib/types';

vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: { findByIdOrSlug: () => null, getByCollection: () => [] },
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: { collections: [{ slug: 'cars' }] },
}));

import ListView from './ListView.svelte';

const FIELDS = [
	{ key: 'score', label: 'Score', type: 'number' },
	{ key: 'shipped', label: 'Shipped', type: 'checkbox' },
	{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'] },
];

function collection(): Collection {
	return {
		id: 'c1',
		workspace_id: 'ws1',
		name: 'Cars',
		slug: 'cars',
		icon: '',
		description: '',
		schema: JSON.stringify({ fields: FIELDS }),
		settings: JSON.stringify({}),
		sort_order: 0,
		is_default: true,
		is_system: false,
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
		prefix: 'CAR',
	} as unknown as Collection;
}

function item(id: string, fields: Record<string, unknown>): Item {
	return {
		id,
		workspace_id: 'ws1',
		collection_id: 'c1',
		slug: id,
		title: id,
		content: '',
		fields: JSON.stringify({ status: 'open', ...fields }),
		tags: '[]',
		status: 'open',
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as unknown as Item;
}

function renderList(
	items: Item[],
	groupField: string,
	// THE LANE WRITER (BUG-3068 renamed it from `onStatusChange`). Every leg in
	// this file is about a DROP, so every one of them binds here; the chip's
	// write is a different prop and is covered in the relation-group files.
	onLaneChange: (item: Item, value: string) => void = vi.fn(),
) {
	return render(ListView, {
		props: {
			items,
			collection: collection(),
			wsSlug: 'ws',
			groupField,
			statusOptions: ['open', 'done'],
			canEdit: true,
			onLaneChange,
		} as never,
	});
}

/** Every item title the view actually rendered, in any lane. */
function renderedTitles(container: HTMLElement): string[] {
	return [...container.querySelectorAll('.item-group')].flatMap((group) =>
		[...group.querySelectorAll('.card-title')].map((el) => el.textContent?.trim() ?? ''),
	);
}

/** The lane headings, in render order. */
function groupTitles(container: HTMLElement): string[] {
	return [...container.querySelectorAll('.group-title')].map((el) => el.textContent?.trim() ?? '');
}

afterEach(() => cleanup());

describe('an item whose group value is 0 or false still appears (BUG-3053)', () => {
	it('renders an item grouped by a number field holding 0', () => {
		const { container } = renderList(
			[item('car-zero', { score: 0 }), item('car-five', { score: 5 })],
			'score',
		);

		// The precondition: the view rendered SOMETHING, so an empty result below
		// is a dropped row rather than a harness that mounted nothing.
		expect(renderedTitles(container)).toContain('car-five');
		expect(renderedTitles(container), 'the 0-scored item must be somewhere').toContain(
			'car-zero',
		);
	});

	it('renders an item grouped by a checkbox field holding false', () => {
		const { container } = renderList(
			[item('car-no', { shipped: false }), item('car-yes', { shipped: true })],
			'shipped',
		);

		expect(renderedTitles(container)).toContain('car-yes');
		expect(renderedTitles(container), 'the unshipped item must be somewhere').toContain('car-no');
	});

	it('gives 0 its OWN lane rather than folding it into Uncategorized', () => {
		// Uncategorized means "this item has no value for the field". An item
		// scoring 0 has a value, and it is 0 — the distinction a reader of the
		// board is relying on.
		const { container } = renderList(
			[item('car-zero', { score: 0 }), item('car-none', {})],
			'score',
		);

		const titles = groupTitles(container);
		expect(titles).toContain('0');
		expect(titles).toContain('Uncategorized');
	});

	it('still folds a genuinely absent value into Uncategorized', () => {
		// The behaviour this must not break: absent, null and '' are the same
		// thing, and that thing is not 0.
		const { container } = renderList(
			[item('car-none', {}), item('car-null', { score: null }), item('car-blank', { score: '' })],
			'score',
		);

		expect(groupTitles(container)).toContain('Uncategorized');
		expect(renderedTitles(container)).toEqual(
			expect.arrayContaining(['car-none', 'car-null', 'car-blank']),
		);
		expect(groupTitles(container), 'no lane should be minted for an absent value').not.toContain(
			'null',
		);
	});
});

describe('dropping an item back into its own lane (codex round 1, finding 1)', () => {
	// Reachable only BECAUSE of this fix: before it, the 0-scored item was not
	// rendered at all, so it could not be dragged. `groupName` is a normalised
	// lane key and the handler compared it against the RAW field value, so
	// `0 !== '0'` read as a move and would have written the STRING '0' into a
	// number field.
	//
	// svelte-dnd-action dispatches a `finalize` CustomEvent and the component
	// binds `onfinalize` as a plain DOM handler, so the drop can be driven
	// directly without simulating pointer input.
	function finalizeOn(
		container: HTMLElement,
		laneIndex: number,
		items: Item[],
		movedId: string,
	) {
		const zones = container.querySelectorAll('.group-items');
		const zone = zones[laneIndex];
		expect(zone, 'precondition: the lane must be rendered and expanded').toBeTruthy();
		zone.dispatchEvent(
			new CustomEvent('finalize', {
				// `droppedIntoZone` is the library's own TRIGGERS.DROPPED_INTO_ZONE,
				// and the id names the item that MOVED — not the first in the list.
				// Getting that wrong makes the handler find no `originalItem` and
				// skip the comparison entirely, which is a green that measures
				// nothing.
				detail: { items, info: { trigger: 'droppedIntoZone', id: movedId } },
			}),
		);
	}

	it('does not rewrite a number field when the item did not change lane', async () => {
		const onLaneChange = vi.fn();
		const zero = item('car-zero', { score: 0 });
		const { container } = renderList([zero], 'score', onLaneChange);

		// Precondition: exactly one lane, and it is the 0 lane — so the drop below
		// really is a same-lane drop rather than a move we failed to notice.
		expect(groupTitles(container)).toEqual(['0']);

		finalizeOn(container, 0, [zero], 'car-zero');
		await Promise.resolve();

		expect(
			onLaneChange,
			'the item is already in the 0 lane; nothing changed, so nothing should be written',
		).not.toHaveBeenCalled();
	});

	it('does not rewrite a checkbox field when the item did not change lane', async () => {
		const onLaneChange = vi.fn();
		const no = item('car-no', { shipped: false });
		const { container } = renderList([no], 'shipped', onLaneChange);

		expect(groupTitles(container)).toEqual(['False']);

		finalizeOn(container, 0, [no], 'car-no');
		await Promise.resolve();

		expect(onLaneChange).not.toHaveBeenCalled();
	});

	it('STILL writes when the item really did change lane', async () => {
		// The counterfactual: without it the two assertions above would pass on an
		// implementation that never calls onLaneChange at all.
		const onLaneChange = vi.fn();
		const zero = item('car-zero', { score: 0 });
		const five = item('car-five', { score: 5 });
		const { container } = renderList([zero, five], 'score', onLaneChange);

		const titles = groupTitles(container);
		expect(titles).toEqual(['0', '5']);

		// Drop the 5-scored item into the 0 lane.
		finalizeOn(container, 0, [zero, five], 'car-five');
		await Promise.resolve();

		expect(onLaneChange).toHaveBeenCalledWith(
			expect.objectContaining({ id: 'car-five' }),
			'0',
		);
	});
});
