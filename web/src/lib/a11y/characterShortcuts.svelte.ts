import { browser } from '$app/env';

/**
 * Single-character keyboard shortcuts (BUG-3465, WCAG 2.1.4).
 *
 * A shortcut made of a printable key alone (`c`, `?`, `j`, `+` …) fires on a
 * stray press, a speech-input word or a sticky key, so WCAG 2.1.4 requires a
 * way to turn it off unless it is active only while its own component has
 * focus. Every such binding goes through `characterKey()`, which answers null
 * while the switch is off, so one setting governs all of them.
 * `characterShortcutCensus.test.ts` fails on a raw single-character key
 * comparison anywhere else, so a new binding cannot bypass it.
 *
 * Per device (localStorage), like the other shell preferences. Per account
 * waits for a general preferences store.
 */

const STORAGE_KEY = 'pad-character-shortcuts';

function readEnabled(): boolean {
	if (!browser) return true;
	try {
		return localStorage.getItem(STORAGE_KEY) !== 'off';
	} catch {
		return true;
	}
}

let enabled = $state(readEnabled());

export const characterShortcuts = {
	get enabled() {
		return enabled;
	},
	set(value: boolean) {
		enabled = value;
		if (!browser) return;
		try {
			if (value) localStorage.removeItem(STORAGE_KEY);
			else localStorage.setItem(STORAGE_KEY, 'off');
		} catch {
			// Storage blocked: the switch still holds for this page's lifetime.
		}
	}
};

/**
 * The printable key this event carries, if it may act as a single-character
 * shortcut: the switch is on, and no Ctrl, Meta or Alt is held (those make it
 * a browser or OS shortcut, or on macOS an Option-composed character). Shift
 * is allowed, since `?` and `+` need it on most layouts. Null otherwise.
 */
export function characterKey(e: KeyboardEvent): string | null {
	if (!enabled) return null;
	if (e.ctrlKey || e.metaKey || e.altKey) return null;
	if (e.key.length !== 1 || e.key === ' ') return null;
	return e.key;
}
