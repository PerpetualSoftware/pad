import { describe, it, expect } from 'vitest';
import { shouldAdoptCollection, type AdoptCollectionInput } from './adoptCollection';

function input(over: Partial<AdoptCollectionInput>): AdoptCollectionInput {
	return {
		fetchedId: 'coll-x',
		liveItemCollectionId: 'coll-x',
		currentCollectionId: 'coll-x',
		genFresh: true,
		...over,
	};
}

describe('shouldAdoptCollection', () => {
	it('admits a fresh same-collection refresh (the common case)', () => {
		expect(shouldAdoptCollection(input({}))).toBe(true);
	});

	it('rejects a generation-stale same-collection refresh (newest-started wins)', () => {
		expect(shouldAdoptCollection(input({ genFresh: false }))).toBe(false);
	});

	it('THE FILED RACE: vetoes a pre-move source-collection snapshot after the live item moved', () => {
		// loadData(X) was in flight; an SSE move adopted the item into Y
		// (itemGen fences the item, so the live item stays moved). The
		// continuation resolves with X — generation-stale, and the old
		// escape hatch (currentCollectionId !== fetchedId) would have
		// admitted it. The semantic veto must fire.
		expect(
			shouldAdoptCollection(
				input({
					fetchedId: 'coll-x',
					liveItemCollectionId: 'coll-y',
					currentCollectionId: 'coll-y',
					genFresh: false,
				}),
			),
		).toBe(false);
	});

	it('vetoes even a generation-FRESH snapshot that disagrees with the live item', () => {
		// Freshness cannot make a semantically wrong snapshot right —
		// e.g. the reused-slug hazard: an SSE refresh fetched by slug got
		// a FOREIGN collection because the slug was re-owned post-rename.
		expect(
			shouldAdoptCollection(
				input({
					fetchedId: 'coll-foreign',
					liveItemCollectionId: 'coll-x',
					currentCollectionId: 'coll-x',
					genFresh: true,
				}),
			),
		).toBe(false);
	});

	it('admits the legitimate cross-collection correction even when generation-stale', () => {
		// The old escape hatch's valid half: loadData resolved the item's
		// REAL collection while a stale refresh for the previously shown
		// collection had bumped the generation. The live item agrees with
		// the fetched collection, so the correction lands.
		expect(
			shouldAdoptCollection(
				input({
					fetchedId: 'coll-y',
					liveItemCollectionId: 'coll-y',
					currentCollectionId: 'coll-x',
					genFresh: false,
				}),
			),
		).toBe(true);
	});

	it('first load (no collection shown yet): live item agreement admits', () => {
		expect(
			shouldAdoptCollection(
				input({ currentCollectionId: null, liveItemCollectionId: 'coll-x', genFresh: false }),
			),
		).toBe(true);
	});

	it('no live item: generation order is the only signal (fresh admits)', () => {
		expect(
			shouldAdoptCollection(
				input({ liveItemCollectionId: null, currentCollectionId: 'coll-w', fetchedId: 'coll-x', genFresh: true }),
			),
		).toBe(true);
	});

	it('no live item: generation-stale cross-collection write is rejected', () => {
		// Without an item to anchor on there is no semantic basis to
		// override ordering — the hatch's blanket admit was the defect.
		expect(
			shouldAdoptCollection(
				input({ liveItemCollectionId: null, currentCollectionId: 'coll-w', fetchedId: 'coll-x', genFresh: false }),
			),
		).toBe(false);
	});

	it('chained-move burst: an older refresh for an intermediate collection is vetoed', () => {
		// X→Y→Z: the Y refresh resolves last while the live item is on Z.
		expect(
			shouldAdoptCollection(
				input({
					fetchedId: 'coll-y',
					liveItemCollectionId: 'coll-z',
					currentCollectionId: 'coll-z',
					genFresh: false,
				}),
			),
		).toBe(false);
	});
});
