import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';
import type { Collection, Item, ItemIndexRow } from '$lib/types';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

/**
 * TASK-2998 / PLAN-2857 U7 — the BoardView half of grouping by a relation.
 *
 * The rules are unit-tested in `relationGroups.test.ts`; what is only
 * observable HERE is the wiring (CONVE-19): that the board asks for relation
 * lanes at all when the group field is a relation, labels them with the chip
 * vocabulary rather than the stored id, and keeps the three non-live cases
 * apart on screen.
 */

const ROWS: Record<string, ItemIndexRow> = {
	'id-red': {
		id: 'id-red',
		title: 'Red',
		item_number: 1,
		collection_prefix: 'COLOR',
		collection_slug: 'colors',
		deleted_at: null,
	} as unknown as ItemIndexRow,
	'id-blue': {
		id: 'id-blue',
		title: 'Blue',
		item_number: 2,
		collection_prefix: 'COLOR',
		collection_slug: 'colors',
		deleted_at: null,
	} as unknown as ItemIndexRow,
	'id-gone': {
		id: 'id-gone',
		title: 'Gone',
		item_number: 9,
		collection_prefix: 'COLOR',
		collection_slug: 'colors',
		deleted_at: '2026-01-01T00:00:00Z',
	} as unknown as ItemIndexRow,
};

vi.mock('$lib/stores/localIndex.svelte', () => ({
	localIndex: {
		findByIdOrSlug: (_ws: string, id: string) => ROWS[id] ?? null,
		getByCollection: () => [],
	},
}));

vi.mock('$lib/stores/collections.svelte', () => ({
	collectionStore: { collections: [{ slug: 'colors' }, { slug: 'cars' }] },
}));

import BoardView from './BoardView.svelte';

/** BoardView's source with comments stripped, so a guard cannot be satisfied by prose. */
const SRC = readFileSync(resolve(__dirname, './BoardView.svelte'), 'utf8').replace(
	/^[ \t]*\/\/.*$/gm,
	'',
);

function collection(): Collection {
	return {
		id: 'c1',
		workspace_id: 'ws1',
		name: 'Cars',
		slug: 'cars',
		icon: '',
		description: '',
		schema: JSON.stringify({
			fields: [{ key: 'car_color', label: 'Colour', type: 'relation', collection: 'colors' }],
		}),
		settings: JSON.stringify({}),
		sort_order: 0,
		is_default: true,
		is_system: false,
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
		prefix: 'CAR',
	} as unknown as Collection;
}

function item(id: string, value?: string): Item {
	return {
		id,
		workspace_id: 'ws1',
		collection_id: 'c1',
		slug: id,
		title: id,
		content: '',
		fields: JSON.stringify(value === undefined ? {} : { car_color: value }),
		tags: '[]',
		status: 'open',
		created_at: '2026-01-01T00:00:00Z',
		updated_at: '2026-01-01T00:00:00Z',
	} as unknown as Item;
}

const DANGLING = 'f47ac10b-58cc-4372-a567-0e02b2c3d479';

function renderBoard(items: Item[]) {
	return render(BoardView, {
		props: {
			items,
			collection: collection(),
			wsSlug: 'ws',
			groupField: 'car_color',
			onStatusChange: vi.fn(),
		} as never,
	});
}

afterEach(() => {
	cleanup();
});

describe('BoardView grouped by a relation field', () => {
	/**
	 * Lane names read from the DOM, NOT by substring over the whole page.
	 * `formatLabel('id-red')` renders "Id-Red", which contains "Red" — so a
	 * `toContain('Red')` assertion passes against a board that fell back to
	 * formatting the raw id, which is the defect. Measured: the label mutant
	 * survived that version of this test.
	 */
	const lanes = (screen: { container: HTMLElement }) =>
		[...screen.container.querySelectorAll('.column-name')].map((el) => ({
			ref: el.querySelector('.lane-ref')?.textContent?.trim() ?? null,
			note: el.querySelector('.lane-note')?.textContent?.trim() ?? null,
			// Everything the lane says that is not the ref or the note.
			name: [...el.childNodes]
				.filter(
					(n) =>
						!(n instanceof HTMLElement) ||
						(!n.classList.contains('lane-ref') && !n.classList.contains('lane-note')),
				)
				.map((n) => n.textContent ?? '')
				.join('')
				.trim(),
		}));

	/**
	 * Cards per lane, read from the DOM. Header assertions alone cannot see the
	 * bucketing (codex round 3): removing `bucketByColumn`'s `valueFor`
	 * callback leaves every relation lane RENDERED and EMPTY while the cards
	 * fall into Uncategorized, and every label test stays green.
	 */
	const cardsByLane = (screen: { container: HTMLElement }) => {
		const out: Record<string, number> = {};
		for (const col of screen.container.querySelectorAll('.kanban-column')) {
			// Whitespace stripped ENTIRELY: the ref, name and note are separate
			// spans with CSS spacing, so their textContent runs together in one
			// place and not another depending on markup indentation.
			const name = (col.querySelector('.column-name')?.textContent ?? '').replace(/\s+/g, '');
			out[name] = col.querySelectorAll('.item-card').length;
		}
		return out;
	};

	it('puts the cards in the lanes, not merely the lanes on the board', () => {
		const screen = renderBoard([
			item('car-1', 'id-red'),
			item('car-2', 'id-red'),
			item('car-3', 'id-gone'),
			item('car-4', DANGLING),
			item('car-5'),
		]);

		const byLane = cardsByLane(screen);
		// PRECONDITION: every card is somewhere, so a lane reading 0 is a
		// misplacement rather than a card that never rendered.
		expect(Object.values(byLane).reduce((a, b) => a + b, 0)).toBe(5);
		expect(byLane['COLOR-1Red']).toBe(2);
		expect(byLane['COLOR-9Gone(deleted)']).toBe(1);
		expect(byLane['Unresolvedreference']).toBe(1);
		expect(byLane['Uncategorized']).toBe(1);
	});

	it('places a PADDED legacy value in the same lane as its clean form', () => {
		// The seam the trim closed, asserted where a user would see it.
		const screen = renderBoard([item('car-1', 'id-red'), item('car-2', '  id-red  ')]);
		expect(cardsByLane(screen)['COLOR-1Red']).toBe(2);
	});

	it('does not turn the card\'s status chip into a RELATION setter', () => {
		// codex round 3, P1. Cards were handed `statusOptions={columns}`, which
		// is fine while a lane value is a status option — under relation
		// grouping the lanes are ITEM IDS, and the parent handler writes what it
		// receives into `fields[groupField]`. So a click on the status chip set
		// the card's relation, cycling through target ids and able to land on
		// the deleted or unresolved lane, which the write path then refuses.
		// The collection needs a STATUS field as well as the relation, or the
		// card renders no status chip for an unrelated reason and the test
		// cannot discriminate. Measured: without it the mutant survived.
		const coll = collection();
		coll.schema = JSON.stringify({
			fields: [
				{ key: 'car_color', label: 'Colour', type: 'relation', collection: 'colors' },
				{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'] },
			],
		});
		const withStatus = (id: string, color: string) =>
			({
				...item(id, color),
				fields: JSON.stringify({ car_color: color, status: 'open' }),
			}) as Item;

		const screen = render(BoardView, {
			props: {
				items: [withStatus('car-1', 'id-red'), withStatus('car-2', 'id-blue')],
				collection: coll,
				wsSlug: 'ws',
				groupField: 'car_color',
				onStatusChange: vi.fn(),
			} as never,
		});

		// PRECONDITION: the cards rendered, so "no chip" is not "no card".
		expect(screen.container.querySelectorAll('.item-card')).toHaveLength(2);
		expect(screen.container.querySelector('[title="Click to cycle status"]')).toBeNull();
	});

	it('STILL offers status cycling on an ordinary board — the counterfactual', () => {
		// Withholding it everywhere would remove a working affordance from every
		// board on the instance, which is worse than the defect.
		const coll = collection();
		coll.schema = JSON.stringify({
			fields: [{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'] }],
		});
		const withStatus = (id: string) =>
			({
				id,
				workspace_id: 'ws1',
				collection_id: 'c1',
				slug: id,
				title: id,
				content: '',
				fields: JSON.stringify({ status: 'open' }),
				tags: '[]',
				status: 'open',
				created_at: '2026-01-01T00:00:00Z',
				updated_at: '2026-01-01T00:00:00Z',
			}) as unknown as Item;

		const screen = render(BoardView, {
			props: {
				items: [withStatus('car-1')],
				collection: coll,
				wsSlug: 'ws',
				groupField: 'status',
				onStatusChange: vi.fn(),
			} as never,
		});

		expect(screen.container.querySelector('[title="Click to cycle status"]')).not.toBeNull();
	});

	it('labels a live lane with REF and title, not the stored id', () => {
		const screen = renderBoard([item('car-1', 'id-red')]);

		// PRECONDITION: the lane exists at all. A relation field has no
		// `options`, so before this unit every card landed in UNCATEGORIZED and
		// there was no lane to label.
		expect(lanes(screen)).toContainEqual({ ref: 'COLOR-1', note: null, name: 'Red' });
		expect(screen.container.innerHTML).not.toContain('id-red');
	});

	it('keeps a DELETED target as its own lane, and says so', () => {
		const screen = renderBoard([item('car-1', 'id-red'), item('car-2', 'id-gone')]);
		const html = screen.container.innerHTML;

		expect(lanes(screen)).toContainEqual({ ref: 'COLOR-9', note: '(deleted)', name: 'Gone' });
		// And not merged with the live one, which keeps its own honest label.
		expect(lanes(screen)).toContainEqual({ ref: 'COLOR-1', note: null, name: 'Red' });
		expect(html).not.toContain('id-gone');
	});

	it('renders a dangling value as an honest lane, never the bare id', () => {
		const screen = renderBoard([item('car-1', DANGLING)]);
		const html = screen.container.innerHTML;

		expect(lanes(screen)).toContainEqual({ ref: null, note: null, name: 'Unresolved reference' });
		expect(html).not.toContain(DANGLING);
		expect(html).not.toContain('f47ac10b');
	});

	it('does NOT group by relation when the field is an ordinary select', () => {
		// The counterfactual for the whole branch: a select must still bucket
		// against its own options, and a board that asked the local index for
		// lanes on every field type would relabel ordinary lanes with whatever
		// an id-shaped option resolved to.
		const coll = collection();
		coll.schema = JSON.stringify({
			fields: [{ key: 'car_color', label: 'Colour', type: 'select', options: ['id-red'] }],
		});
		const screen = render(BoardView, {
			props: {
				items: [item('car-1', 'id-red')],
				collection: coll,
				wsSlug: 'ws',
				groupField: 'car_color',
				onStatusChange: vi.fn(),
			} as never,
		});

		const html = screen.container.innerHTML;
		expect(html).not.toContain('COLOR-1');
		// formatLabel's own casing of the option value, not a resolved title.
		expect(lanes(screen)).toContainEqual({ ref: null, note: null, name: 'Id-Red' });
	});
});

describe('the drop gate on a relation lane', () => {
	/**
	 * A SOURCE guard, and the header says why rather than leaving it implied:
	 * the gate lives inside `commitColumnMove`, which is reached through
	 * svelte-dnd-action's drag events, and driving those in jsdom tests the
	 * drag library rather than this decision. Measured first — removing the
	 * gate leaves every rendering test in this file green, so without something
	 * here the wiring is unobserved (CONVE-19).
	 *
	 * WHAT A SOURCE GUARD CANNOT DO: it checks spellings, not behaviour. A gate
	 * that consulted the wrong lane, or one made unreachable by an earlier
	 * return, would still pass — and the rollback below is asserted as an
	 * ASSIGNMENT rather than as a board that visibly snaps back. The honest
	 * instrument for that is a drag driven through svelte-dnd-action, which
	 * tests the drag library as much as this decision.
	 */
	it('puts the card BACK when it refuses the drop', () => {
		// codex round 1, P1, and the most important mutant this file missed:
		// svelte-dnd-action has already moved the card in `columnData` by the
		// time the gate runs, so refusing the WRITE without undoing that leaves
		// the board showing a move that never happened — the exact failure the
		// refusal exists to avoid, one step later. Every rendering test stayed
		// green through it.
		const start = SRC.indexOf('async function commitColumnMove(');
		const body = SRC.slice(start, SRC.indexOf('\n\t}', start));
		const gate = body.indexOf('relationLaneAcceptsDrop');
		// Matched as "reassigns columnData from propColumnData", not as one exact
		// spelling: the restore became a COPY in round 6 (the two aliased, so
		// assigning the derived back restored nothing), and a guard pinned to
		// the old literal failed the fix rather than the defect.
		const restore = body.indexOf('columnData =');
		expect(restore, 'the refused drop is not reverted').toBeGreaterThan(-1);
		expect(restore).toBeGreaterThan(gate);
		expect(restore).toBeLessThan(body.indexOf('onStatusChange('));
		expect(body.slice(restore, body.indexOf('onStatusChange('))).toContain('propColumnData');
	});

	it('consults relationLaneAcceptsDrop BEFORE calling onStatusChange', () => {
		const start = SRC.indexOf('async function commitColumnMove(');
		expect(start, 'commitColumnMove was renamed or removed').toBeGreaterThan(-1);
		const body = SRC.slice(start, SRC.indexOf('\n\t}', start));

		const gate = body.indexOf('relationLaneAcceptsDrop');
		const write = body.indexOf('onStatusChange(');
		expect(gate, 'the relation drop gate is gone').toBeGreaterThan(-1);
		expect(write).toBeGreaterThan(-1);
		expect(gate, 'the write happens before the gate').toBeLessThan(write);
	});

	it('gates on isRelationGroup, so an ordinary board is untouched', () => {
		// THE EIGHTH WRONG-REASON TEST (codex round 6). It asserted only that
		// the source CONTAINS `if (isRelationGroup)` — a mutant leaving that
		// block empty and calling `onStatusChange` unconditionally would pass.
		// It now requires the refusal to live INSIDE that block, ahead of the
		// write.
		const start = SRC.indexOf('async function commitColumnMove(');
		const body = SRC.slice(start, SRC.indexOf('\n\t}', start));
		const gate = body.indexOf('if (isRelationGroup)');
		const refuse = body.indexOf('relationLaneAcceptsDrop');
		const write = body.indexOf('onStatusChange(');
		expect(gate).toBeGreaterThan(-1);
		expect(refuse).toBeGreaterThan(gate);
		expect(refuse).toBeLessThan(write);
		// and the block returns rather than falling through
		expect(body.slice(gate, write)).toContain('return;');
	});
});

describe('a relation field with no declared target collection', () => {
	it('is NOT grouped as a relation', () => {
		// `narrowRelationRow` skips the collection check when there is nothing
		// to check against, so a legacy or half-written relation field would
		// resolve ids ANYWHERE in the workspace and label lanes with whatever it
		// found. The filter UI already required a target; the board did not
		// (codex round 2).
		const coll = collection();
		coll.schema = JSON.stringify({
			fields: [{ key: 'car_color', label: 'Colour', type: 'relation' }],
		});
		const screen = render(BoardView, {
			props: {
				items: [item('car-1', 'id-red')],
				collection: coll,
				wsSlug: 'ws',
				groupField: 'car_color',
				onStatusChange: vi.fn(),
			} as never,
		});

		// No lane resolved from the workspace-wide index — the ref is the tell.
		expect(screen.container.querySelector('.lane-ref')).toBeNull();
	});

	it('withholds the lane menu\'s "add" for a relation lane, like the visible +', () => {
		// Otherwise the menu is an undocumented second creation path into a
		// lane whose "+" was deliberately withheld, and it contradicts this
		// component's own stated design rather than merely duplicating it.
		const start = SRC.indexOf('onAddItem={onCreateInColumn');
		expect(start, 'the lane menu no longer passes onAddItem').toBeGreaterThan(-1);
		expect(SRC.slice(start, start + 160)).toContain('!isRelationGroup');
	});
});

describe('the MENU move into a refused relation lane', () => {
	/**
	 * codex round 6, P2 — and the reason this leg is rendered rather than
	 * source-guarded like its drag sibling.
	 *
	 * Round 6 reported that `moveItem`'s `columnData[col] = ...` writes THROUGH
	 * to `propColumnData`'s cached object, so restoring by assigning it back
	 * restores nothing. That premise is FALSE (settled in round 7, see the
	 * failure-exit block at the end of this file): the alias mutant SURVIVES
	 * this test, and a direct probe of the shape reports the derived's cache
	 * unchanged by the write.
	 *
	 * What this test does prove — verified by a gate mutant killing it — is
	 * that it drives the refusal path for real. No source guard could have done
	 * that; it took driving the menu.
	 */
	const cardsIn = (screen: { container: HTMLElement }, laneName: string) => {
		for (const col of screen.container.querySelectorAll('.kanban-column')) {
			const name = (col.querySelector('.column-name')?.textContent ?? '').replace(/\s+/g, '');
			if (name === laneName) return [...col.querySelectorAll('.card-title')].map((e) => e.textContent?.trim());
		}
		return null;
	};

	it('leaves the card where it was', async () => {
		const onStatusChange = vi.fn();
		const screen = render(BoardView, {
			props: {
				items: [item('car-1', 'id-red'), item('car-2', 'id-gone')],
				collection: collection(),
				wsSlug: 'ws',
				groupField: 'car_color',
				onStatusChange,
				onReorder: vi.fn(),
			} as never,
		});

		// PRECONDITION: two lanes, each holding its own card, and the deleted
		// lane is to the RIGHT of the live one (lanes sort live-then-deleted).
		expect(cardsIn(screen, 'COLOR-1Red')).toEqual(['car-1']);
		expect(cardsIn(screen, 'COLOR-9Gone(deleted)')).toEqual(['car-2']);

		// Open car-1's action menu and move it right, into the deleted lane.
		// The card's own ⋮ (ItemActionsMenu), not the lane's ⋯ — and the menu may
		// render into <body>, so the search below is document-wide.
		const cardMenuButtons = [...screen.container.querySelectorAll('.item-card button')].filter(
			(b) => (b.textContent ?? '').trim() === '⋮',
		) as HTMLButtonElement[];
		expect(cardMenuButtons.length, 'no card action menu trigger — re-point this test').toBeGreaterThan(0);
		cardMenuButtons[0].click();
		await tick();
		await tick();

		const moveRight = [...document.querySelectorAll('button')].find((b) =>
			(b.textContent ?? '').includes('Move right'),
		);
		expect(moveRight, '"Move right" was not offered — re-point this test').toBeTruthy();
		moveRight!.click();
		await tick();
		await tick();

		// The write was refused AND the board did not keep the card there.
		expect(onStatusChange).not.toHaveBeenCalled();
		expect(cardsIn(screen, 'COLOR-1Red')).toEqual(['car-1']);
		expect(cardsIn(screen, 'COLOR-9Gone(deleted)')).toEqual(['car-2']);
	});
});

describe('the MENU move whose WRITE fails', () => {
	/**
	 * codex round 7, P1 — REFUTED, and this test is what refutes it.
	 *
	 * Rounds 6 and 7 both reasoned that the sync `$effect` assigns the
	 * `$derived.by` VALUE into `columnData`, so `moveItem`'s
	 * `columnData[col] = …` writes THROUGH to the derived cache — round 6
	 * against the refusal exit, round 7 against this one, the failure exit
	 * where `onStatusChange` rejects and the restore rests on
	 * `dropCooldown = false` alone.
	 *
	 * The premise does not hold. A probe over exactly that shape — a
	 * `$derived.by` object assigned into a `$state`, a property written on the
	 * `$state`, then the derived read back — reported the derived's cached
	 * value UNCHANGED (`after mutation, derived.a=[1]`), and the restore then
	 * produced the original lanes. A `$state` holding a derived's object is a
	 * deep proxy; its property writes do not reach what the derivation cached.
	 *
	 * So this test passes against the tree as written, and that is the finding
	 * rather than a fix. It is kept because the failure exit had NO behavioural
	 * coverage before it and because it discriminates: removing
	 * `dropCooldown = false` turns it red (measured), so its green is evidence
	 * that the cooldown release is what restores the board, not an accident of
	 * a card that never moved — which the `toHaveBeenCalledTimes(1)`
	 * precondition rules out separately.
	 *
	 * The class is every exit from `commitColumnMove` that must restore lanes
	 * after `moveItem` mutated them. There are two, the refusal and the
	 * failure; both are now covered, and neither is defective.
	 */
	const cardsIn = (screen: { container: HTMLElement }, laneName: string) => {
		for (const col of screen.container.querySelectorAll('.kanban-column')) {
			const name = (col.querySelector('.column-name')?.textContent ?? '').replace(/\s+/g, '');
			if (name === laneName) return [...col.querySelectorAll('.card-title')].map((e) => e.textContent?.trim());
		}
		return null;
	};

	it('puts the card back when onStatusChange rejects', async () => {
		const onStatusChange = vi.fn().mockRejectedValue(new Error('nope'));
		const screen = render(BoardView, {
			props: {
				items: [item('car-1', 'id-blue'), item('car-2', 'id-red')],
				collection: collection(),
				wsSlug: 'ws',
				groupField: 'car_color',
				onStatusChange,
				onReorder: vi.fn(),
			} as never,
		});

		// PRECONDITION: two LIVE lanes, sorted by title, each holding its card.
		expect(cardsIn(screen, 'COLOR-2Blue')).toEqual(['car-1']);
		expect(cardsIn(screen, 'COLOR-1Red')).toEqual(['car-2']);

		const cardMenuButtons = [...screen.container.querySelectorAll('.item-card button')].filter(
			(b) => (b.textContent ?? '').trim() === '⋮',
		) as HTMLButtonElement[];
		expect(cardMenuButtons.length, 'no card action menu trigger — re-point this test').toBeGreaterThan(0);
		cardMenuButtons[0].click();
		await tick();
		await tick();

		const moveRight = [...document.querySelectorAll('button')].find((b) =>
			(b.textContent ?? '').includes('Move right'),
		);
		expect(moveRight, '"Move right" was not offered — re-point this test').toBeTruthy();
		moveRight!.click();
		await tick();
		await tick();
		await tick();

		// PRECONDITION for the assertion below: the write was ATTEMPTED and
		// failed. Without this, a board that never moved the card would pass.
		expect(onStatusChange).toHaveBeenCalledTimes(1);

		// The write failed, so the board must show the state it started in.
		expect(cardsIn(screen, 'COLOR-2Blue')).toEqual(['car-1']);
		expect(cardsIn(screen, 'COLOR-1Red')).toEqual(['car-2']);
	});
});

describe('a REFUSED grouping must not write a group value (U4, codex round 1 P5)', () => {
	/**
	 * The defect: `isRelationGroup` is `type === 'relation'`, so a
	 * `multi_relation` board took the SCALAR arm of the drop handler.
	 * `currentValue` was then the stored ARRAY and `targetColumn` a lane
	 * string, which are never equal, so every drop fired
	 * `onStatusChange(item, "<lane>")` — a write that would replace the list
	 * with a scalar. The server refuses it, `moveSucceeded` goes false, and the
	 * reorder is dropped. The refusal NOTICE rendered correctly throughout,
	 * which is exactly what hid it: the view said the right sentence and then
	 * did the wrong write.
	 */
	function multiCollection(): Collection {
		const coll = collection();
		coll.schema = JSON.stringify({
			fields: [{ key: 'car_color', label: 'Colour', type: 'multi_relation', collection: 'colors' }],
		});
		return coll;
	}

	it('renders the refusal and lands every card in ONE fallback lane', () => {
		// PRECONDITION for the guard below, and the thing that makes it about a
		// screen that actually happens: this configuration reaches the board,
		// says why it cannot group, and still shows the cards.
		const screen = render(BoardView, {
			props: {
				items: [item('car-1', 'id-red'), item('car-2', 'id-blue')],
				collection: multiCollection(),
				wsSlug: 'ws',
				groupField: 'car_color',
				onStatusChange: vi.fn(),
				onReorder: vi.fn(),
			} as never,
		});
		expect(screen.container.textContent).toContain('more than one group');
		expect(screen.container.querySelectorAll('.kanban-column')).toHaveLength(1);
		expect([...screen.container.querySelectorAll('.card-title')].map((e) => e.textContent?.trim()))
			.toEqual(['car-1', 'car-2']);
	});

	it('lands every card in ONE lane even when the schema RETAINED options', () => {
		// codex round 8, R8-3. Round 7 gated every affordance that WRITES the
		// group value and left the LANES reading it, so the board went on
		// bucketing under a notice saying it did not.
		//
		// The fixture is the finding's own: a lane key is the STRINGIFIED array,
		// so options carried through a type change can MATCH one. `['id-red']`
		// and `['id-red','id-blue']` stringify to 'id-red' and 'id-red,id-blue',
		// and with those retained as options the two cars landed in two named
		// lanes under "Showing everything ungrouped." Empty named lanes would be
		// wrong too, but this version makes the board disagree with its own
		// notice about the CARDS, which is the visible defect.
		const coll = collection();
		coll.schema = JSON.stringify({
			fields: [
				{
					key: 'car_color',
					label: 'Colour',
					type: 'multi_relation',
					collection: 'colors',
					options: ['id-red', 'id-red,id-blue'],
				},
			],
		});
		const multi = (id: string, value: string[]) =>
			({ ...item(id), fields: JSON.stringify({ car_color: value }) }) as Item;

		const screen = render(BoardView, {
			props: {
				items: [multi('car-1', ['id-red']), multi('car-2', ['id-red', 'id-blue'])],
				collection: coll,
				wsSlug: 'ws',
				groupField: 'car_color',
				onStatusChange: vi.fn(),
				onReorder: vi.fn(),
			} as never,
		});

		// PRECONDITIONS: the refusal is in force and both cards rendered, so
		// "one lane" is neither "not refused" nor "nothing on the board".
		expect(screen.container.textContent).toContain('more than one group');
		expect([...screen.container.querySelectorAll('.card-title')].map((e) => e.textContent?.trim()))
			.toEqual(['car-1', 'car-2']);
		expect(screen.container.querySelectorAll('.kanban-column')).toHaveLength(1);
		// And the one lane holds them BOTH — a lane count alone would pass on a
		// board that dropped a card.
		expect(
			[...screen.container.querySelectorAll('.kanban-column')].map(
				(c) => c.querySelectorAll('.item-card').length,
			),
		).toEqual([2]);
	});

	it('withholds the status chip even when the schema RETAINED options', () => {
		// The fixture that makes this leg discriminate, and the whole lesson of
		// rounds 5 and 6. Round 5's fixture had a multi_relation with no
		// `options`, so `columns` was empty, the chip could not cycle for a
		// reason unrelated to the guard, and the mutant reverting the guard
		// SURVIVED. I read that as "the guard is unreachable" and removed it.
		//
		// Nothing strips `options` when a field's type changes. A `multi_select`
		// retyped to `multi_relation` in the schema editor keeps them — so
		// `columns` is non-empty, the chip cycles, and it writes a scalar into a
		// list. "A multi_relation declares no options" was a claim about the
		// schemas people write, never about the code.
		//
		// AND THIS LEG STOPPED DISCRIMINATING ITS GUARD IN ROUND 8, which is
		// worth saying plainly given how it got here. R8-3 makes a refused
		// grouping yield NO lanes, so `columns` is empty for a reason that has
		// nothing to do with `cardStatusOptions` — measured, not assumed:
		// deleting `groupingRefusal` from that derivation now leaves all 63
		// tests in this directory green. The situation round 5 THOUGHT it was in
		// is the situation the code is now actually in, by construction rather
		// than by a claim about schemas.
		//
		// The guard stays: it is one of four affordances phrased the same way,
		// and the drag one is NOT subsumed (its mutant still dies). What does
		// not stay is the pretence — a leg that cannot fail is not the evidence,
		// and the leg that goes red if R8-3 regresses is the one-lane test.
		const coll = collection();
		coll.schema = JSON.stringify({
			fields: [
				{
					key: 'car_color',
					label: 'Colour',
					type: 'multi_relation',
					collection: 'colors',
					options: ['old-a', 'old-b'],
				},
				{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'] },
			],
		});
		const withStatus = (id: string, color: string) =>
			({
				...item(id, color),
				fields: JSON.stringify({ car_color: [color], status: 'open' }),
			}) as Item;

		const screen = render(BoardView, {
			props: {
				items: [withStatus('car-1', 'id-red')],
				collection: coll,
				wsSlug: 'ws',
				groupField: 'car_color',
				onStatusChange: vi.fn(),
			} as never,
		});

		// PRECONDITIONS: the refusal is in force and the card rendered, so "no
		// chip" is neither "not refused" nor "no card".
		expect(screen.container.textContent).toContain('more than one group');
		expect(screen.container.querySelectorAll('.item-card')).toHaveLength(1);
		expect(screen.container.querySelector('[title="Click to cycle status"]')).toBeNull();
	});

	it('offers no lane create control, which would send the lane string as the value', () => {
		// The third affordance in the class, after the drag and the status chip
		// (codex round 7). A multi_relation that RETAINED options produced NAMED
		// lanes, so neither `isUncategorized` nor `isRelationGroup` withheld the
		// lane "+": creating there sent the lane string as the relation value.
		// Every affordance that writes the GROUP VALUE has to ask the refusal.
		//
		// REWRITTEN IN ROUND 8, AND THIS LEG NO LONGER DISCRIMINATES ITS GUARD.
		// Its precondition used to be `.kanban-column > 1` — the named lanes
		// really rendered, so "no +" was not "no lanes". R8-3 removed those
		// lanes: a refused grouping now yields the single UNCATEGORIZED lane,
		// which withholds the "+" through `!isUncategorized` regardless of the
		// `!groupingRefusal` gate this leg was written for. Deleting that gate
		// leaves this green.
		//
		// The gate is KEPT anyway, and the reason is not defence in depth: each
		// affordance asking the refusal directly is what made round 7's class
		// enumerable at all, and one of the four (drag) is NOT subsumed, since a
		// drop is gated on the refusal rather than on the lane. Keeping three of
		// four phrased the same way is worth more than deleting two lines. What
		// is not worth keeping is the pretence that this leg proves it: the leg
		// that actually fails when R8-3 regresses is the one-lane test above.
		const coll = collection();
		coll.schema = JSON.stringify({
			fields: [
				{
					key: 'car_color',
					label: 'Colour',
					type: 'multi_relation',
					collection: 'colors',
					options: ['old-a', 'old-b'],
				},
			],
		});
		const screen = render(BoardView, {
			props: {
				items: [item('car-1', 'id-red')],
				collection: coll,
				wsSlug: 'ws',
				groupField: 'car_color',
				onStatusChange: vi.fn(),
				onCreateInColumn: vi.fn(),
			} as never,
		});

		// PRECONDITIONS: refused, and a lane really is rendered — so "no +
		// button" is not "no lanes". One lane exactly, since R8-3.
		expect(screen.container.textContent).toContain('more than one group');
		expect(screen.container.querySelectorAll('.kanban-column')).toHaveLength(1);
		expect(screen.container.querySelectorAll('.item-card')).toHaveLength(1);
		const plus = [...screen.container.querySelectorAll('.kanban-column button')].filter(
			(b) => (b.textContent ?? '').trim() === '+',
		);
		expect(plus).toHaveLength(0);
	});

	it('RENDERS a card whose status is a list, instead of throwing (BUG-3041)', () => {
		// The crash this fixture used to be written AROUND. The leg below still
		// says "the item's status is ABSENT on purpose: a card carrying an ARRAY
		// there trips scalar status formatting and the board throws before
		// rendering" — that was true when it was written and is what BUG-3041
		// filed. This leg is the same schema WITH the array present.
		//
		// `status` and `priority` are conventional key names, not reserved ones,
		// so the schema editor will type them however it likes and nothing
		// rewrites the values already stored. The board correctly refuses to
		// GROUP by such a field; it then threw while drawing the card's chip,
		// which is a rendering contract and not a grouping one.
		const coll = collection();
		coll.schema = JSON.stringify({
			fields: [
				{
					key: 'status',
					label: 'Status',
					type: 'multi_relation',
					collection: 'colors',
					options: ['old-a', 'old-b'],
				},
				{ key: 'priority', label: 'Priority', type: 'multi_relation', collection: 'colors' },
			],
		});
		const listy = (id: string) =>
			({
				...item(id),
				fields: JSON.stringify({ status: ['id-red', 'id-blue'], priority: ['id-green'] }),
			}) as Item;

		const screen = render(BoardView, {
			props: {
				items: [listy('car-1')],
				collection: coll,
				wsSlug: 'ws',
				groupField: 'status',
				onStatusChange: vi.fn(),
			} as never,
		});

		// The card is THERE — the point is that the board draws rather than dies.
		expect([...screen.container.querySelectorAll('.card-title')].map((e) => e.textContent?.trim()))
			.toEqual(['car-1']);
		expect(screen.container.textContent).toContain('more than one group');

		// And no chip, rather than an empty one: a chip that formats nothing
		// describes nothing. Both keys are covered, since `priority` reaches the
		// same helpers by a different branch.
		expect(screen.container.querySelectorAll('.item-card .chip')).toHaveLength(0);
		// The raw ids must not leak out through a stringified array either.
		expect(screen.container.textContent).not.toContain('id-red');
	});

	it('CONTROL: ordinary string status AND priority still render their chips', () => {
		// Without this, withholding the chip for EVERY value would satisfy the
		// leg above, and the boards that work today would quietly lose their
		// chips. PRIORITY is here because the first version of this control
		// covered status alone — and the enumeration round showed that deleting
		// every ItemCard priority chip left the whole suite green (d4). Each
		// gate needs a leg that fails when it is removed.
		const coll = collection();
		coll.schema = JSON.stringify({
			fields: [
				{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'] },
				{ key: 'priority', label: 'Priority', type: 'select', options: ['high', 'low'] },
			],
		});
		const withBoth = (id: string) =>
			({ ...item(id), fields: JSON.stringify({ status: 'in_progress', priority: 'high' }) }) as Item;

		const screen = render(BoardView, {
			props: {
				items: [withBoth('car-1')],
				collection: coll,
				wsSlug: 'ws',
				groupField: 'status',
				onStatusChange: vi.fn(),
			} as never,
		});

		const chips = [...screen.container.querySelectorAll('.item-card .chip')].map((c) =>
			(c.textContent ?? '').trim(),
		);
		expect(chips).toContain('In Progress');
		expect(chips).toContain('High');
	});

	it('offers no bulk "Move all to" either — the FIFTH group-writing affordance', async () => {
		// Round 8 enumeration. Round 7 enumerated four affordances that write the
		// group value and this was not among them: the lane menu reads
		// `statusField.options` and offers them as destinations, so a `status`
		// field retyped to `multi_relation` with its options retained offers
		// named lanes that no longer exist and calls `onMoveColumn` with a
		// SCALAR — a write the server refuses for a list-shaped field.
		//
		// The item's status is ABSENT on purpose: a card carrying an ARRAY there
		// trips scalar status formatting and the board throws before rendering,
		// which is its own finding and not this one.
		const coll = collection();
		coll.schema = JSON.stringify({
			fields: [
				{
					key: 'status',
					label: 'Status',
					type: 'multi_relation',
					collection: 'colors',
					options: ['old-a', 'old-b'],
				},
			],
		});
		const onMoveColumn = vi.fn();
		const screen = render(BoardView, {
			props: {
				items: [{ ...item('car-1'), fields: JSON.stringify({}) } as Item],
				collection: coll,
				wsSlug: 'ws',
				groupField: 'status',
				onStatusChange: vi.fn(),
				onMoveColumn,
				// Supplied so the menu has an entry that is NOT gated on the
				// refusal — the control that proves it opened.
				onTagColumn: vi.fn(),
			} as never,
		});

		// PRECONDITIONS: refused, one lane, one card, and the lane menu really is
		// reachable — so "no Move all to" is not "no menu".
		expect(screen.container.textContent).toContain('more than one group');
		expect(screen.container.querySelectorAll('.kanban-column')).toHaveLength(1);
		expect(screen.container.querySelectorAll('.item-card')).toHaveLength(1);
		const menuButton = [...screen.container.querySelectorAll('button')].find(
			(b) => (b.textContent ?? '').trim() === '\u22ef',
		) as HTMLButtonElement | undefined;
		expect(menuButton, 'no lane actions menu trigger — re-point this test').toBeTruthy();
		menuButton!.click();
		await tick();
		await tick();

		// PRECONDITION FOR THE ABSENCE: the menu really opened. Without this the
		// leg passes against a menu that never rendered, which is how its first
		// version stayed green with the gate deleted. "Tag all" is present for
		// every lane and is not gated on the refusal.
		const menuEntries = [...document.querySelectorAll('button')].map((b) => (b.textContent ?? '').trim());
		expect(menuEntries.some((t) => t.includes('Tag all'))).toBe(true);

		expect(menuEntries.filter((t) => t.includes('Move all to'))).toHaveLength(0);
		expect(onMoveColumn).not.toHaveBeenCalled();
	});

	it('CONTROL: an ordinary board still offers the lane create control', () => {
		// Withholding it everywhere would remove a working affordance from every
		// board on the instance.
		const coll = collection();
		coll.schema = JSON.stringify({
			fields: [{ key: 'status', label: 'Status', type: 'select', options: ['open', 'done'] }],
		});
		const withStatus = (id: string) =>
			({
				...item(id),
				fields: JSON.stringify({ status: 'open' }),
			}) as Item;
		const screen = render(BoardView, {
			props: {
				items: [withStatus('car-1')],
				collection: coll,
				wsSlug: 'ws',
				groupField: 'status',
				onStatusChange: vi.fn(),
				onCreateInColumn: vi.fn(),
			} as never,
		});
		const plus = [...screen.container.querySelectorAll('.kanban-column button')].filter(
			(b) => (b.textContent ?? '').trim() === '+',
		);
		expect(plus.length).toBeGreaterThan(0);
	});

	it('gates the group write on groupingRefusal, not on the value comparison alone', () => {
		// A SOURCE guard, for the same reason its drag sibling above is one:
		// driving svelte-dnd-action's finalize through jsdom costs more setup
		// than the property is worth, and the property is structural.
		//
		// It reads the CONDITION rather than a spelling — the assertion is that
		// the refusal is consulted in the same `if` that gates the write, so a
		// rewrite that keeps the behaviour keeps the guard.
		const start = SRC.indexOf('async function handleFinalize');
		expect(start, 'handleFinalize not found — re-point this guard').toBeGreaterThan(-1);
		const body = SRC.slice(start, SRC.indexOf('\n\t}', SRC.indexOf('onStatusChange(', start)));
		const write = body.indexOf('await onStatusChange(');
		expect(write).toBeGreaterThan(-1);
		const guard = body.lastIndexOf('groupingRefusal', write);
		expect(guard, 'the drop handler never consults groupingRefusal before writing').toBeGreaterThan(-1);
		const condition = body.slice(guard, write);
		expect(condition).toContain('currentValue !== targetColumn');
		// The refusal must NEGATE the write, not merely appear near it.
		expect(body.slice(Math.max(0, guard - 8), guard)).toContain('!');
	});
});
