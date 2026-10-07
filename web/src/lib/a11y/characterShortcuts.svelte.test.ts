// Runs in the jsdom vitest project (filename ends `.svelte.test.ts`).
import { describe, it, expect, afterEach, vi } from 'vitest';
import { characterKey, characterShortcuts } from './characterShortcuts.svelte';

function key(init: KeyboardEventInit): KeyboardEvent {
	return new KeyboardEvent('keydown', init);
}

afterEach(() => {
	characterShortcuts.set(true);
	vi.restoreAllMocks();
});

describe('characterKey (BUG-3465)', () => {
	it('answers a printable key, with Shift allowed', () => {
		expect(characterKey(key({ key: 'c' }))).toBe('c');
		expect(characterKey(key({ key: '?', shiftKey: true }))).toBe('?');
	});

	it('declines Ctrl, Meta and Alt chords, non-printable keys and Space', () => {
		for (const init of [
			{ key: 'c', ctrlKey: true },
			{ key: 'c', metaKey: true },
			{ key: 'c', altKey: true },
			{ key: 'Escape' },
			{ key: 'ArrowDown' },
			{ key: ' ' }
		]) {
			expect({ init, got: characterKey(key(init)) }).toEqual({ init, got: null });
		}
	});

	it('declines everything while the switch is off, and the switch is stored per device', () => {
		characterShortcuts.set(false);
		expect(characterKey(key({ key: 'c' }))).toBeNull();
		expect(localStorage.getItem('pad-character-shortcuts')).toBe('off');
		characterShortcuts.set(true);
		expect(characterKey(key({ key: 'c' }))).toBe('c');
		expect(localStorage.getItem('pad-character-shortcuts')).toBeNull();
	});

	it('a blocked storage does not break the switch', () => {
		vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
			throw new Error('blocked');
		});
		characterShortcuts.set(false);
		expect(characterShortcuts.enabled).toBe(false);
		expect(characterKey(key({ key: 'c' }))).toBeNull();
	});
});
