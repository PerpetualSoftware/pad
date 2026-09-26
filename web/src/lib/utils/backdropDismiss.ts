/**
 * Backdrop-dismiss action (BUG-3229).
 *
 * A surface whose backdrop WRAPS its content (a native modal <dialog>, whose
 * ::backdrop dispatches to the dialog element, or an overlay div around a
 * panel) cannot dismiss on `click` alone. A click is dispatched to the nearest
 * common ancestor of the pointerdown and pointerup targets, so a text
 * selection that starts inside the content and is released on the backdrop
 * produces a click whose target IS the backdrop, and the surface closes under
 * the user's drag. The content's `stopPropagation` never runs, because the
 * event never passes through the content.
 *
 * The rule: dismiss only when BOTH the pointerdown and the pointerup landed on
 * the backdrop element itself, and the click that follows does too. A press
 * that starts in the content, or ends there, dismisses nothing.
 *
 * A backdrop that is a SIBLING of the content does not need this (the common
 * ancestor of a drag from content to backdrop is their parent, so the
 * backdrop's own click never fires), but it is harmless there.
 */
interface BackdropDismissOptions {
	enabled?: boolean;
	onDismiss: () => void;
}

export function backdropDismiss(node: HTMLElement, options: BackdropDismissOptions) {
	let opts = options;
	// Where each live pointer went down, by pointerId, so one pointer's press on
	// the backdrop cannot vouch for another pointer's drag out of the content.
	const downOnBackdrop = new Map<number, boolean>();
	// Whether the most recent release completed a press that began AND ended on
	// the backdrop. The click that follows a release reads it.
	let lastPressOnBackdrop = false;

	function onPointerDown(e: PointerEvent) {
		downOnBackdrop.set(e.pointerId, e.target === node);
	}
	function onPointerUp(e: PointerEvent) {
		lastPressOnBackdrop = downOnBackdrop.get(e.pointerId) === true && e.target === node;
		downOnBackdrop.delete(e.pointerId);
	}
	function onPointerCancel(e: PointerEvent) {
		downOnBackdrop.delete(e.pointerId);
		lastPressOnBackdrop = false;
	}
	function onClick(e: MouseEvent) {
		const both = lastPressOnBackdrop;
		lastPressOnBackdrop = false;
		if (opts.enabled === false) return;
		if (e.target !== node || !both) return;
		opts.onDismiss();
	}

	// Capture phase, so content that stops a pointer event's propagation cannot
	// leave a stale reading from an earlier press.
	node.addEventListener('pointerdown', onPointerDown, true);
	node.addEventListener('pointerup', onPointerUp, true);
	node.addEventListener('pointercancel', onPointerCancel, true);
	node.addEventListener('click', onClick);
	return {
		update(next: BackdropDismissOptions) {
			opts = next;
		},
		destroy() {
			node.removeEventListener('pointerdown', onPointerDown, true);
			node.removeEventListener('pointerup', onPointerUp, true);
			node.removeEventListener('pointercancel', onPointerCancel, true);
			node.removeEventListener('click', onClick);
		}
	};
}
