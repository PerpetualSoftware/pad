import { browser } from '$app/env';
import { uiStore } from '$lib/stores/ui.svelte';

// Which keys send in the Chat composer (TASK-4099, DOC-4316 section 5).
// The preference lives on the preferences page and is kept per device.
// With no stored choice, the primary pointer decides: fine means Enter sends,
// coarse means Enter adds a line. Touch on the box overrides the fine default,
// but preserves a stored choice. While a soft keyboard is up, Enter adds a line
// whatever is stored; when it goes down, a stored choice applies again. Turning
// the setting off removes the stored choice, and the pointer default applies at once.
// Nothing here ever sends by itself.
const STORAGE_KEY = 'pad-chat-enter-sends';

function readStored(): boolean {
	if (!browser) return false;
	try {
		return localStorage.getItem(STORAGE_KEY) === 'on';
	} catch {
		return false;
	}
}

class InputMode {
	#stored = $state(readStored());
	#touched = $state(false);

	#finePointer(): boolean {
		return typeof window !== 'undefined' && !!window.matchMedia?.('(pointer: fine)').matches;
	}

	/** True when unmodified Enter sends. */
	get hardware(): boolean {
		if (uiStore.keyboardVisible) return false;
		return this.#stored || (this.#finePointer() && !this.#touched);
	}

	/** The per-device choice, even while a soft keyboard overrides it for this page. */
	get stored(): boolean {
		return this.#stored;
	}

	/** Offer the setting when hardware is not the default, or a stored choice can be turned off. */
	get offered(): boolean {
		return !this.#finePointer() || this.#touched || this.#stored;
	}

	/** A tap overrides only the pointer default, preserving an explicit stored choice. */
	enterByTouch(): void {
		if (!this.#stored) this.#touched = true;
	}

	/**
	 * A soft keyboard came up: with nothing stored, touch mode holds for this page. A stored
	 * choice is paused by `hardware` reading uiStore.keyboardVisible directly, so no copy of
	 * that state can outlive the composer that saw it.
	 */
	softKeyboardUp(): void {
		this.#touched = true;
	}

	/** The person's explicit choice. Off removes it: the pointer default applies, now and after a reload. */
	setHardware(on: boolean): void {
		this.#stored = on;
		this.#touched = false;
		if (!browser) return;
		try {
			if (on) localStorage.setItem(STORAGE_KEY, 'on');
			else localStorage.removeItem(STORAGE_KEY);
		} catch {
			// Storage blocked: the choice still holds for this page's lifetime.
		}
	}

	/** Back to the pointer default (tests; nothing else needs it). */
	reset(): void {
		this.#stored = false;
		this.#touched = false;
	}
}

export const inputMode = new InputMode();
