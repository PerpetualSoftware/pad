import { describe, it, expect, beforeEach, vi } from 'vitest';
import {
	keepRefusedRawDraft,
	readRefusedRawDraft,
	clearRefusedRawDraft,
	refusedRawDraftOffer,
	purgeRefusedRawDraftsForUser,
	sweepExpiredRefusedRawDrafts,
	offerKeptDraft,
	resolveKeptDraft,
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

// BUG-3863: a shown draft is held, so newer kept text cannot replace its only
// stored copy before the user resolves it.
describe('held kept drafts (BUG-3863)', () => {
	const notOwned = () => false;
	const HELD = 'pad:refused-raw-draft:u1:i1:held';
	const PENDING = 'pad:refused-raw-draft:u1:i1';
	beforeEach(() => {
		localStorage.clear();
		vi.restoreAllMocks();
	});

	it('offering a draft from before this tab holds it, and a producer writing after that leaves it alone', () => {
		keepRefusedRawDraft('u1', 'i1', 'D', Date.now(), { seq: 3, body: 'base' });
		const shown = offerKeptDraft('u1', 'i1', 'body', notOwned);
		expect(shown).toMatchObject({ slot: 'held', draft: { markdown: 'D', baseSeq: 3, baseBody: 'base' } });
		expect(localStorage.getItem(PENDING)).toBeNull();
		// The keeper (or any producer) keeps newer text E: the pending slot only.
		keepRefusedRawDraft('u1', 'i1', 'E');
		expect(readRefusedRawDraft('u1', 'i1', Date.now(), 'held')?.markdown).toBe('D');
		// A reload offers D again, from the held slot, and E stays pending.
		expect(offerKeptDraft('u1', 'i1', 'body', notOwned)).toMatchObject({ slot: 'held', draft: { markdown: 'D' } });
		expect(readRefusedRawDraft('u1', 'i1')?.markdown).toBe('E');
	});

	it('the held draft keeps its savedAt (the TTL runs from when it was kept)', () => {
		const t = Date.now() - 1000;
		keepRefusedRawDraft('u1', 'i1', 'D', t);
		expect(offerKeptDraft('u1', 'i1', 'body', notOwned)?.draft.savedAt).toBe(t);
		expect(readRefusedRawDraft('u1', 'i1', Date.now(), 'held')?.savedAt).toBe(t);
	});

	it('a producer clear (onlyIf, pending) never removes a held draft', () => {
		keepRefusedRawDraft('u1', 'i1', 'D');
		offerKeptDraft('u1', 'i1', 'body', notOwned);
		clearRefusedRawDraft('u1', 'i1', 'D');
		clearRefusedRawDraft('u1', 'i1');
		expect(readRefusedRawDraft('u1', 'i1', Date.now(), 'held')?.markdown).toBe('D');
	});

	it("the live keeper's own draft stays pending (lead ruling 1)", () => {
		keepRefusedRawDraft('u1', 'i1', 'mine');
		const owns = vi.fn((md: string) => md === 'mine');
		expect(offerKeptDraft('u1', 'i1', 'body', owns)).toMatchObject({ slot: 'pending', draft: { markdown: 'mine' } });
		expect(owns).toHaveBeenCalledWith('mine');
		expect(localStorage.getItem(HELD)).toBeNull();
		expect(readRefusedRawDraft('u1', 'i1')?.markdown).toBe('mine');
	});

	it('once the held draft is resolved, the pending one is offered next', () => {
		keepRefusedRawDraft('u1', 'i1', 'D');
		offerKeptDraft('u1', 'i1', 'body', notOwned);
		keepRefusedRawDraft('u1', 'i1', 'E');
		resolveKeptDraft('u1', 'i1', 'D');
		expect(localStorage.getItem(HELD)).toBeNull();
		expect(offerKeptDraft('u1', 'i1', 'body', notOwned)).toMatchObject({ slot: 'held', draft: { markdown: 'E' } });
	});

	it('resolving is guarded by the text: a different held draft stays', () => {
		keepRefusedRawDraft('u1', 'i1', 'D');
		offerKeptDraft('u1', 'i1', 'body', notOwned);
		resolveKeptDraft('u1', 'i1', 'not D');
		expect(readRefusedRawDraft('u1', 'i1', Date.now(), 'held')?.markdown).toBe('D');
	});

	it('resolving a pending (keeper-owned) draft removes it there', () => {
		keepRefusedRawDraft('u1', 'i1', 'mine');
		resolveKeptDraft('u1', 'i1', 'mine');
		expect(readRefusedRawDraft('u1', 'i1')).toBeNull();
	});

	it('a held draft equal to the stored body is cleared silently, and the pending one is considered', () => {
		keepRefusedRawDraft('u1', 'i1', 'D');
		offerKeptDraft('u1', 'i1', 'body', notOwned);
		keepRefusedRawDraft('u1', 'i1', 'E');
		expect(offerKeptDraft('u1', 'i1', 'D', notOwned)).toMatchObject({ slot: 'held', draft: { markdown: 'E' } });
		expect(readRefusedRawDraft('u1', 'i1')).toBeNull();
	});

	it('a pending draft equal to the stored body is cleared silently, as before', () => {
		keepRefusedRawDraft('u1', 'i1', 'body');
		expect(offerKeptDraft('u1', 'i1', 'body', notOwned)).toBeNull();
		expect(Object.keys(localStorage)).toEqual([]);
	});

	it('a pending copy of the held text (a hold cut short by a crash) is removed, and the held one shown', () => {
		localStorage.setItem(HELD, JSON.stringify({ markdown: 'D', savedAt: Date.now() }));
		localStorage.setItem(PENDING, JSON.stringify({ markdown: 'D', savedAt: Date.now() }));
		expect(offerKeptDraft('u1', 'i1', 'body', notOwned)).toMatchObject({ slot: 'held', draft: { markdown: 'D' } });
		expect(localStorage.getItem(PENDING)).toBeNull();
	});

	it('while a draft is held, a different pending draft is neither shown nor moved', () => {
		keepRefusedRawDraft('u1', 'i1', 'D');
		offerKeptDraft('u1', 'i1', 'body', notOwned);
		keepRefusedRawDraft('u1', 'i1', 'E');
		offerKeptDraft('u1', 'i1', 'body', notOwned);
		expect(readRefusedRawDraft('u1', 'i1')?.markdown).toBe('E');
		expect(readRefusedRawDraft('u1', 'i1', Date.now(), 'held')?.markdown).toBe('D');
	});

	it('a held write that fails shows the draft from pending, and leaves it there', () => {
		keepRefusedRawDraft('u1', 'i1', 'D');
		vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
			throw new DOMException('full', 'QuotaExceededError');
		});
		expect(offerKeptDraft('u1', 'i1', 'body', notOwned)).toMatchObject({ slot: 'pending', draft: { markdown: 'D' } });
		expect(readRefusedRawDraft('u1', 'i1')?.markdown).toBe('D');
	});

	it('an expired held draft reads as none and is removed', () => {
		localStorage.setItem(HELD, JSON.stringify({ markdown: 'D', savedAt: Date.now() - REFUSED_RAW_DRAFT_TTL_MS - 1 }));
		expect(offerKeptDraft('u1', 'i1', 'body', notOwned)).toBeNull();
		expect(localStorage.getItem(HELD)).toBeNull();
	});

	// Lead condition (a): a held draft never outlives sign-out or the TTL.
	it('the sign-out purge removes held drafts', () => {
		keepRefusedRawDraft('u1', 'i1', 'D');
		offerKeptDraft('u1', 'i1', 'body', notOwned);
		keepRefusedRawDraft('u1', 'i1', 'E');
		purgeRefusedRawDraftsForUser('u1');
		expect(Object.keys(localStorage)).toEqual([]);
	});

	it('the boot sweep removes an expired held draft and keeps a fresh one', () => {
		const now = 2_000_000_000_000;
		localStorage.setItem(HELD, JSON.stringify({ markdown: 'D', savedAt: now - REFUSED_RAW_DRAFT_TTL_MS - 1 }));
		localStorage.setItem('pad:refused-raw-draft:u1:i2:held', JSON.stringify({ markdown: 'F', savedAt: now - 1000 }));
		sweepExpiredRefusedRawDrafts(now);
		expect(Object.keys(localStorage)).toEqual(['pad:refused-raw-draft:u1:i2:held']);
	});

	// Lead condition (b): the suffix never aliases an item id, and making room
	// never evicts the item's own held draft.
	it("an item id ending in ':held' is refused, so it cannot reach another item's held slot", () => {
		keepRefusedRawDraft('u1', 'i1', 'D');
		offerKeptDraft('u1', 'i1', 'body', notOwned);
		expect(readRefusedRawDraft('u1', 'i1:held')).toBeNull();
		expect(keepRefusedRawDraft('u1', 'i1:held', 'X')).toBe(false);
		clearRefusedRawDraft('u1', 'i1:held');
		resolveKeptDraft('u1', 'i1:held', 'D');
		expect(offerKeptDraft('u1', 'i1:held', 'body', notOwned)).toBeNull();
		expect(readRefusedRawDraft('u1', 'i1', Date.now(), 'held')?.markdown).toBe('D');
		expect(Object.keys(localStorage)).toEqual([HELD]);
	});

	it("making room for a keep evicts another item's draft, never the same item's held one", () => {
		const t = Date.now();
		// The item's own held draft is the oldest entry of this user.
		localStorage.setItem(HELD, JSON.stringify({ markdown: 'D', savedAt: t - 3000 }));
		keepRefusedRawDraft('u1', 'i2', 'other', t - 2000);
		const real = Storage.prototype.setItem;
		let calls = 0;
		vi.spyOn(Storage.prototype, 'setItem').mockImplementation(function (this: Storage, k: string, v: string) {
			calls++;
			if (calls === 1) throw new DOMException('full', 'QuotaExceededError');
			real.call(this, k, v);
		});
		expect(keepRefusedRawDraft('u1', 'i1', 'E', t)).toBe(true);
		expect(Object.keys(localStorage).sort()).toEqual([PENDING, HELD].sort());
	});
});
