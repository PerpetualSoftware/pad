import { beforeEach, describe, expect, it } from 'vitest';
import {
	__resetArchivedItemRegistry,
	consumeItemRestored,
	markItemArchived,
	reconcileArchivedState,
} from './archivedItemRegistry';

/**
 * BUG-2509. The two cases that motivate a registry rather than a per-mount edge
 * latch are "navigating away from an archived item is not a restore" and
 * "a restore that happens while the pane is elsewhere still has to be
 * reconciled" — the last two tests here.
 */
describe('archivedItemRegistry', () => {
	beforeEach(() => {
		__resetArchivedItemRegistry();
	});

	const show = (itemId: string, archived: boolean, matched = true) =>
		reconcileArchivedState({ matched, archived, itemId });

	it('announces when an item seen archived is next seen live', () => {
		expect(show('A', true).restoredItemId).toBeNull();
		expect(show('A', false).restoredItemId).toBe('A');
	});

	it('announces once — a second host showing the same item does not re-announce', () => {
		show('A', true);
		expect(show('A', false).restoredItemId).toBe('A');
		expect(show('A', false).restoredItemId).toBeNull();
	});

	it('says nothing about an item that was never archived', () => {
		expect(show('A', false).restoredItemId).toBeNull();
	});

	it('re-arms after a second archive', () => {
		show('A', true);
		expect(show('A', false).restoredItemId).toBe('A');
		show('A', true);
		expect(show('A', false).restoredItemId).toBe('A');
	});

	it('is a level while the item stays archived', () => {
		expect(show('A', true).restoredItemId).toBeNull();
		expect(show('A', true).restoredItemId).toBeNull();
	});

	it('ignores an unmatched host — mid-switch it still holds the previous row', () => {
		// Marking here would attribute item A's archived state to whatever the
		// route now names; announcing here is the false-positive the edge latch had.
		expect(show('A', true, false).restoredItemId).toBeNull();
		expect(show('A', false, false).restoredItemId).toBeNull();
		// …and nothing was recorded, so a later genuine view of live A is quiet.
		expect(show('A', false).restoredItemId).toBeNull();
	});

	it('ignores an empty id', () => {
		expect(show('', true).restoredItemId).toBeNull();
		expect(show('', false).restoredItemId).toBeNull();
	});

	/** THE FALSE POSITIVE the per-mount edge latch produced. */
	it('does not announce when navigating away from an archived item', () => {
		show('A', true);
		// The host stops matching (route now names B) while `item` still holds A…
		expect(show('A', true, false).restoredItemId).toBeNull();
		// …and then B loads, live. Neither step is a restore of anything.
		expect(show('B', false).restoredItemId).toBeNull();
	});

	/** THE MISS the per-mount edge latch could not see. */
	it('announces a restore that happened while the pane was showing another item', () => {
		show('A', true); // archived, in front of the user
		show('B', false); // user navigates to B; A is restored elsewhere meanwhile
		// Coming back to a now-live A: no archived→live edge was ever observed by
		// this tab, but its cached `missing` for A's attachments is still poisoned.
		expect(show('A', false).restoredItemId).toBe('A');
	});

	it('exposes the primitives the reconciler is built from', () => {
		markItemArchived('A');
		expect(consumeItemRestored('A')).toBe(true);
		expect(consumeItemRestored('A')).toBe(false);
		expect(consumeItemRestored('')).toBe(false);
	});
});
