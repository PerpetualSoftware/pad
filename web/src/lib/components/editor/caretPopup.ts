// Where the editor's caret popups (the slash menu and the [[ link picker)
// go (TASK-2218, audit C40).
//
// Both opened at `coords.bottom + 4` with no viewport check. Appending to a
// document, the normal caret position, put the 320px slash menu almost
// entirely below the viewport (measured: top at y=899 in a 900px window), and
// the link picker ran 74px off the right edge of a split pane. Now: below the
// caret when it fits, else above it when more room is there, and always
// clamped inside the viewport horizontally.
//
// When the popup sits ABOVE the caret it is anchored by its bottom edge, so a
// list that shrinks as the user filters stays attached to the caret instead of
// leaving a gap.

export interface CaretRect {
	left: number;
	top: number;
	bottom: number;
}

export interface PopupPlacement {
	left: number;
	/** Set when the popup opens below the caret. */
	top?: number;
	/** Set when it opens above: distance from the viewport's bottom edge. */
	bottom?: number;
	/** The height it may use without leaving the viewport. */
	maxHeight: number;
}

const GAP = 4;
const MARGIN = 8;

export function placePopup(
	caret: CaretRect,
	size: { width: number; height: number },
	viewport: { width: number; height: number }
): PopupPlacement {
	const below = viewport.height - MARGIN - (caret.bottom + GAP);
	const above = caret.top - GAP - MARGIN;
	const left = Math.max(MARGIN, Math.min(caret.left, viewport.width - MARGIN - size.width));
	if (size.height <= below || below >= above) {
		return { left, top: caret.bottom + GAP, maxHeight: Math.max(0, below) };
	}
	return { left, bottom: viewport.height - (caret.top - GAP), maxHeight: Math.max(0, above) };
}

/**
 * Svelte action: places a fixed-position popup at `caret`, measuring the
 * popup's own size. Re-places when `caret` changes; the orientation is decided
 * from the popup's FULL height (its CSS max-height), so it does not flip back
 * and forth as filtering shortens the list.
 */
export function atCaret(node: HTMLElement, caret: CaretRect) {
	function apply(c: CaretRect) {
		node.style.top = '';
		node.style.bottom = '';
		node.style.maxHeight = '';
		const cap = parseFloat(getComputedStyle(node).maxHeight);
		const height = Number.isFinite(cap) ? cap : node.offsetHeight;
		const p = placePopup(c, { width: node.offsetWidth, height }, { width: window.innerWidth, height: window.innerHeight });
		node.style.left = `${p.left}px`;
		if (p.top !== undefined) node.style.top = `${p.top}px`;
		if (p.bottom !== undefined) node.style.bottom = `${p.bottom}px`;
		if (Number.isFinite(cap) && p.maxHeight < cap) node.style.maxHeight = `${p.maxHeight}px`;
	}
	apply(caret);
	return {
		update(c: CaretRect) {
			apply(c);
		}
	};
}
