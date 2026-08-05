import { describe, expect, it } from 'vitest';
import { nextViewerResourceGen, viewerResourceKey } from './viewerResource';

/**
 * The generation half of the viewer's lifecycle rule (TASK-2428).
 *
 * Extracted from `ItemDetail` precisely so these sequences can be stated: a
 * host-level test cannot distinguish a correct generation from one that simply
 * never moves, because all it ever sees is the number it was handed.
 *
 * The sequences below are the ones `ItemDetail` actually produces — the key is
 * `itemMatchesRef ? {ws, item.id} : ''`, which reads empty mid-switch.
 */
describe('viewerResourceKey', () => {
	it('is empty until the loaded item matches the requested ref', () => {
		expect(viewerResourceKey('ws', 'item-a', false)).toBe('');
		expect(viewerResourceKey('ws', undefined, true)).toBe('');
		expect(viewerResourceKey('ws', 'item-a', true)).not.toBe('');
	});

	it('includes the workspace, so the same ref in two workspaces is two resources', () => {
		// A reused pane can navigate ws1→ws2 carrying `?item=<ref>` where both
		// workspaces own that ref (IDEA-2135).
		expect(viewerResourceKey('ws1', 'item-a', true)).not.toBe(
			viewerResourceKey('ws2', 'item-a', true)
		);
	});

	it('contains no control characters', () => {
		// A NUL separator makes the whole source file binary to grep — caught
		// in review on the first draft of this rule.
		expect(viewerResourceKey('ws', 'item-a', true)).toMatch(/^[\x20-\x7e]+$/);
	});
});

describe('nextViewerResourceGen', () => {
	/** Replays a key sequence, returning the generation after each step. */
	function replay(keys: string[]): number[] {
		let gen = 0;
		let lastKey = '';
		return keys.map((key) => {
			const next = nextViewerResourceGen(key, lastKey, gen);
			if (next !== gen) {
				lastKey = key;
				gen = next;
			}
			return gen;
		});
	}

	it('advances once when the first item loads', () => {
		expect(replay(['', 'ws A'])).toEqual([0, 1]);
	});

	it('does NOT advance on a same-item reload', () => {
		// The whole point: a collection schema edit refetches the item already
		// on screen. `loadGeneration` moves; this must not.
		expect(replay(['ws A', 'ws A', 'ws A'])).toEqual([1, 1, 1]);
	});

	it('counts an A→B switch once, through the empty mid-switch key', () => {
		expect(replay(['ws A', '', 'ws B'])).toEqual([1, 1, 2]);
	});

	it('does not count a boundary flap that lands back on the same item', () => {
		expect(replay(['ws A', '', 'ws A'])).toEqual([1, 1, 1]);
	});

	it('counts A→B→A as three distinct resources, not two', () => {
		// Rapid j/k paging. Coming BACK to A is a resource change too: whatever
		// was open belonged to B.
		expect(replay(['ws A', '', 'ws B', '', 'ws A'])).toEqual([1, 1, 2, 2, 3]);
	});

	it('counts a cross-workspace switch that keeps the ref', () => {
		expect(replay(['ws1 A', 'ws2 A'])).toEqual([1, 2]);
	});

	it('never advances on the empty key alone', () => {
		expect(replay(['', '', ''])).toEqual([0, 0, 0]);
	});
});
