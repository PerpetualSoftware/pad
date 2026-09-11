import { afterEach, describe, expect, it } from 'vitest';
import { render, cleanup } from '@testing-library/svelte';
import PublicListView from './PublicListView.svelte';
import type { PublicCollection, PublicItem } from './shareView';

/**
 * TASK-2998, codex round 2 — the LIST door onto public grouping.
 *
 * `resolveGroupField` guards the public BOARD. A saved view with
 * `view_type: "list"` routes its `group_by` to `list_group_by`, which this
 * component resolves itself — so the board-only refusal left a shared list
 * rendering one group per stored relation id.
 *
 * Rendered rather than source-guarded, because the predicate already has unit
 * tests and what was unobserved is whether THIS component asks it (CONVE-19):
 * the mutant that reverted this call site left every predicate test green.
 */

const RELATION_ID = 'f47ac10b-58cc-4372-a567-0e02b2c3d479';

function collection(groupBy: string, fieldType: string): PublicCollection {
	return {
		name: 'Cars',
		slug: 'cars',
		fields: [
			{ key: 'car_color', label: 'Colour', type: fieldType },
			{ key: 'title', label: 'Title', type: 'text' },
		],
		settings: { list_group_by: groupBy },
	} as unknown as PublicCollection;
}

function item(key: string, value: string): PublicItem {
	return {
		key,
		title: key,
		fields: { car_color: value },
		tags: [],
	} as unknown as PublicItem;
}

afterEach(() => {
	cleanup();
});

describe('PublicListView grouped by a relation field', () => {
	it('does not render a group per stored id', () => {
		const screen = render(PublicListView, {
			props: {
				collection: collection('car_color', 'relation'),
				items: [item('a', RELATION_ID)],
			} as never,
		});

		// PRECONDITION: the item rendered at all, so "no group" is not a claim
		// about an empty list.
		expect(screen.container.textContent).toContain('a');

		// ASSERTED AS THE ABSENCE OF A GROUP HEADING, not as the absence of the
		// id string. `formatLabel` replaces `-` with spaces and title-cases, so
		// a uuid renders as "F47ac10b 58cc …" and `not.toContain(RELATION_ID)`
		// passed against the broken component. Measured: that is exactly how
		// the mutant survived the first version of this test.
		expect(screen.container.querySelectorAll('.group-heading')).toHaveLength(0);
	});

	it('STILL groups by an ordinary field — the counterfactual', () => {
		// A refusal that fired for every field would silently ungroup every
		// shared list on the instance, which is a worse regression than the one
		// being fixed.
		const screen = render(PublicListView, {
			props: {
				collection: collection('car_color', 'select'),
				items: [item('a', 'red')],
			} as never,
		});

		expect(screen.container.querySelectorAll('.group-heading')).toHaveLength(1);
		expect(screen.container.querySelector('.group-heading')?.textContent).toContain('Red');
	});
});
