// BUG-3053, lead review of PR #1348 — the FIFTH consumer of the lane-key pair,
// and the one I had said I checked.
//
// `commitColumnMove` compared the RAW field value against the lane string:
//
//     const currentValue = isRelationGroup ? relationLaneValueFor(…) : fields[groupField];
//     if (!groupingRefusal && currentValue !== targetColumn) { … }
//
// So an item scoring 0, dropped back into its own lane, reads as a move —
// `0 !== '0'` — and the "move" writes the STRING '0' into a number field.
//
// This one is LIVE ON MAIN, unlike the identical line in ListView. BoardView has
// always bucketed through `laneValue` (IDEA-2275), so a 0-scored card has always
// been visible in lane '0' and has always been draggable. The list's copy was
// unreachable only because the item was dropped from the view entirely, which is
// what made it arrive with that fix rather than before it.
//
// svelte-dnd-action dispatches a `finalize` CustomEvent and the component binds
// `onfinalize` as a plain DOM handler, so the drop is driven directly.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import type { Collection, Item } from '$lib/types';

vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: { findByIdOrSlug: () => null, getByCollection: () => [] },
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: { collections: [{ slug: 'cars' }] },
}));

import BoardView from './BoardView.svelte';

const FIELDS = [
	{ key: 'score', label: 'Score', type: 'number', options: ['0', '5'] },
	{ key: 'shipped', label: 'Shipped', type: 'checkbox', options: ['true', 'false'] },
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
		fields: JSON.stringify(fields),
		tags: '[]',
		status: 'open',
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as unknown as Item;
}

function renderBoard(
	items: Item[],
	groupField: string,
	onStatusChange: (item: Item, value: string) => void,
) {
	return render(BoardView, {
		props: {
			items,
			collection: collection(),
			wsSlug: 'ws',
			groupField,
			canEdit: true,
			onStatusChange,
		} as never,
	});
}

/** Lane headings, in render order. */
function laneTitles(container: HTMLElement): string[] {
	return [...container.querySelectorAll('.column-name')].map((el) => el.textContent?.trim() ?? '');
}

/**
 * Dispatch the library's real `finalize` event on a lane's drop zone.
 *
 * `movedId` names the card that MOVED, not the first in the list — getting that
 * wrong makes the handler find no `originalItem` and skip the comparison
 * entirely, which is a green that measures nothing. That exact fault made two
 * legs vacuous in the ListView half of this unit.
 */
function finalizeOnLane(container: HTMLElement, laneIndex: number, items: Item[], movedId: string) {
	const zones = container.querySelectorAll('.column-cards');
	const zone = zones[laneIndex];
	expect(zone, `lane ${laneIndex} should have a drop zone`).toBeTruthy();
	zone.dispatchEvent(
		new CustomEvent('finalize', {
			detail: { items, info: { trigger: 'droppedIntoZone', id: movedId } },
		}),
	);
}

afterEach(() => cleanup());

describe('dropping a card back into its own lane writes nothing (BUG-3053)', () => {
	it('does not rewrite a number field when the card did not change lane', async () => {
		const onStatusChange = vi.fn();
		const zero = item('car-zero', { score: 0 });
		const { container } = renderBoard([zero], 'score', onStatusChange);

		// Precondition, and the thing that makes this different from the list: the
		// card IS on the board, in the '0' lane, today.
		expect(laneTitles(container)).toContain('0');

		const laneIndex = laneTitles(container).indexOf('0');
		finalizeOnLane(container, laneIndex, [zero], 'car-zero');
		await Promise.resolve();

		expect(
			onStatusChange,
			'already in the 0 lane; nothing moved, so nothing should be written',
		).not.toHaveBeenCalled();
	});

	it('does not rewrite a checkbox field when the card did not change lane', async () => {
		const onStatusChange = vi.fn();
		const no = item('car-no', { shipped: false });
		const { container } = renderBoard([no], 'shipped', onStatusChange);

		const titles = laneTitles(container);
		expect(titles).toContain('False');

		finalizeOnLane(container, titles.indexOf('False'), [no], 'car-no');
		await Promise.resolve();

		expect(onStatusChange).not.toHaveBeenCalled();
	});

	it('STILL writes when the card really did change lane', async () => {
		// The counterfactual. Without it the two assertions above pass on an
		// implementation that never calls onStatusChange at all — which is exactly
		// how two legs in the ListView half of this unit came to measure nothing.
		const onStatusChange = vi.fn();
		const zero = item('car-zero', { score: 0 });
		const five = item('car-five', { score: 5 });
		const { container } = renderBoard([zero, five], 'score', onStatusChange);

		const titles = laneTitles(container);
		expect(titles).toContain('0');
		expect(titles).toContain('5');

		// Drop the 5-scored card into the 0 lane.
		finalizeOnLane(container, titles.indexOf('0'), [zero, five], 'car-five');
		await Promise.resolve();

		expect(onStatusChange).toHaveBeenCalledWith(expect.objectContaining({ id: 'car-five' }), '0');
	});
});
