import { describe, it, expect, afterEach, vi } from 'vitest';
import { captureListAnchor, applyListAnchor, holdListAnchor } from './listScrollHandoff';

// jsdom has no layout, so a fake list lays its rows out from `scrollTop`:
// the scroller's band is [bandTop, bandTop + height) in the viewport and row i
// sits at bandTop + i * rowHeight - scrollTop.
function makeList(opts: { rows: number; rowHeight: number; bandTop: number; height: number; keyOf?: (i: number) => string }) {
	const scroller = document.createElement('div');
	let scrollTop = 0;
	Object.defineProperty(scroller, 'scrollTop', {
		get: () => scrollTop,
		set: (v: number) => {
			scrollTop = Math.max(0, v);
		},
	});
	const rect = (top: number, h: number) => ({ top, bottom: top + h, height: h, left: 0, right: 100, width: 100, x: 0, y: top, toJSON() {} }) as DOMRect;
	scroller.getBoundingClientRect = () => rect(opts.bandTop, opts.height);
	const state = { rowHeight: opts.rowHeight };
	for (let i = 0; i < opts.rows; i++) {
		const row = document.createElement('a');
		row.dataset.itemKey = opts.keyOf ? opts.keyOf(i) : `K-${i}`;
		row.dataset.itemSlug = `slug-${i}`;
		row.getBoundingClientRect = () => rect(opts.bandTop + i * state.rowHeight - scrollTop, state.rowHeight);
		scroller.appendChild(row);
	}
	document.body.appendChild(scroller);
	return { scroller, state };
}

afterEach(() => {
	document.body.innerHTML = '';
	vi.useRealTimers();
});

describe('captureListAnchor', () => {
	it('anchors on the preferred row when it is visible (by key or slug)', () => {
		const { scroller } = makeList({ rows: 50, rowHeight: 100, bandTop: 50, height: 700 });
		scroller.scrollTop = 600; // rows 6.. visible; row 9 at 50 + 900 - 600 = 350
		expect(captureListAnchor(scroller, 'K-9')).toEqual({ key: 'K-9', clientTop: 350 });
		expect(captureListAnchor(scroller, 'slug-9')).toEqual({ key: 'K-9', clientTop: 350 });
	});

	it('falls back to the top visible row when the preferred row is off screen or absent', () => {
		const { scroller } = makeList({ rows: 50, rowHeight: 100, bandTop: 50, height: 700 });
		scroller.scrollTop = 650; // row 6 spans 0..100 in the viewport: its bottom is below the band top
		expect(captureListAnchor(scroller, 'K-40')).toEqual({ key: 'K-6', clientTop: 0 });
		expect(captureListAnchor(scroller, 'nope')).toEqual({ key: 'K-6', clientTop: 0 });
		expect(captureListAnchor(scroller, null)).toEqual({ key: 'K-6', clientTop: 0 });
	});

	it('returns null for a list that is not laid out', () => {
		const { scroller } = makeList({ rows: 5, rowHeight: 100, bandTop: 50, height: 0 });
		expect(captureListAnchor(scroller, 'K-1')).toBeNull();
		const empty = makeList({ rows: 0, rowHeight: 100, bandTop: 50, height: 700 });
		expect(captureListAnchor(empty.scroller, null)).toBeNull();
	});
});

describe('applyListAnchor', () => {
	it('puts the anchor row back at its viewport top in a scroller with different row heights', () => {
		const { scroller } = makeList({ rows: 50, rowHeight: 140, bandTop: 50, height: 700 });
		// Captured in the other layout: K-9 at clientTop 350. Here K-9 is at
		// 50 + 9 * 140 = 1310 with scrollTop 0, so scrollTop must become 960.
		expect(applyListAnchor(scroller, { key: 'K-9', clientTop: 350 })).toBe(960);
		expect(scroller.children[9].getBoundingClientRect().top).toBe(350);
	});

	it('returns null and leaves the scroller alone when the row is not in it', () => {
		const { scroller } = makeList({ rows: 5, rowHeight: 100, bandTop: 50, height: 700 });
		scroller.scrollTop = 30;
		expect(applyListAnchor(scroller, { key: 'gone', clientTop: 10 })).toBeNull();
		expect(scroller.scrollTop).toBe(30);
	});

	it('holdListAnchor reports that nothing was applied (null) for a missing row or scroller', () => {
		const { scroller } = makeList({ rows: 5, rowHeight: 100, bandTop: 50, height: 700 });
		expect(holdListAnchor(() => scroller, { key: 'gone', clientTop: 10 })).toBeNull();
		expect(holdListAnchor(() => null, { key: 'K-1', clientTop: 10 })).toBeNull();
	});
});

describe('holdListAnchor', () => {
	it('re-applies while the layout settles, and stops once something else moves the scroller', () => {
		let raf: FrameRequestCallback[] = [];
		vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => (raf.push(cb), raf.length));
		vi.stubGlobal('cancelAnimationFrame', () => {});
		const flush = () => {
			const q = raf;
			raf = [];
			q.forEach((cb) => cb(0));
		};
		const { scroller, state } = makeList({ rows: 50, rowHeight: 100, bandTop: 50, height: 700 });
		holdListAnchor(() => scroller, { key: 'K-9', clientTop: 350 });
		expect(scroller.scrollTop).toBe(600);
		state.rowHeight = 120; // rows reflow a frame later
		flush();
		expect(scroller.children[9].getBoundingClientRect().top).toBe(350);
		scroller.scrollTop = 5; // the reader scrolls
		state.rowHeight = 150;
		flush();
		expect(scroller.scrollTop).toBe(5);
		vi.unstubAllGlobals();
	});
});
