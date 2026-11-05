// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
// TASK-4099: the in-memory key mode shared by every composer in a tab.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { inputMode } from './inputMode.svelte';

function setPointer(fine: boolean | null) {
	if (fine === null) {
		Reflect.deleteProperty(window, 'matchMedia');
		return;
	}
	Object.defineProperty(window, 'matchMedia', {
		configurable: true,
		writable: true,
		value: (q: string) => ({ matches: q.includes('pointer: fine') ? fine : false, media: q })
	});
}

beforeEach(() => inputMode.reset());
afterEach(() => Reflect.deleteProperty(window, 'matchMedia'));

describe('inputMode', () => {
	it('a fine primary pointer defaults to hardware, a coarse one or none to touch', () => {
		setPointer(true);
		expect(inputMode.hardware).toBe(true);
		expect(inputMode.offered).toBe(false);
		setPointer(false);
		expect(inputMode.hardware).toBe(false);
		expect(inputMode.offered).toBe(true);
		setPointer(null);
		expect(inputMode.hardware).toBe(false);
	});

	it('a touch entry turns hardware off and the setting is offered, whatever the pointer', () => {
		setPointer(true);
		inputMode.enterByTouch();
		expect(inputMode.hardware).toBe(false);
		expect(inputMode.offered).toBe(true);
	});

	it('choosing hardware after a touch entry turns it back on; reset clears everything', () => {
		setPointer(true);
		inputMode.enterByTouch();
		inputMode.setHardware(true);
		expect(inputMode.hardware).toBe(true);
		inputMode.setHardware(false);
		expect(inputMode.hardware).toBe(false);
		inputMode.reset();
		expect(inputMode.hardware).toBe(true);
	});

	it('a touch entry clears a chosen override', () => {
		setPointer(false);
		inputMode.setHardware(true);
		expect(inputMode.hardware).toBe(true);
		inputMode.enterByTouch();
		expect(inputMode.hardware).toBe(false);
	});
});
