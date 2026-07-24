/**
 * Portal a node to <body> (or the nearest open <dialog>, so portaled UI
 * stays interactive inside native dialogs). Extracted from the hand-copied
 * versions in ItemActionsMenu and EmojiPickerButton (PLAN-2290 Phase 2).
 *
 * Why portal at all: triggers that live under `content-visibility: auto`
 * containment (board/list cards) clip absolutely-positioned descendants —
 * a fixed-position panel portaled to the body escapes that.
 */
export function portal(node: HTMLElement) {
	const dialog = node.closest('dialog');
	(dialog ?? document.body).appendChild(node);
	return {
		destroy() {
			node.remove();
		}
	};
}
