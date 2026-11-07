// The visible box of the screen while the on-screen keyboard is up (TASK-4315 PR 4).
// On a phone the layout viewport keeps its size while visualViewport shrinks, so a
// fixed surface (the Chat sheet, the standalone thread) must fit INSIDE the visual
// viewport rather than offset its own bottom edge by a guessed inset. The Composer
// no longer lifts itself: the surface that contains it is the one that moves.

export type FitBox = { top: number; height: number };

/** The visual viewport's box in layout coordinates. */
export function fitBox(vv: { height: number; offsetTop: number }): FitBox {
	return { top: Math.round(vv.offsetTop), height: Math.round(vv.height) };
}

/** Report the box now and on every viewport change (one frame at most); returns the unsubscribe. */
export function watchFitBox(onChange: (box: FitBox) => void): () => void {
	if (typeof window === 'undefined' || !window.visualViewport) return () => {};
	const vv = window.visualViewport;
	let frame = 0;
	const measure = () => {
		frame = 0;
		onChange(fitBox(vv));
	};
	const schedule = () => {
		if (!frame) frame = requestAnimationFrame(measure);
	};
	vv.addEventListener('resize', schedule);
	vv.addEventListener('scroll', schedule);
	window.addEventListener('orientationchange', schedule);
	measure();
	return () => {
		vv.removeEventListener('resize', schedule);
		vv.removeEventListener('scroll', schedule);
		window.removeEventListener('orientationchange', schedule);
		if (frame) cancelAnimationFrame(frame);
		frame = 0;
	};
}
