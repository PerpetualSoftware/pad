import { describe, it, expect, beforeEach, vi } from 'vitest';
import {
	keepRefusedRawDraft,
	readRefusedRawDraft,
	clearRefusedRawDraft,
	refusedRawDraftOffer,
} from './refusedRawDraft';

// BUG-3230 U0: the kept text an unload raw save could not store.
describe('refusedRawDraft', () => {
	beforeEach(() => {
		localStorage.clear();
		vi.restoreAllMocks();
	});

	it('keeps, reads back with its time, and clears', () => {
		keepRefusedRawDraft('u1', 'i1', 'kept text', 1234);
		expect(readRefusedRawDraft('u1', 'i1')).toEqual({ markdown: 'kept text', savedAt: 1234 });
		clearRefusedRawDraft('u1', 'i1');
		expect(readRefusedRawDraft('u1', 'i1')).toBeNull();
	});

	it('is per user: another account on this browser is never offered it', () => {
		keepRefusedRawDraft('u1', 'i1', 'mine', 1);
		expect(readRefusedRawDraft('u2', 'i1')).toBeNull();
		expect(readRefusedRawDraft('u1', 'i2')).toBeNull();
	});

	it('clear with onlyIf leaves newer text a later unload kept', () => {
		keepRefusedRawDraft('u1', 'i1', 'older', 1);
		keepRefusedRawDraft('u1', 'i1', 'newer', 2);
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
});
