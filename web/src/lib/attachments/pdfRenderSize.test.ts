import { describe, it, expect } from 'vitest';
import { pageRenderSize, stepZoom, PDF_MAX_PIXELS, PDF_MAX_DPR, PDF_ZOOM_STEPS } from './pdfRenderSize';

// TASK-4075 Q4: the two canvas caps, each a test.
describe('pageRenderSize', () => {
	it('uses the device pixel ratio for the backing store and none of it for the CSS box', () => {
		const s = pageRenderSize(612, 792, 612, 2);
		expect(s).toMatchObject({ cssWidth: 612, cssHeight: 792, pixelWidth: 1224, pixelHeight: 1584 });
	});

	it('caps the DPR at 2 (a DPR-3 phone gets the DPR-2 backing store)', () => {
		const three = pageRenderSize(612, 792, 612, 3);
		const two = pageRenderSize(612, 792, 612, 2);
		expect(PDF_MAX_DPR).toBe(2);
		expect(three.pixelWidth).toBe(two.pixelWidth);
		expect(three.pixelHeight).toBe(two.pixelHeight);
	});

	it('never goes below DPR 1, and treats a missing ratio as 1', () => {
		expect(pageRenderSize(100, 100, 100, 0).pixelWidth).toBe(100);
		expect(pageRenderSize(100, 100, 100, 0.5).pixelWidth).toBe(100);
	});

	it('caps the backing store at 16 megapixels whatever the zoom asks for', () => {
		// 3x zoom of a 1200 css px sheet at DPR 2: 7200 x 9318 = 67 MP uncapped.
		const s = pageRenderSize(612, 792, 3600, 2);
		expect(s.pixelWidth * s.pixelHeight).toBeLessThanOrEqual(PDF_MAX_PIXELS);
		// ...and it is not shrunk further than it must be.
		expect(s.pixelWidth * s.pixelHeight).toBeGreaterThan(PDF_MAX_PIXELS * 0.99);
		// The layout is untouched by the cap.
		expect(s.cssWidth).toBe(3600);
		expect(s.cssHeight).toBeCloseTo(792 * (3600 / 612), 5);
	});

	it('holds the aspect ratio under the cap', () => {
		const s = pageRenderSize(612, 792, 3600, 2);
		expect(s.pixelWidth / s.pixelHeight).toBeCloseTo(612 / 792, 2);
	});

	it('caps a hostile page size (a 14400 x 14400 pt page) too', () => {
		const s = pageRenderSize(14400, 14400, 800, 2);
		expect(s.pixelWidth * s.pixelHeight).toBeLessThanOrEqual(PDF_MAX_PIXELS);
	});

	it('does not touch a page already under the cap', () => {
		const s = pageRenderSize(612, 792, 800, 1);
		expect(s.pixelWidth).toBe(800);
	});

	it('never yields a zero-sized canvas for a degenerate page', () => {
		const s = pageRenderSize(1, 100000, 800, 2);
		expect(s.pixelWidth).toBeGreaterThanOrEqual(1);
		expect(s.pixelHeight).toBeGreaterThanOrEqual(1);
		expect(s.pixelWidth * s.pixelHeight).toBeLessThanOrEqual(PDF_MAX_PIXELS);
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
