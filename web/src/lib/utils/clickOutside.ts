/**
 * Instance-scoped outside-click action (PLAN-2290 Phase 2).
 *
 * Listens on capture-phase `pointerdown`, NOT `click`: a row handler that
 * mutates state can detach the row before a window `click` listener runs,
 * making `node.contains(target)` false and slamming the menu shut on the
 * same interaction (BUG-2281's detach hazard — previously papered over
 * with stopPropagation on every row). pointerdown fires before any click
 * handler mutates state, so containment checks see the live DOM.
 *
 * Instance-scoped by design: each consumer checks ITS OWN node (+ extras),
 * never a shared class — sibling menus must not cross-close each other.
 */
interface ClickOutsideOptions {
	enabled?: boolean;
	/** Called when a pointerdown lands outside the node and all extras. */
	onOutside: () => void;
	/** Additional containers that count as "inside" (trigger element,
	 *  portaled sub-popovers like EmojiPicker). */
	extra?: () => (Element | null | undefined)[];
}

export function clickOutside(node: HTMLElement, options: ClickOutsideOptions) {
	let opts = options;

	function onPointerDown(e: PointerEvent) {
		if (opts.enabled === false) return;
		const target = e.target as Element | null;
		if (!target) return;
		if (node.contains(target)) return;
		for (const el of opts.extra?.() ?? []) {
			if (el && el.contains(target)) return;
		}
		opts.onOutside();
	}

	document.addEventListener('pointerdown', onPointerDown, true);
	return {
		update(next: ClickOutsideOptions) {
			opts = next;
		},
		destroy() {
			document.removeEventListener('pointerdown', onPointerDown, true);
		}
	};
}
