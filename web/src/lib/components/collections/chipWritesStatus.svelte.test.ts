// BUG-3068 — a card's status chip wrote into the GROUP field, not `status`.
//
// `ListView` and `BoardView` each took ONE `onStatusChange` prop and handed it to
// two consumers with different meanings: the drop handler (which means "put this
// item in this lane", naming the GROUP field) and `ItemCard`'s status chip (which
// means "set this item's status"). The page's writer defaults its target to
// `groupField`, so the chip's value went wherever the board was grouped.
//
// The TABLE arm of this was fixed in BUG-3057 (#1353) by naming `status` at the
// call site — it could, because its chip is its only consumer of the prop. These
// two needed the prop split, which is what BUG-3068 did: `onLaneChange` for the
// drop, `onStatusChange` for the chip.
//
// THE TWO VIEWS FAILED DIFFERENTLY and the difference is why this file tests
// each rather than parameterising one:
//
//   - ListView was handed REAL status options (a prop from the page). Grouped by
//     `priority`, the chip cycled statuses correctly and delivered the result to
//     `fields.priority` — a status word in the priority field.
//   - BoardView was handed its LANES as status options (`statusOptions={columns}`).
//     Grouped by `priority`, `ItemCard` looks up `statusOptions.indexOf(fields.status)`
//     — a status in a list of priorities — which MISSES, returning -1 and making
//     the next index 0 unconditionally. Every chip click therefore wrote the
//     FIRST LANE into `fields.priority`, from wherever the card was, while the
//     chip's label went on showing the status it was not changing. That write is
//     type-coherent, so BUG-3057's conversion accepted it silently.
//
// WHICH LEGS ACTUALLY DISCRIMINATE, measured against the unfixed tree (the
// components restored from 9e121bde with this file unchanged): 3 of the 8 go red.
//
//   RED   the board's `cycles the STATUS field options` leg — `expected 'low' to
//         be 'done'`, i.e. the chip wrote the first LANE, which is the -1 defect
//         above observed directly.
//   RED   both `the DROP writes through a different callback` legs — the unfixed
//         component does not declare `onLaneChange`, so the drop goes to
//         `onStatusChange` and the lane mock is never called.
//   GREEN the two list chip legs, the board's `never reaches the lane writer`
//         leg, and the two controls.
//
// The greens are not weak legs; they are legs a component-level mock cannot make
// red, and saying so is the point. The list's defect is a DESTINATION defect: the
// chip always reached the callback the test names `onStatusChange` and always
// carried the right status. What made it a bug is that the page pointed that same
// callback at `groupField`, because the drop shared it. A mock cannot observe a
// property of the caller's wiring. The drop legs are what make it observable
// here, and `laneKeyCallers.test.ts` asserts the page's half.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup, fireEvent } from '@testing-library/svelte';
import type { Collection, Item } from '$lib/types';

vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: { findByIdOrSlug: () => null, getByCollection: () => [] },
}));
vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: { collections: [{ slug: 'cars' }] },
}));

import ListView from './ListView.svelte';
import BoardView from './BoardView.svelte';

/**
 * THREE statuses, and the item sits on the MIDDLE one. Both halves are
 * load-bearing rather than incidental:
 *
 *  - three, because with two options every wrong answer is also the right one:
 *    cycling from `open` in `['open','done']` gives `done`, and so does the
 *    board's broken -1 → index 0 path if `done` happens to be first.
 *  - the middle one, because the -1 defect writes index 0. An item already on
 *    the first status cannot distinguish "cycled correctly" from "reset to the
 *    first entry".
 */
const STATUSES = ['open', 'in_progress', 'done'];
const PRIORITIES = ['low', 'high'];

const FIELDS = [
	{ key: 'status', label: 'Status', type: 'select', options: STATUSES },
	{ key: 'priority', label: 'Priority', type: 'select', options: PRIORITIES },
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

/** An item in the MIDDLE status and a named priority — see STATUSES above. */
function item(id: string, priority = 'high'): Item {
	return {
		id,
		workspace_id: 'ws1',
		collection_id: 'c1',
		slug: id,
		title: id,
		content: '',
		fields: JSON.stringify({ status: 'in_progress', priority }),
		tags: '[]',
		status: 'in_progress',
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as unknown as Item;
}

afterEach(() => {
	cleanup();
});

function chipIn(container: HTMLElement): HTMLElement {
	const chip = container.querySelector('[title="Click to cycle status"]');
	expect(chip, 'no status chip rendered — this test cannot discriminate without one').not.toBeNull();
	return chip as HTMLElement;
}

describe('a status chip on a list grouped by a NON-status field', () => {
	function renderList() {
		const onLaneChange = vi.fn();
		const onStatusChange = vi.fn();
		const screen = render(ListView, {
			props: {
				items: [item('car-1')],
				collection: collection(),
				wsSlug: 'ws',
				groupField: 'priority',
				statusOptions: STATUSES,
				canEdit: true,
				onLaneChange,
				onStatusChange,
			} as never,
		});
		return { screen, onLaneChange, onStatusChange };
	}

	it('sends the next STATUS through the status writer', async () => {
		const { screen, onStatusChange } = renderList();

		// PRECONDITION: the list really is grouped by priority, so the write
		// below is a chip click on a non-status grouping rather than on a
		// status one — where the defect is invisible because both targets agree.
		expect(screen.container.textContent).toContain('High');

		await fireEvent.click(chipIn(screen.container));

		expect(onStatusChange).toHaveBeenCalledTimes(1);
		expect(onStatusChange.mock.calls[0][0]).toEqual(expect.objectContaining({ id: 'car-1' }));
		// `in_progress` is index 1 of three, so the next status is `done`.
		expect(onStatusChange.mock.calls[0][1]).toBe('done');
	});

	it('never reaches the lane writer, which is the only path to the group field', async () => {
		// THE ASSERTION THAT FAILS ON THE UNFIXED TREE. Before the split there
		// was one prop, so the chip's value arrived at the callback the page
		// points at `groupField` — and `done` was written into `fields.priority`.
		// Asserting the VALUE alone would not catch it: the value was right, the
		// destination was not, and the destination is a different callback.
		const { screen, onLaneChange } = renderList();
		await fireEvent.click(chipIn(screen.container));
		expect(onLaneChange).not.toHaveBeenCalled();
	});
});

describe('a status chip on a board grouped by a NON-status field', () => {
	function renderBoard() {
		const onLaneChange = vi.fn();
		const onStatusChange = vi.fn();
		const screen = render(BoardView, {
			props: {
				items: [item('car-1')],
				collection: collection(),
				wsSlug: 'ws',
				groupField: 'priority',
				canEdit: true,
				onLaneChange,
				onStatusChange,
			} as never,
		});
		return { screen, onLaneChange, onStatusChange };
	}

	it('cycles the STATUS field options, not the board lanes', async () => {
		// THE BOARD'S OWN ARM, and it is not the list's. `statusOptions={columns}`
		// fed the card the PRIORITY lanes, so `indexOf('in_progress')` missed,
		// returned -1, and index 0 sent `low` — the first lane. Both assertions
		// below are needed: `toBe('done')` alone would also pass if the chip
		// cycled lanes in a schema where a lane happened to be named `done`,
		// and `not.toBe('low')` alone permits any other wrong answer.
		const { screen, onStatusChange } = renderBoard();

		// PRECONDITION: grouped by priority, so the lanes really are the wrong
		// vocabulary for a status chip.
		expect(screen.container.textContent).toContain('High');

		await fireEvent.click(chipIn(screen.container));

		expect(onStatusChange).toHaveBeenCalledTimes(1);
		expect(onStatusChange.mock.calls[0][1]).toBe('done');
		expect(
			onStatusChange.mock.calls[0][1],
			'the chip cycled the LANES: -1 from a missed indexOf makes the next index 0',
		).not.toBe(PRIORITIES[0]);
	});

	it('never reaches the lane writer, which is the only path to the group field', async () => {
		const { screen, onLaneChange } = renderBoard();
		await fireEvent.click(chipIn(screen.container));
		expect(onLaneChange).not.toHaveBeenCalled();
	});
});

describe('CONTROL: grouped BY status, where the two writers agree', () => {
	// Without this, routing the chip to a callback that does nothing at all
	// would satisfy every leg above. It also pins the case the fix must NOT
	// change: on a status-grouped board the lanes and the status options are the
	// same array, and they were before the fix too.
	it('still cycles status on a status-grouped board', async () => {
		const onLaneChange = vi.fn();
		const onStatusChange = vi.fn();
		const screen = render(BoardView, {
			props: {
				items: [item('car-1')],
				collection: collection(),
				wsSlug: 'ws',
				groupField: 'status',
				canEdit: true,
				onLaneChange,
				onStatusChange,
			} as never,
		});

		await fireEvent.click(chipIn(screen.container));

		expect(onStatusChange).toHaveBeenCalledTimes(1);
		expect(onStatusChange.mock.calls[0][1]).toBe('done');
		expect(onLaneChange).not.toHaveBeenCalled();
	});

	it('still cycles status on a status-grouped list', async () => {
		const onLaneChange = vi.fn();
		const onStatusChange = vi.fn();
		const screen = render(ListView, {
			props: {
				items: [item('car-1')],
				collection: collection(),
				wsSlug: 'ws',
				groupField: 'status',
				statusOptions: STATUSES,
				canEdit: true,
				onLaneChange,
				onStatusChange,
			} as never,
		});

		await fireEvent.click(chipIn(screen.container));

		expect(onStatusChange).toHaveBeenCalledTimes(1);
		expect(onStatusChange.mock.calls[0][1]).toBe('done');
		expect(onLaneChange).not.toHaveBeenCalled();
	});
});

/**
 * Dispatch svelte-dnd-action's real `finalize` event on a lane's drop zone.
 *
 * `movedId` names the card that MOVED, not the first in the list — getting that
 * wrong makes the handler find no `originalItem` and skip the write entirely,
 * which is a green that measures nothing.
 */
function finalizeOn(container: HTMLElement, selector: string, laneIndex: number, items: Item[], movedId: string) {
	const zone = container.querySelectorAll(selector)[laneIndex];
	expect(zone, `lane ${laneIndex} (${selector}) should have a drop zone`).toBeTruthy();
	zone.dispatchEvent(
		new CustomEvent('finalize', {
			detail: { items, info: { trigger: 'droppedIntoZone', id: movedId } },
		}),
	);
}

/**
 * THE LEGS THAT SEPARATE A FIXED TREE FROM THE UNFIXED ONE, and they are here
 * rather than with the chip legs above because of what a mock can and cannot
 * see.
 *
 * A chip leg alone does NOT discriminate for the list. Before the fix the two
 * gestures shared one prop, so a chip click called the callback the test passed
 * as `onStatusChange` — with the RIGHT VALUE — and the unfixed component simply
 * ignored the `onLaneChange` prop it did not declare. Both assertions pass. The
 * defect was never in the value or in the callback the chip reached; it was that
 * the SAME callback served the drop, so the page pointed it at `groupField`.
 *
 * What a component-level mock CAN see is exactly that sharing: drive the DROP
 * and ask which callback it went to. On a fixed tree the drop reaches
 * `onLaneChange` and the chip reaches `onStatusChange`; on the unfixed tree both
 * reach `onStatusChange`, because there is only one. Paired with the chip legs
 * above, that pins the split rather than either half of it.
 */
describe('the DROP writes through a different callback than the chip', () => {
	it('in the list: a drop between lanes reaches the lane writer alone', async () => {
		const onLaneChange = vi.fn();
		const onStatusChange = vi.fn();
		const low = item('car-low', 'low');
		const high = item('car-high', 'high');
		const screen = render(ListView, {
			props: {
				items: [low, high],
				collection: collection(),
				wsSlug: 'ws',
				groupField: 'priority',
				statusOptions: STATUSES,
				canEdit: true,
				onLaneChange,
				onStatusChange,
			} as never,
		});

		// PRECONDITION: two lanes rendered, so the drop below really crosses one.
		expect(screen.container.querySelectorAll('.group-items').length).toBeGreaterThan(1);

		finalizeOn(screen.container, '.group-items', 0, [low, high], 'car-high');
		await Promise.resolve();

		expect(onLaneChange).toHaveBeenCalledTimes(1);
		expect(
			onStatusChange,
			'the drop went to the STATUS writer — the two gestures still share one callback',
		).not.toHaveBeenCalled();
	});

	it('on the board: a drop into a lane reaches the lane writer alone', async () => {
		const onLaneChange = vi.fn();
		const onStatusChange = vi.fn();
		const low = item('car-low', 'low');
		const high = item('car-high', 'high');
		const screen = render(BoardView, {
			props: {
				items: [low, high],
				collection: collection(),
				wsSlug: 'ws',
				groupField: 'priority',
				canEdit: true,
				onLaneChange,
				onStatusChange,
			} as never,
		});

		expect(screen.container.querySelectorAll('.column-cards').length).toBeGreaterThan(1);

		finalizeOn(screen.container, '.column-cards', 0, [low, high], 'car-high');
		await Promise.resolve();

		expect(onLaneChange).toHaveBeenCalledTimes(1);
		expect(onStatusChange).not.toHaveBeenCalled();
	});
});

describe('a status field that is not a status at all', () => {
	// THE HAZARD THE BOARD COMMENT NAMES, pinned from the side that would notice
	// if it stopped holding. `cardStatusOptions` reads the `status` field's
	// declared options, so on a board grouped BY a `status` that was retyped to a
	// relation type — which KEEPS its stale `options`, nothing strips them — a
	// cycling chip would offer names of lanes that no longer exist and write a
	// scalar into a field that is not one.
	//
	// TWO DIFFERENT GUARDS STOP IT and the fixtures below are split so each leg
	// can only be saved by one of them. My first version tested the
	// `multi_relation` case alone and claimed it proved the field-type guard; it
	// did not — an ARRAY value is withheld by the VALUE-shape test long before
	// the field's type is consulted, so deleting the type guard left the leg
	// green (measured). A fixture rejectable for two reasons discriminates
	// neither.
	function renderRetypedStatus(type: string, storedStatus: unknown) {
		const coll = collection();
		coll.schema = JSON.stringify({
			fields: [
				{ key: 'status', label: 'Status', type, collection: 'colors', options: ['old-a', 'old-b'] },
			],
		});
		const retyped = {
			...item('car-1'),
			fields: JSON.stringify({ status: storedStatus }),
		} as Item;
		return render(BoardView, {
			props: {
				items: [retyped],
				collection: coll,
				wsSlug: 'ws',
				groupField: 'status',
				canEdit: true,
				onLaneChange: vi.fn(),
				onStatusChange: vi.fn(),
			} as never,
		});
	}

	it('renders no chip when a retyped `status` holds a LIST — the value-shape guard', () => {
		// `multi_select`, NOT `multi_relation`, and that is the whole point of the
		// leg. A relation type is withheld by the field-type guard below, so a
		// relation fixture here would be saved by either guard and pin neither —
		// measured: with a `multi_relation` fixture, deleting the value-shape test
		// left this green. `multi_select` is not a relation type, so `chippable`
		// passes it through and only the value's shape can withhold it.
		const screen = renderRetypedStatus('multi_select', ['old-a']);
		// PRECONDITION: the card rendered, so "no chip" is not "no card".
		expect(screen.container.querySelectorAll('.item-card')).toHaveLength(1);
		expect(screen.container.querySelector('[title="Click to cycle status"]')).toBeNull();
		expect(screen.container.textContent).not.toContain('Old A');
	});

	it('renders no chip when a retyped `status` holds an ID STRING — the field-type guard', () => {
		// The case that reaches `ItemCard.chippable` (BUG-3016): a SCALAR relation
		// value IS a string, so the value-shape test above passes it through and
		// only the field's declared type can withhold it. Without that guard the
		// card renders a stored item id as a title-cased pill AND makes it
		// clickable — which would now cycle `old-a`/`old-b` into it.
		const screen = renderRetypedStatus('relation', 'id-red');
		expect(screen.container.querySelectorAll('.item-card')).toHaveLength(1);
		expect(screen.container.querySelector('[title="Click to cycle status"]')).toBeNull();
		expect(screen.container.textContent).not.toContain('Id Red');
	});
});
