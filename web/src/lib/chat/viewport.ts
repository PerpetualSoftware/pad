// The on-screen keyboard's height, from visualViewport (TASK-4044 PR 3b), as
// Editor.svelte does for its toolbar: on a phone the layout viewport keeps its
// size while the visual one shrinks, so a bottom-anchored composer is covered.

/** Pixels of the layout viewport's bottom covered by the keyboard; 0 when none. */
export function keyboardInset(innerHeight: number, vv: { height: number; offsetTop: number }): number {
	return Math.max(0, Math.round(innerHeight - vv.height - vv.offsetTop));
}

/** Report the inset now and on every viewport change; returns the unsubscribe. */
export function watchKeyboardInset(onChange: (px: number) => void): () => void {
	if (typeof window === 'undefined' || !window.visualViewport) return () => {};
	const vv = window.visualViewport;
	const update = () => onChange(keyboardInset(window.innerHeight, vv));
	vv.addEventListener('resize', update);
	vv.addEventListener('scroll', update);
	update();
	return () => {
		vv.removeEventListener('resize', update);
		vv.removeEventListener('scroll', update);
	};
}
