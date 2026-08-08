import { describe, it, expect } from 'vitest';
import {
	THUMB_LONG_EDGE,
	AUTO_LOAD_MAX_PIXELS,
	classify,
	decideFirstRequest,
	servedOriginal,
} from './viewerLoading';

describe('viewerLoading constants', () => {
	it('mirror the server bounds by value', () => {
		expect(THUMB_LONG_EDGE).toBe(1024);
		expect(AUTO_LOAD_MAX_PIXELS).toBe(8_000_000);
	});
});

describe('classify — pixels only, never bytes', () => {
	it('is small at or under the pixel ceiling, large strictly above', () => {
		expect(classify(2000, 2000)).toBe('small'); // 4 MP
		expect(classify(4000, 2000)).toBe('small'); // exactly 8 MP — still small
		expect(classify(4000, 2001)).toBe('large'); // just over
		expect(classify(5000, 5000)).toBe('large'); // 25 MP
	});

	it('classifies a SMALL-byte, LARGE-pixel image as large (bytes are never consulted)', () => {
		// A heavily-compressed 50 MP JPEG is a few MB on the wire but 200 MiB
		// decoded — `large` is about the decoded cost, not the download.
		expect(classify(10000, 5000)).toBe('large'); // 50 MP
	});

	it('yields `unknown` (a third value, NOT `large`) when a dimension is missing', () => {
		expect(classify(null, 900)).toBe('unknown');
		expect(classify(900, null)).toBe('unknown');
		expect(classify(null, null)).toBe('unknown');
		expect(classify(undefined, 900)).toBe('unknown');
		// Explicitly distinct from `large`.
		expect(classify(null, 900)).not.toBe('large');
	});

	it('treats non-finite / non-positive dimensions as `unknown`', () => {
		expect(classify(0, 900)).toBe('unknown');
		expect(classify(-1, 900)).toBe('unknown');
		expect(classify(NaN, 900)).toBe('unknown');
		expect(classify(Infinity, 900)).toBe('unknown');
	});
});

describe('decideFirstRequest — the DR-5b decision table', () => {
	it('small, long edge <= 1024: the original directly, either platform', () => {
		expect(decideFirstRequest(800, 600, 'desktop')).toEqual({ variant: 'original' });
		expect(decideFirstRequest(800, 600, 'mobile')).toEqual({ variant: 'original' });
		expect(decideFirstRequest(1024, 700, 'desktop')).toEqual({ variant: 'original' }); // exactly 1024
	});

	it('small, long edge > 1024: thumb-md — upgrade on desktop, not on mobile', () => {
		// 2000x100 = 200k px (small) but long edge 2000 > 1024.
		expect(decideFirstRequest(2000, 100, 'desktop')).toEqual({ variant: 'thumb-md', upgrade: true });
		expect(decideFirstRequest(2000, 100, 'mobile')).toEqual({ variant: 'thumb-md', upgrade: false });
	});

	it('large (dims known): desktop thumb-md+upgrade, mobile nothing (tap)', () => {
		expect(decideFirstRequest(5000, 5000, 'desktop')).toEqual({ variant: 'thumb-md', upgrade: true });
		expect(decideFirstRequest(5000, 5000, 'mobile')).toEqual({ variant: null });
	});

	it('unknown dims: desktop the original DIRECTLY (one request), mobile nothing (tap)', () => {
		// The unknown-desktop cell must not request a thumb — a bounded fallback
		// original would defeat the detector and cause a second request.
		expect(decideFirstRequest(null, 900, 'desktop')).toEqual({ variant: 'original' });
		expect(decideFirstRequest(null, null, 'desktop')).toEqual({ variant: 'original' });
		expect(decideFirstRequest(null, 900, 'mobile')).toEqual({ variant: null });
	});
});

describe('servedOriginal — the fallback detector', () => {
	it('is true when the decoded long edge exceeds the thumbnail bound', () => {
		// The thumb request was served the original (variant absent).
		expect(servedOriginal(2048, 1200)).toBe(true);
		expect(servedOriginal(1200, 2048)).toBe(true); // long edge on either axis
		expect(servedOriginal(1025, 10)).toBe(true);
	});

	it('is false when the decoded bitmap fits the thumbnail bound (a real thumb)', () => {
		expect(servedOriginal(1024, 768)).toBe(false); // exactly at the bound
		expect(servedOriginal(1000, 500)).toBe(false);
	});

	it('is false (never crashes) on missing / degenerate dimensions', () => {
		expect(servedOriginal(null, null)).toBe(false);
		expect(servedOriginal(0, 0)).toBe(false);
		expect(servedOriginal(NaN, undefined)).toBe(false);
	});
});
