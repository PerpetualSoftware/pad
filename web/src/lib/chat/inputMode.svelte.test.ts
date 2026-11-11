// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
// The per-device key preference and tab mode shared by every composer.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
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

beforeEach(() => {
	localStorage.clear();
	inputMode.reset();
});
afterEach(() => {
	vi.restoreAllMocks();
	Reflect.deleteProperty(window, 'matchMedia');
});

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

	it('a stored choice survives touch entry', () => {
		setPointer(false);
		inputMode.setHardware(true);
		expect(inputMode.hardware).toBe(true);
		inputMode.enterByTouch();
		expect(inputMode.hardware).toBe(true);
		expect(inputMode.stored).toBe(true);
		expect(localStorage.getItem('pad-chat-enter-sends')).toBe('on');
	});

	it('a soft keyboard drops the tab to touch mode and keeps the stored choice', () => {
		setPointer(true);
		inputMode.setHardware(true);
		inputMode.softKeyboardUp();
		expect(inputMode.hardware).toBe(false);
		expect(inputMode.stored).toBe(true);
		expect(localStorage.getItem('pad-chat-enter-sends')).toBe('on');
		inputMode.enterByTouch();
		expect(inputMode.hardware).toBe(false);
		inputMode.setHardware(true);
		expect(inputMode.hardware).toBe(true);
	});

	it('offers a stored choice even on a fine pointer so it can be turned off', () => {
		setPointer(true);
		inputMode.setHardware(true);
		expect(inputMode.offered).toBe(true);
		inputMode.setHardware(false);
		expect(inputMode.stored).toBe(false);
		expect(inputMode.hardware).toBe(false);
		expect(inputMode.offered).toBe(true);
		expect(localStorage.getItem('pad-chat-enter-sends')).toBeNull();
	});

	it('reset clears the in-memory stored choice and tab flags', () => {
		setPointer(false);
		inputMode.setHardware(true);
		inputMode.softKeyboardUp();
		inputMode.reset();
		expect(inputMode.stored).toBe(false);
		expect(inputMode.hardware).toBe(false);
		setPointer(true);
		expect(inputMode.hardware).toBe(true);
		expect(inputMode.offered).toBe(false);
	});

	it('blocked writes and removals keep the choice for the page lifetime', () => {
		setPointer(false);
		vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw Error('blocked'); });
		vi.spyOn(Storage.prototype, 'removeItem').mockImplementation(() => { throw Error('blocked'); });
		inputMode.setHardware(true);
		expect(inputMode.stored).toBe(true);
		expect(inputMode.hardware).toBe(true);
		inputMode.setHardware(false);
		expect(inputMode.stored).toBe(false);
		expect(inputMode.hardware).toBe(false);
	});

	it('blocked reads fall back to the pointer default without breaking import', async () => {
		vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => { throw Error('blocked'); });
		vi.resetModules();
		const { inputMode: reloaded } = await import('./inputMode.svelte');
		setPointer(false);
		expect(reloaded.stored).toBe(false);
		expect(reloaded.hardware).toBe(false);
		setPointer(true);
		expect(reloaded.hardware).toBe(true);
	});
});
