import { describe, it, expect } from 'vitest';
import { pageRenderSize, stepZoom, PDF_MAX_PIXELS, PDF_MAX_SIDE, PDF_MAX_DPR, PDF_ZOOM_STEPS } from './pdfRenderSize';

// TASK-4075 Q4: the two canvas caps, each a test.
describe('pageRenderSize', () => {
	it('uses the device pixel ratio for the backing store and none of it for the CSS box', () => {
		const s = pageRenderSize(612, 792, 612, 2)!;
		expect(s).toMatchObject({ cssWidth: 612, cssHeight: 792, pixelWidth: 1224, pixelHeight: 1584 });
	});

	it('caps the DPR at 2 (a DPR-3 phone gets the DPR-2 backing store)', () => {
		const three = pageRenderSize(612, 792, 612, 3)!;
		const two = pageRenderSize(612, 792, 612, 2)!;
		expect(PDF_MAX_DPR).toBe(2);
		expect(three.pixelWidth).toBe(two.pixelWidth);
		expect(three.pixelHeight).toBe(two.pixelHeight);
	});

	it('never goes below DPR 1', () => {
		expect(pageRenderSize(100, 100, 100, 1)!.pixelWidth).toBe(100);
		expect(pageRenderSize(100, 100, 100, 0.5)!.pixelWidth).toBe(100);
	});

	it('caps the backing store at 16 megapixels whatever the zoom asks for', () => {
		// 3x zoom of a 1200 css px sheet at DPR 2: 7200 x 9318 = 67 MP uncapped.
		const s = pageRenderSize(612, 792, 3600, 2)!;
		expect(s.pixelWidth * s.pixelHeight).toBeLessThanOrEqual(PDF_MAX_PIXELS);
		// ...and it is not shrunk further than it must be.
		expect(s.pixelWidth * s.pixelHeight).toBeGreaterThan(PDF_MAX_PIXELS * 0.99);
		// The layout is untouched by the cap.
		expect(s.cssWidth).toBe(3600);
		expect(s.cssHeight).toBeCloseTo(792 * (3600 / 612), 5);
	});

	it('holds the aspect ratio under the cap', () => {
		const s = pageRenderSize(612, 792, 3600, 2)!;
		expect(s.pixelWidth / s.pixelHeight).toBeCloseTo(612 / 792, 2);
	});

	it('caps a hostile page size (a 14400 x 14400 pt page zoomed to 8000 css px, 256 MP uncapped) at 16 MP', () => {
		// 8000 css px at DPR 2 is 16000 x 16000 = 256 MP: over the area cap, while the
		// longest side (16000) is UNDER the side cap, so only the area cap can bring it down.
		const s = pageRenderSize(14400, 14400, 8000, 2)!;
		expect(s.pixelWidth * s.pixelHeight).toBeLessThanOrEqual(PDF_MAX_PIXELS);
		expect(s.pixelWidth * s.pixelHeight).toBeGreaterThan(PDF_MAX_PIXELS * 0.99);
		expect(s.pixelWidth).toBe(4000);
	});

	it('does not touch a page already under the cap', () => {
		const s = pageRenderSize(612, 792, 800, 1)!;
		expect(s.pixelWidth).toBe(800);
	});

	it('never yields a zero-sized canvas for a degenerate page', () => {
		const s = pageRenderSize(1, 100000, 800, 2)!;
		expect(s.pixelWidth).toBeGreaterThanOrEqual(1);
		expect(s.pixelHeight).toBeGreaterThanOrEqual(1);
		expect(s.pixelWidth * s.pixelHeight).toBeLessThanOrEqual(PDF_MAX_PIXELS);
	});
});

describe('pageRenderSize refuses input with no honest size (F3)', () => {
	it.each([
		['Infinity page width', [Infinity, 792, 800, 2]],
		['NaN page height', [612, NaN, 800, 2]],
		['zero page width', [0, 792, 800, 2]],
		['negative page height', [612, -792, 800, 2]],
		['Infinity css width', [612, 792, Infinity, 2]],
		['NaN css width', [612, 792, NaN, 2]],
		['zero css width', [612, 792, 0, 2]],
		['negative css width', [612, 792, -5, 2]],
		['Infinity dpr', [612, 792, 800, Infinity]],
		['NaN dpr', [612, 792, 800, NaN]],
		['zero dpr', [612, 792, 800, 0]],
		['negative dpr', [612, 792, 800, -2]]
	] as [string, [number, number, number, number]][])('%s returns null', (_n, args) => {
		expect(pageRenderSize(...args)).toBeNull();
	});
});

describe('pageRenderSize caps each side (H1)', () => {
	// The area cap alone admits both: 10 x 14400 pt at fit-width is ~1600 x 2.3M px
	// (area 3.7 G px before any cap bites), and 14400 x 10 pt zoomed to 24000 css px
	// is 48000 x 3 px (area 144 K px, far UNDER 16 MP).
	it.each([
		['10 x 14400 pt (tall strip)', 10, 14400, 800],
		['14400 x 10 pt (wide strip, zoomed)', 14400, 10, 24000]
	])('%s: both sides <= PDF_MAX_SIDE, area <= 16 MP, longest side at the cap', (_n, pw, ph, css) => {
		const s = pageRenderSize(pw, ph, css, 2)!;
		expect(PDF_MAX_SIDE).toBe(16384);
		expect(s.pixelWidth).toBeLessThanOrEqual(PDF_MAX_SIDE);
		expect(s.pixelHeight).toBeLessThanOrEqual(PDF_MAX_SIDE);
		expect(s.pixelWidth * s.pixelHeight).toBeLessThanOrEqual(PDF_MAX_PIXELS);
		expect(Math.max(s.pixelWidth, s.pixelHeight)).toBeGreaterThanOrEqual(PDF_MAX_SIDE - 1);
		// Uniform scaling: the ratio survives (to the floor's one pixel).
		expect(Math.abs(s.pixelWidth / s.pixelHeight - pw / ph) / (pw / ph)).toBeLessThan(0.2);
	});
});

describe('stepZoom', () => {
	it('walks the steps and clamps at both ends', () => {
		expect(stepZoom(1, 1)).toBe(1.25);
		expect(stepZoom(1, -1)).toBe(0.75);
		expect(stepZoom(PDF_ZOOM_STEPS[PDF_ZOOM_STEPS.length - 1], 1)).toBe(3);
		expect(stepZoom(PDF_ZOOM_STEPS[0], -1)).toBe(0.5);
	});
	it('snaps an off-grid value first', () => {
		expect(stepZoom(1.1, 1)).toBe(1.25);
	});
});
