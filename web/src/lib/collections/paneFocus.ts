/**
 * Focus helpers for the collection detail pane (PLAN-2105 / TASK-2122).
 *
 * Split out of `[collection]/+page.svelte` so the DOM-selection and
 * focus-trap-cycle logic is unit testable without a full component mount
 * (mirrors `paneUrlParams` / `boardNav`). The `.svelte` side owns the effects
 * that install/tear these down; the selection + cycle helpers are pure DOM math.
 * The one exception is {@link handoffFocus}, which deliberately MOVES focus (its
 * whole job) — the `.svelte` side calls it from an effect, but it is exported
 * here so it lives with the focus math it builds on.
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
 * Portalled / self-trapping surfaces that legitimately overlay EITHER pane
 * region — a native modal `<dialog>`, an ARIA dialog / menu / listbox, or the
 * editor's imperative block context menu (no ARIA role, matched by class). They
 * own their own focus + keyboard and sit in NEITHER the master column nor the
 * pane region (most portal out to `<body>`), so both call sites treat a focus /
 * pointer landing on them as "not a region event":
 *   • the mobile focus trap must not hijack their Tab or yank focus off them, and
 *   • the full-page host's focus-follows-editing classifier (PLAN-2179 / DR-2)
 *     must not read a focus / pointerdown on them as a master↔pane switch.
 * ONE definition shared by both call sites (PaneHost's mobile trap + the host
 * route's activePane classifier) so the two can't drift.
 *
 * `[role="dialog"]:not(.item-pane)` EXCLUDES the pane region itself (TASK-2131):
 * the mobile overlay now carries `role="dialog"` (a true modal), but it is the
 * region these consumers operate ON, not a foreign overlay to defer to — without
 * the `:not`, `closest()` from any in-pane element would match the pane and
 * wrongly mark the whole pane exempt (killing the mobile Tab trap and confusing
 * the classifier). A genuinely nested dialog opened FROM the pane still matches.
 *
 * The attachment viewer (TASK-2429) matches the ARIA branch, and MUST: it is a
 * body-portaled `role="dialog"` that runs its own Tab trap and key handling, so
 * both consumers have to leave it alone — exactly the case this set exists for.
 * That is the opposite of the route ESC guards, which have to look PAST it
 * (`hasForeignEscapeOwner`, `$lib/a11y/viewerBackdrop`) because its Escape is on
 * the shared stack. Same attribute, two different questions; audited as part of
 * TASK-2429's collision sweep.
 */
export const PANE_EXEMPT_SURFACE_SELECTOR =
	'dialog, [role="dialog"]:not(.item-pane), [role="menu"], [role="listbox"], .block-context-menu';

/** True when `el` (or an ancestor) is one of the {@link PANE_EXEMPT_SURFACE_SELECTOR} overlays. */
export function inExemptSurface(el: Element | null | undefined): boolean {
	return !!el?.closest?.(PANE_EXEMPT_SURFACE_SELECTOR);
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
 * Keep focus INSIDE a modal surface when a focused control leaves it — is
 * removed from the DOM, or becomes `disabled` (PLAN-2392 / TASK-2456).
 *
 * `aria-modal="true"` promises focus never escapes the surface while it is open,
 * yet a conditionally-rendered control that had focus drops focus to `<body>`
 * when it unmounts, and a control that becomes `disabled` does the same on a real
 * engine — landing focus BEHIND the surface's own inerted background. Neither the
 * Tab trap (fires only on a later Tab) nor the teardown restore (only at close)
 * repairs that gap; this does, moving focus to the surface's stable fallback.
 *
 * Two call shapes, one helper (the house pattern from
 * `editor/attachment-image.ts` — blur the departing control, focus its
 * replacement):
 *
 *  - BEFORE an imperative removal/disable, pass the `departing` control: if it
 *    currently holds focus it is blurred and focus moves to the fallback. This
 *    is the shape TASK-2459's retry and TASK-2460's tap-to-load use (a real
 *    engine drops focus to `<body>` the instant a focused control is disabled,
 *    so the handoff must run while the control is still focused).
 *  - AFTER a reactive removal (a Svelte `{#if}` dropped the control), pass no
 *    `departing`: if focus has ALREADY fallen out of `container`, it is pulled
 *    back within the same synchronous flush, so `<body>` is never observably
 *    focused. A focus still resting on a live control inside `container` is left
 *    untouched.
 *
 * The fallback is the first tabbable control (for the viewer, its close button),
 * else `container` itself — the same target entry focus uses, so it is always
 * reachable even mid-load with no other control yet.
 */
export function handoffFocus(
	container: HTMLElement,
	departing: Element | null = null,
	isVisible: (el: HTMLElement) => boolean = isFocusableVisible,
): void {
	if (typeof document === 'undefined') return;
	const active = document.activeElement;
	// Imperative (departing given): act only while that control still owns focus,
	// so a handoff for a control the user already left is a no-op. Reactive (no
	// departing): act only once focus has left the surface — a live focus inside
	// it is fine and must not be yanked to the fallback.
	const leaving =
		departing !== null
			? active === departing
			: active === null || active === document.body || !container.contains(active);
	if (!leaving) return;
	if (departing instanceof HTMLElement) departing.blur();
	// EXCLUDE `departing` from the candidates: in the imperative shape it is still
	// enabled and in the DOM at call time (the caller disables/removes it AFTER
	// this), so a departing control that is the first — or only — tabbable would
	// otherwise be re-selected here and then dropped to <body> the instant the
	// caller disables it. When it is the only one, fall through to the container.
	const fallback =
		paneFocusables(container, isVisible).find((el) => el !== departing) ?? container;
	fallback.focus({ preventScroll: true });
	// Verify it took, then drop to the container — the same verified-restore
	// pattern the viewer uses on close. `paneFocusables`' visibility filter is
	// geometry-only, so it cannot see that a candidate sits under `inert` /
	// `visibility: hidden` (a real engine refuses focus there); if the preferred
	// target refuses, the container (a `tabindex="-1"` surface root) still keeps
	// focus INSIDE the surface rather than letting it fall to <body>.
	if (fallback !== container && document.activeElement !== fallback) {
		container.focus({ preventScroll: true });
	}
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
