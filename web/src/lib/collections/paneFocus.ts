/**
 * Focus helpers for the collection detail pane (PLAN-2105 / TASK-2122).
 *
 * Split out of `[collection]/+page.svelte` so the DOM-selection and
 * focus-trap-cycle logic is unit testable without a full component mount
 * (mirrors `paneUrlParams` / `boardNav`). The `.svelte` side owns the effects
 * that install/tear these down; this module is pure, side-effect-free DOM math.
 */

/**
 * Tabbable-element selector. Excludes `tabindex="-1"` (programmatic-only focus
 * targets, e.g. the pane region container itself) and disabled form controls.
 * The Tiptap editor body is a `contenteditable="true"` div, so it's included.
 */
export const PANE_FOCUSABLE_SELECTOR =
	'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), ' +
	'textarea:not([disabled]), [tabindex]:not([tabindex="-1"]), [contenteditable="true"]';

/**
 * Default visibility test for a focusable candidate. `offsetParent === null`
 * catches `display:none` (and detached) subtrees; the `getClientRects` fallback
 * catches the `position:fixed` case where `offsetParent` is null despite the
 * element being on-screen. Injected in {@link paneFocusables} so tests (jsdom
 * has no layout) can supply a deterministic stub.
 */
export function isFocusableVisible(el: HTMLElement): boolean {
	return el.offsetParent !== null || el.getClientRects().length > 0;
}

/**
 * Visible, enabled, tabbable descendants of `container`, in DOM order — the
 * trap cycle set.
 */
export function paneFocusables(
	container: HTMLElement,
	isVisible: (el: HTMLElement) => boolean = isFocusableVisible,
): HTMLElement[] {
	return Array.from(
		container.querySelectorAll<HTMLElement>(PANE_FOCUSABLE_SELECTOR),
	).filter(
		// A `tabindex="-1"` element is programmatic-focus-only — never a Tab stop,
		// even when it's a natively-focusable tag (`<button tabindex="-1">`) that
		// the tag clauses of the selector would otherwise pick up.
		(el) => el.getAttribute('tabindex') !== '-1' && isVisible(el),
	);
}

/**
 * Where a Tab / Shift+Tab should send focus while the pane is TRAPPING (the
 * mobile full-screen overlay). Returns the element to focus, or `null` to let
 * the browser's native Tab move stand (focus is mid-list and staying inside the
 * pane — no wrap needed).
 *
 * Wrapping rules (standard modal trap):
 *  • Forward Tab off the LAST focusable → wrap to the first.
 *  • Shift+Tab off the FIRST focusable (or off the region container itself) →
 *    wrap to the last.
 *  • Focus somehow OUTSIDE the pane → pull it back to the edge (first on a
 *    forward Tab, last on a back Tab).
 *  • Empty pane (no focusables yet — loading) → keep focus on the container.
 */
export function nextTrapTarget(
	focusables: HTMLElement[],
	active: Element | null,
	shiftKey: boolean,
	container: HTMLElement,
): HTMLElement | null {
	if (focusables.length === 0) return container;
	const first = focusables[0];
	const last = focusables[focusables.length - 1];
	const inside = active != null && container.contains(active);
	if (shiftKey) {
		if (active === first || active === container || !inside) return last;
		return null;
	}
	if (active === last || !inside) return first;
	return null;
}

/**
 * Resolve the element to return focus to when the pane CLOSES (TASK-2122): the
 * row that opened / last drove the pane.
 *
 * The paned item's row always carries the `.focused` marker (kept in sync as
 * the pane follows j/k), so it's the canonical "row that opened it" even after
 * paging A→C. List/board rows ARE the anchor (`.item-card` is an `<a>`); table
 * rows are a `<div>` wrapping a `.title-link` anchor — so fall through to the
 * first focusable inside the row there. When no row is present (a deep-linked
 * item that isn't in the current filtered list), fall back to the captured
 * trigger element if it's still in the document.
 */
export function resolvePaneReturnTarget(
	root: Document | HTMLElement,
	captured: HTMLElement | null,
): HTMLElement | null {
	const row = root.querySelector<HTMLElement>('.item-card.focused, .table-row.focused');
	if (row) {
		if (row.matches('a[href]')) return row;
		const inner = row.querySelector<HTMLElement>('a[href], button, [tabindex]');
		if (inner) return inner;
	}
	if (captured && captured.isConnected) return captured;
	return null;
}
