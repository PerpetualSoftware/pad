// BUG-3050 U1, door A4: the raw editor never starts from a stale body.
// Every leg forces one of the fallbacks the item pane can hit when switching
// to raw markdown, and asserts that a seed which is not the live document is
// REFUSED (a raw save would send it back, tokenless, over a tab's edits).
import { describe, expect, it } from 'vitest';
import { rawSeedDecision } from './rawSeed';

const MARKED = 'applied_pending_flush';

describe('rawSeedDecision', () => {
	it('no provider or editor (no live read), item marked: refused', () => {
		expect(rawSeedDecision({ liveNow: undefined, lastFlushed: null, stored: 'old', contentState: MARKED }).refuse).toBe(true);
	});

	it('the flush threw part-way (no live read), item marked: refused', () => {
		expect(rawSeedDecision({ liveNow: undefined, lastFlushed: 'A', stored: 'old', contentState: MARKED }).refuse).toBe(true);
	});

	it('the flush loop hit its cap with the editor newer than the last flush: refused, marker or not', () => {
		expect(rawSeedDecision({ liveNow: 'B', lastFlushed: 'A', stored: 'A', contentState: undefined }).refuse).toBe(true);
	});

	it('a deduped flush while another tab typed during the await: refused (codex R2)', () => {
		// The loop saw "A", the flush deduped against the stored "A", and the
		// editor now holds the other tab's "A+typed".
		expect(rawSeedDecision({ liveNow: 'A+typed', lastFlushed: 'A', stored: 'A', contentState: MARKED }).refuse).toBe(true);
	});

	it("this tab's OWN edits mark the item, but a live read equal to the seed is current: allowed", () => {
		const d = rawSeedDecision({ liveNow: 'mine', lastFlushed: 'mine', stored: 'older', contentState: MARKED });
		expect(d).toEqual({ seed: 'mine', refuse: false });
	});

	it('no live read and an unmarked item: the stored body is current, allowed', () => {
		expect(rawSeedDecision({ liveNow: undefined, lastFlushed: null, stored: 'body', contentState: undefined })).toEqual({
			seed: 'body',
			refuse: false,
		});
	});
});
