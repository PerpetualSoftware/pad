// BUG-3208 — board lanes named after an Object.prototype member.
//
// Two records on the board were keyed by a raw option VALUE:
//
// 1. The lane buckets (`bucketByColumn` → `columnData`). An option named
//    `__proto__` did not create a lane: the assignment RE-PARENTED the record
//    onto that lane's array. The card still rendered, because the read went
//    through the prototype and found the array — which is why a leg asserting
//    "the card is in its lane" passes on the defect. What broke is that
//    `$state` proxies only objects whose prototype is Object.prototype, so the
//    whole record was left unproxied and no lane on the board re-rendered on a
//    drag. The drag leg below is the discriminating one.
// 2. The per-lane sort overrides. A bare `overrides[value] ?? sortMode` finds
//    the INHERITED member for every Object.prototype name, so a lane named
//    `constructor` read a function as its override and ignored the page sort.
//
// svelte-dnd-action dispatches `consider` / `finalize` CustomEvents and the
// component binds them as plain DOM handlers, so a drag is driven directly.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';
import type { Collection, Item } from '$lib/types';

vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: { findByIdOrSlug: () => null, getByCollection: () => [] },
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: { collections: [{ slug: 'cars' }] },
}));

import BoardView from './BoardView.svelte';

function collection(options: string[]): Collection {
	return {
		id: 'c1',
		workspace_id: 'ws1',
		name: 'Cars',
		slug: 'cars',
		icon: '',
		description: '',
		schema: JSON.stringify({ fields: [{ key: 'stage', label: 'Stage', type: 'select', options }] }),
		settings: JSON.stringify({}),
		sort_order: 0,
		is_default: true,
		is_system: false,
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
		prefix: 'CAR',
	} as unknown as Collection;
}

function item(id: string, stage: string, sortOrder = 0): Item {
	return {
		id,
		workspace_id: 'ws1',
		collection_id: 'c1',
		slug: id,
		title: id,
		content: '',
		fields: JSON.stringify({ stage }),
		tags: '[]',
		sort_order: sortOrder,
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as unknown as Item;
}

function renderBoard(options: string[], items: Item[], sortMode = 'manual') {
	return render(BoardView, {
		props: {
			items,
			collection: collection(options),
			wsSlug: 'ws',
			groupField: 'stage',
			canEdit: true,
			sortMode,
			onLaneChange: vi.fn(),
		} as never,
	});
}

function laneTitles(container: HTMLElement): string[] {
	return [...container.querySelectorAll('.column-name')].map((el) => el.textContent?.trim() ?? '');
}

/** The card ids rendered in a lane's drop zone, in order. */
function cardsIn(container: HTMLElement, laneIndex: number): string[] {
	const zone = container.querySelectorAll('.column-cards')[laneIndex];
	return [...zone.querySelectorAll('.card-wrapper')].map(
		(el) => el.querySelector('[data-item-id]')?.getAttribute('data-item-id') ?? el.textContent?.trim() ?? '',
	);
}

function laneIndex(container: HTMLElement, title: string): number {
	const i = laneTitles(container).indexOf(title);
	expect(i, `lane "${title}" should render; lanes are ${JSON.stringify(laneTitles(container))}`).toBeGreaterThanOrEqual(0);
	return i;
}

afterEach(() => cleanup());

describe('an option named __proto__ does not un-proxy the board (BUG-3208)', () => {
	it('re-renders a drag in another lane when one lane is named __proto__', async () => {
		const a = item('car-a', 'open');
		const p = item('car-p', '__proto__');
		const { container } = renderBoard(['__proto__', 'open'], [a, p]);

		const open = laneIndex(container, 'Open');
		expect(cardsIn(container, open)).toHaveLength(1);

		// Drag the only card out of "open": the library's consider event hands
		// the lane its new (empty) list. A proxied record re-renders; the
		// re-parented one did not, and the card stayed on screen.
		container.querySelectorAll('.column-cards')[open].dispatchEvent(
			new CustomEvent('consider', {
				detail: { items: [], info: { trigger: 'dragStarted', id: 'car-a' } },
			}),
		);
		await tick();

		expect(cardsIn(container, open), 'the drag should re-render the lane').toHaveLength(0);
	});

	it('control: the same drag re-renders when no lane is named __proto__', async () => {
		// Without this the leg above could pass on a harness where consider never
		// re-renders anything, and fail on the fix for the same reason.
		const a = item('car-a', 'open');
		const { container } = renderBoard(['closed', 'open'], [a]);

		const open = laneIndex(container, 'Open');
		container.querySelectorAll('.column-cards')[open].dispatchEvent(
			new CustomEvent('consider', {
				detail: { items: [], info: { trigger: 'dragStarted', id: 'car-a' } },
			}),
		);
		await tick();

		expect(cardsIn(container, open)).toHaveLength(0);
	});
});

describe('a lane named after an Object.prototype member follows the page sort (BUG-3208)', () => {
	// Manual order (sort_order) is z-car, then a-car; title order is the reverse.
	for (const name of ['constructor', 'toString', '__proto__']) {
		it(`sorts the "${name}" lane by the page-wide title sort`, () => {
			const z = item('z-car', name, 0);
			const a = item('a-car', name, 1);
			const { container } = renderBoard([name, 'open'], [z, a], 'title');

			const lane = [...container.querySelectorAll('.column-cards')].findIndex(
				(zone) => zone.querySelectorAll('.card-wrapper').length === 2,
			);
			expect(lane, 'the lane holding both cards should render').toBeGreaterThanOrEqual(0);
			const order = [...container.querySelectorAll('.column-cards')[lane].querySelectorAll('.card-wrapper')].map(
				(card) => (card.textContent?.includes('a-car') ? 'a-car' : card.textContent?.includes('z-car') ? 'z-car' : '?'),
			);
			expect(order, 'title sort puts a-car first').toEqual(['a-car', 'z-car']);
		});
	}
});
