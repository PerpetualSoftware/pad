// Which keys send in the Chat composer (TASK-4099, DOC-4316 section 5, product 11-04).
//
// The browser cannot say whether an Enter came from a physical keyboard, so the
// default follows the PRIMARY pointer: (pointer: fine) is a hardware-keyboard
// setup (Enter sends, Shift+Enter adds a line); (pointer: coarse), or no way to
// ask, is touch (Enter adds a line, the Send button sends). A person with a
// keyboard on a tablet can say so with the Hardware keyboard setting, held in
// memory for the tab only. A touch on the box, or a soft keyboard coming up,
// drops back to touch mode, which also covers a 2-in-1 that reports a fine
// pointer while it is used as a tablet. Nothing here ever sends by itself.
class InputMode {
	/** "Hardware keyboard: Enter sends" chosen in this tab. */
	#chosen = $state(false);
	/** The box was entered by touch (or a soft keyboard opened) since the last choice. */
	#touched = $state(false);

	#finePointer(): boolean {
		return typeof window !== 'undefined' && !!window.matchMedia?.('(pointer: fine)').matches;
	}

	/** True when unmodified Enter sends. */
	get hardware(): boolean {
		return this.#chosen || (this.#finePointer() && !this.#touched);
	}

	/** True when the Hardware keyboard setting is worth showing: hardware is not already the default. */
	get offered(): boolean {
		return !this.#finePointer() || this.#touched;
	}

	/** Touch on the box, or a soft keyboard: Enter stops sending until the person chooses hardware again. */
	enterByTouch(): void {
		this.#touched = true;
		this.#chosen = false;
	}

	/** The person's explicit choice. Off is a choice too: it holds even on a fine pointer. */
	setHardware(on: boolean): void {
		this.#chosen = on;
		this.#touched = !on;
	}

	/** Back to the pointer default (tests; nothing else needs it). */
	reset(): void {
		this.#chosen = false;
		this.#touched = false;
	}
}

export const inputMode = new InputMode();
