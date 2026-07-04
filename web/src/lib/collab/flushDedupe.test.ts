import { describe, it, expect } from 'vitest';
import { shouldDedupeEditorSpace } from './flushDedupe';

describe('shouldDedupeEditorSpace', () => {
	it('dedupes a pure no-edit view: no flush yet this session, markdown matches the seed', () => {
		expect(shouldDedupeEditorSpace(null, 'seed text', 'seed text')).toBe(true);
	});

	it('does not dedupe when no flush yet but markdown differs from the seed (a real edit)', () => {
		expect(shouldDedupeEditorSpace(null, 'seed text', 'edited text')).toBe(false);
	});

	it('does not dedupe when no seed was ever captured (e.g. the lazy-seed never fired this session)', () => {
		expect(shouldDedupeEditorSpace(null, null, 'seed text')).toBe(false);
	});

	// Revert-safety (BUG-1899's original guarantee): once a real flush has
	// landed this session, the server holds edited content — reverting the
	// editor back to the original seed text must still PATCH, not dedupe,
	// because the storage-space compare against `lastFlushedContent` (not
	// this predicate) is what's authoritative from that point on.
	it('does NOT dedupe once a flush has already happened this session, even if markdown reverts to the seed', () => {
		expect(shouldDedupeEditorSpace('some flushed content', 'seed text', 'seed text')).toBe(false);
	});
});
