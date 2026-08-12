import { describe, expect, it } from 'vitest';
import { parentRestoreEdge } from './parentRestoreEdge';

/**
 * BUG-2509. The case that motivates this helper is the LAST one here: the first
 * version of the emitter latched the bare level, and navigating away from an
 * archived item flips that level exactly as a restore does.
 */
describe('parentRestoreEdge', () => {
	const base = { prevArchivedItemId: '', matched: true, archived: false, itemId: 'A' };

	it('announces when the same matched item goes archived → live', () => {
		expect(parentRestoreEdge({ ...base, prevArchivedItemId: 'A' })).toEqual({
			nextArchivedItemId: '',
			restoredItemId: 'A',
		});
	});

	it('latches the id when a matched item becomes archived, and announces nothing', () => {
		expect(parentRestoreEdge({ ...base, archived: true })).toEqual({
			nextArchivedItemId: 'A',
			restoredItemId: null,
		});
	});

	it('is a level, not an edge, while nothing changes', () => {
		expect(parentRestoreEdge({ ...base, prevArchivedItemId: 'A', archived: true })).toEqual({
			nextArchivedItemId: 'A',
			restoredItemId: null,
		});
		expect(parentRestoreEdge(base)).toEqual({ nextArchivedItemId: '', restoredItemId: null });
	});

	it('a mount on an already-archived item announces nothing (seeded latch)', () => {
		// The component seeds `prevArchivedItemId` from the initial value, so the
		// first run sees no change at all.
		expect(parentRestoreEdge({ ...base, prevArchivedItemId: 'A', archived: true })).toEqual({
			nextArchivedItemId: 'A',
			restoredItemId: null,
		});
	});

	/**
	 * THE REGRESSION. Navigating away from an archived item drops the level the
	 * same way a restore does — `matched` goes false while `item` still holds the
	 * old row — so a level-only latch announced a restore that never happened, at
	 * the cost of a workspace-wide cache invalidation and a round of probes on
	 * every such navigation.
	 */
	it('does NOT announce when navigating away from an archived item', () => {
		expect(
			parentRestoreEdge({ prevArchivedItemId: 'A', matched: false, archived: true, itemId: 'A' })
		).toEqual({ nextArchivedItemId: '', restoredItemId: null });
	});

	it('does NOT announce when a different item loads over an archived one', () => {
		expect(
			parentRestoreEdge({ prevArchivedItemId: 'A', matched: true, archived: false, itemId: 'B' })
		).toEqual({ nextArchivedItemId: '', restoredItemId: null });
	});

	it('does NOT announce when the item goes away entirely', () => {
		expect(
			parentRestoreEdge({ prevArchivedItemId: 'A', matched: false, archived: false, itemId: '' })
		).toEqual({ nextArchivedItemId: '', restoredItemId: null });
	});

	it('announces for B, not A, when B is the one restored after a switch', () => {
		// Switch away from archived A (no announce, latch cleared)…
		const away = parentRestoreEdge({
			prevArchivedItemId: 'A',
			matched: false,
			archived: true,
			itemId: 'A',
		});
		expect(away.restoredItemId).toBeNull();
		// …B loads archived, then is restored.
		const bArchived = parentRestoreEdge({
			prevArchivedItemId: away.nextArchivedItemId,
			matched: true,
			archived: true,
			itemId: 'B',
		});
		expect(bArchived).toEqual({ nextArchivedItemId: 'B', restoredItemId: null });
		expect(
			parentRestoreEdge({
				prevArchivedItemId: bArchived.nextArchivedItemId,
				matched: true,
				archived: false,
				itemId: 'B',
			})
		).toEqual({ nextArchivedItemId: '', restoredItemId: 'B' });
	});

	it('never announces for an empty id', () => {
		expect(
			parentRestoreEdge({ prevArchivedItemId: '', matched: true, archived: true, itemId: '' })
		).toEqual({ nextArchivedItemId: '', restoredItemId: null });
	});
});
