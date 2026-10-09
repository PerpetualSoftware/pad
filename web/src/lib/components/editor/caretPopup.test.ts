import { describe, it, expect } from 'vitest';
import { placePopup } from './caretPopup';

// TASK-2218 (audit C40), from the audit's own measurements.
const viewport = { width: 1440, height: 900 };
const menu = { width: 220, height: 320 };

describe('placePopup', () => {
	it('opens below the caret when it fits', () => {
		const p = placePopup({ left: 300, top: 200, bottom: 220 }, menu, viewport);
		expect(p.top).toBe(224);
		expect(p.bottom).toBeUndefined();
		expect(p.left).toBe(300);
	});

	it('flips above a caret near the bottom (the menu used to open at y=899 in a 900px window)', () => {
		const p = placePopup({ left: 300, top: 875, bottom: 895 }, menu, viewport);
		expect(p.top).toBeUndefined();
		// Anchored by its bottom edge just above the caret's top.
		expect(p.bottom).toBe(900 - (875 - 4));
		expect(p.maxHeight).toBeGreaterThanOrEqual(320);
	});

	it('clamps a popup that would run off the right edge (the picker ran 74px past it)', () => {
		const p = placePopup({ left: 1300, top: 200, bottom: 220 }, { width: 466, height: 200 }, viewport);
		expect(p.left + 466).toBeLessThanOrEqual(1440 - 8);
	});

	it('never places left of the margin', () => {
		const p = placePopup({ left: -40, top: 200, bottom: 220 }, menu, viewport);
		expect(p.left).toBe(8);
	});

	it('stays below, shortened, when there is more room below than above', () => {
		const p = placePopup({ left: 10, top: 100, bottom: 120 }, menu, { width: 400, height: 380 });
		expect(p.top).toBe(124);
		expect(p.maxHeight).toBe(380 - 8 - 124);
	});
});
