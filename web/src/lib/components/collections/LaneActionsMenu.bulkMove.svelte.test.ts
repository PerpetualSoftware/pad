import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, cleanup, screen } from '@testing-library/svelte';
import type { Collection, Item } from '$lib/types';
import LaneActionsMenu from './LaneActionsMenu.svelte';

/**
 * BUG-3074 — the board lane's bulk "Move all to" offered destinations that the
 * `status` field could not hold.
 *
 * The mechanism is that `options` SURVIVES a retype. `moveTargets` derived its
 * destinations from `statusField.options` and nothing asked whether the field
 * was still one those options could be written to, so a `status` retyped to
 * `multi_select` — a legitimately groupable type, so no grouping refusal fires —
 * rendered named lanes and sent a scalar into a list field. The server then
 * refused it once per item.
 *
 * WHY THIS RENDERS THE COMPONENT rather than calling the predicate (CONVE-19):
 * `laneKeyIsBulkMovable` is unit-tested next to `laneWriteValue`, and a passing
 * unit test there vouches for the predicate, not for the menu consulting it.
 * The claim this file makes is the BINDING — that the entry a user can click is
 * the one the type check reached.
 *
 * The pre-existing `moveTargets.length > 0` guard is why every negative case
 * below carries a POSITIVE control in the same shape: a field with no options
 * withholds the entry for a reason that has nothing to do with this fix, so a
 * test that only asserted absence would pass on the unfixed tree for half the
 * cases and prove nothing for the other half.
 */

vi.mock('$lib/stores/workspace.svelte', () => ({
	workspaceStore: { canEdit: true, currentWorkspace: { slug: 'ws' } },
}));

const collectionWith = (statusField: Record<string, unknown>): Collection =>
	({
		slug: 'tasks',
		name: 'Tasks',
		schema: JSON.stringify({ fields: [statusField] }),
		settings: '{}',
	}) as unknown as Collection;

const laneItems: Item[] = [
	{ id: 'i1', slug: 'i1', title: 'One', fields: '{"status":"open"}', tags: '[]' } as unknown as Item,
];

function renderMenu(collection: Collection) {
	return render(LaneActionsMenu, {
		props: {
			items: laneItems,
			groupValue: 'open',
			groupField: 'status',
			collection,
			filtered: false,
			members: [],
			tagSuggestions: [],
			onClose: () => {},
			onMove: () => {},
		},
	});
}

const moveEntry = () => screen.queryByText(/Move all to/i);

afterEach(cleanup);

describe('bulk "Move all to" is offered only when the status field can hold a lane key', () => {
	it('offers it for a plain select — the positive control', () => {
		renderMenu(
			collectionWith({ key: 'status', type: 'select', options: ['open', 'done', 'blocked'] }),
		);
		expect(moveEntry()).not.toBeNull();
	});

	it('lists every other option as a destination, and not the lane it is on', () => {
		// Guards the filter itself: a fix that withheld everything would pass
		// the control above only by accident of the entry still rendering.
		renderMenu(
			collectionWith({ key: 'status', type: 'select', options: ['open', 'done', 'blocked'] }),
		);
		screen.getByText(/Move all to/i).click();
		return Promise.resolve().then(() => {
			expect(screen.queryByText('Done')).not.toBeNull();
			expect(screen.queryByText('Blocked')).not.toBeNull();
		});
	});

	it('withholds it for a status retyped to multi_select that kept its options', () => {
		// THE FILED CASE. `multi_select` is groupable, so no grouping refusal
		// fires and BoardView's existing gate lets the menu through.
		renderMenu(
			collectionWith({ key: 'status', type: 'multi_select', options: ['open', 'done', 'blocked'] }),
		);
		expect(moveEntry()).toBeNull();
	});

	it('withholds it for a status retyped to number that kept its options', () => {
		// Not covered by `moveTargets.length > 0`: the options are still there.
		renderMenu(
			collectionWith({ key: 'status', type: 'number', options: ['open', 'done', 'blocked'] }),
		);
		expect(moveEntry()).toBeNull();
	});

	it('withholds it for a status retyped to checkbox that kept its options', () => {
		renderMenu(
			collectionWith({ key: 'status', type: 'checkbox', options: ['open', 'done', 'blocked'] }),
		);
		expect(moveEntry()).toBeNull();
	});

	it('still offers it for a string-shaped retype, so the fix is not "withhold on any retype"', () => {
		// `text` holds the lane key perfectly well. A fix keyed on "type !==
		// select" would withhold here and this asserts it does not.
		renderMenu(collectionWith({ key: 'status', type: 'text', options: ['open', 'done'] }));
		expect(moveEntry()).not.toBeNull();
	});
});
