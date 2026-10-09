/**
 * Return focus to where it was before a surface opened (TASK-2235).
 *
 * Modal and BottomSheet each kept the same two-line bookkeeping: remember
 * `document.activeElement` when the surface opens, and on close (or teardown
 * while open) focus it again if it is still in the document. This is that
 * bookkeeping, once, for every surface that takes focus.
 *
 * A factory rather than a Svelte action, deliberately. An action's `destroy`
 * runs when its element leaves the DOM, which for a surface with an outro
 * transition is AFTER the transition: focus would sit on a departing element
 * (or <body>) for the length of the animation, and a keyboard user pressing a
 * key in that window would act on nothing. The owners call `restore()` at the
 * moment they decide to close, from the same effect that reads `open`.
 *
 * Plain closure state, not `$state`: it is read and written only inside the
 * owner's effects and teardown, never in reactive position (CONVE-1688).
 */
export interface FocusReturn {
	/** Remember the focused element. Keeps the FIRST capture until `restore()`,
	 *  so an effect that re-runs while open cannot overwrite the trigger with
	 *  something inside the surface. */
	save(): void;
	/** Focus the remembered element if it is still in the document, then forget it. */
	restore(): void;
}

export function createFocusReturn(): FocusReturn {
	let saved: HTMLElement | null = null;
	let captured = false;
	return {
		save() {
			if (captured) return;
			captured = true;
			saved = (document.activeElement as HTMLElement | null) ?? null;
		},
		restore() {
			const el = saved;
			saved = null;
			captured = false;
			if (el && el !== document.body && document.contains(el)) {
				el.focus();
			}
		}
	};
}
