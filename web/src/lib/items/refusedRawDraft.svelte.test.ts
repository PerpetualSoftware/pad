import { describe, it, expect, beforeEach, vi } from 'vitest';
import {
	keepRefusedRawDraft,
	readRefusedRawDraft,
	clearRefusedRawDraft,
	refusedRawDraftOffer,
	purgeRefusedRawDraftsForUser,
	sweepExpiredRefusedRawDrafts,
	REFUSED_RAW_DRAFT_TTL_MS,
} from './refusedRawDraft';

// BUG-3230 U0: the kept text an unload raw save could not store.
describe('refusedRawDraft', () => {
	beforeEach(() => {
		localStorage.clear();
		vi.restoreAllMocks();
	});

	it('keeps, reads back with its time, and clears', () => {
		// Times are near now: an entry older than the TTL (BUG-3571 part 2)
		// reads as none.
		const t = Date.now();
		keepRefusedRawDraft('u1', 'i1', 'kept text', t);
		expect(readRefusedRawDraft('u1', 'i1')).toEqual({ markdown: 'kept text', savedAt: t });
		clearRefusedRawDraft('u1', 'i1');
		expect(readRefusedRawDraft('u1', 'i1')).toBeNull();
	});

	it('is per user: another account on this browser is never offered it', () => {
		keepRefusedRawDraft('u1', 'i1', 'mine');
		expect(readRefusedRawDraft('u1', 'i1')?.markdown).toBe('mine');
		expect(readRefusedRawDraft('u2', 'i1')).toBeNull();
		expect(readRefusedRawDraft('u1', 'i2')).toBeNull();
	});

	it('clear with onlyIf leaves newer text a later unload kept', () => {
		keepRefusedRawDraft('u1', 'i1', 'older');
		keepRefusedRawDraft('u1', 'i1', 'newer');
		clearRefusedRawDraft('u1', 'i1', 'older');
		expect(readRefusedRawDraft('u1', 'i1')?.markdown).toBe('newer');
		clearRefusedRawDraft('u1', 'i1', 'newer');
		expect(readRefusedRawDraft('u1', 'i1')).toBeNull();
	});

	it('does nothing without a user or item id', () => {
		keepRefusedRawDraft('', 'i1', 'x', 1);
		keepRefusedRawDraft('u1', '', 'x', 1);
		expect(localStorage.length).toBe(0);
		expect(readRefusedRawDraft('', 'i1')).toBeNull();
	});

	it('a malformed entry reads as none', () => {
		keepRefusedRawDraft('u1', 'i1', 'x', 1);
		const key = localStorage.key(0)!;
		localStorage.setItem(key, '{not json');
		expect(readRefusedRawDraft('u1', 'i1')).toBeNull();
		localStorage.setItem(key, JSON.stringify({ markdown: 5, savedAt: 1 }));
		expect(readRefusedRawDraft('u1', 'i1')).toBeNull();
	});

	it('blocked storage never throws into a save', () => {
		vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
			throw new DOMException('blocked', 'SecurityError');
		});
		vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
			throw new DOMException('blocked', 'SecurityError');
		});
		vi.spyOn(Storage.prototype, 'removeItem').mockImplementation(() => {
			throw new DOMException('blocked', 'SecurityError');
		});
		expect(() => keepRefusedRawDraft('u1', 'i1', 'x', 1)).not.toThrow();
		expect(readRefusedRawDraft('u1', 'i1')).toBeNull();
		expect(() => clearRefusedRawDraft('u1', 'i1')).not.toThrow();
		expect(() => clearRefusedRawDraft('u1', 'i1', 'x')).not.toThrow();
	});

	it('offers kept text that differs from the stored body, and clears text equal to it', () => {
		expect(refusedRawDraftOffer(null, 'body')).toBe('none');
		expect(refusedRawDraftOffer({ markdown: 'body', savedAt: 1 }, 'body')).toBe('clear');
		expect(refusedRawDraftOffer({ markdown: 'other', savedAt: 1 }, 'body')).toBe('offer');
	});

	// BUG-3571 part 2: kept drafts now include text kept for a user signed out
	// under it, so they expire, and an explicit sign-out purges the user's.
	it('an entry older than the TTL reads as none and is removed', () => {
		const t = 1_000_000_000_000;
		keepRefusedRawDraft('u1', 'i1', 'old', t);
		expect(readRefusedRawDraft('u1', 'i1', t + REFUSED_RAW_DRAFT_TTL_MS)?.markdown).toBe('old');
		expect(localStorage.length).toBe(1);
		expect(readRefusedRawDraft('u1', 'i1', t + REFUSED_RAW_DRAFT_TTL_MS + 1)).toBeNull();
		expect(localStorage.length).toBe(0);
	});

	it('the TTL is seven days', () => {
		expect(REFUSED_RAW_DRAFT_TTL_MS).toBe(7 * 24 * 60 * 60 * 1000);
	});

	it("purging a user removes every one of their drafts and nobody else's", () => {
		keepRefusedRawDraft('u1', 'i1', 'a');
		keepRefusedRawDraft('u1', 'i2', 'b');
		keepRefusedRawDraft('u1', 'i3', 'c');
		// A user id that the purged one prefixes is a different user.
		keepRefusedRawDraft('u10', 'i1', 'd');
		keepRefusedRawDraft('u2', 'i1', 'e');
		localStorage.setItem('pad-theme', 'dark');
		purgeRefusedRawDraftsForUser('u1');
		expect(readRefusedRawDraft('u1', 'i1')).toBeNull();
		expect(readRefusedRawDraft('u1', 'i2')).toBeNull();
		expect(readRefusedRawDraft('u1', 'i3')).toBeNull();
		expect(readRefusedRawDraft('u10', 'i1')?.markdown).toBe('d');
		expect(readRefusedRawDraft('u2', 'i1')?.markdown).toBe('e');
		expect(localStorage.getItem('pad-theme')).toBe('dark');
	});

	it('purging with no user removes nothing', () => {
		keepRefusedRawDraft('u1', 'i1', 'a');
		purgeRefusedRawDraftsForUser('');
		expect(readRefusedRawDraft('u1', 'i1')?.markdown).toBe('a');
	});

	// Lead follow-up: a boot sweep, since expiry otherwise happens only on read.
	it("the boot sweep removes every user's expired or unreadable drafts and keeps the rest", () => {
		const now = 2_000_000_000_000;
		keepRefusedRawDraft('u1', 'old', 'a', now - REFUSED_RAW_DRAFT_TTL_MS - 1);
		keepRefusedRawDraft('u2', 'old', 'b', now - REFUSED_RAW_DRAFT_TTL_MS - 1);
		keepRefusedRawDraft('u1', 'fresh', 'c', now - 1000);
		keepRefusedRawDraft('u2', 'edge', 'd', now - REFUSED_RAW_DRAFT_TTL_MS);
		localStorage.setItem('pad:refused-raw-draft:u3:bad', '{not json');
		localStorage.setItem('pad-theme', 'dark');
		sweepExpiredRefusedRawDrafts(now);
		const left = Object.keys(localStorage).sort();
		expect(left).toEqual(['pad-theme', 'pad:refused-raw-draft:u1:fresh', 'pad:refused-raw-draft:u2:edge']);
	});
});
