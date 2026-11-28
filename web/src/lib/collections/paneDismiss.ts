// Click-outside-to-collapse a split pane (IDEA-2148), shared by the collection
// route's list column and home (TASK-4902). While the pane is open on the
// DESKTOP split, a primary click on genuinely DEAD space in the host column
// collapses the pane — the pointer analogue of ESC-to-close. A click on ANY
// interactive/meaningful surface (a card, a control, a menu, the resize
// divider) belongs to that surface's own handler and must NOT collapse, so we
// bail whenever the click lands on — or inside — one; those handlers run first
// (bubbling), so re-targeting to another card or firing a button never races
// the close. Excluded on mobile: the host column is `inert` behind the
// full-screen overlay there so this can't fire anyway, but the viewport guard
// makes the intent explicit.
//
// This is a DENYLIST (bail on interactive) rather than a background
// allowlist: interactive elements are semantic and few (anchors, buttons,
// form controls, ARIA-roled widgets), so enumerating them is smaller and
// more stable than enumerating every background wrapper across list / board
// / table. A `[data-pane-keep]` opt-out lets any future non-semantic click
// surface exclude itself without editing this list.
export const PANE_DISMISS_INTERACTIVE =
	'a, button, input, textarea, select, label, summary, ' +
	'[role="button"], [role="link"], [role="row"], [role="tab"], ' +
	'[role="menuitem"], [role="menu"], [role="menuitemradio"], ' +
	'[role="menuitemcheckbox"], [role="listbox"], [role="option"], ' +
	'[role="checkbox"], [role="switch"], [role="separator"], ' +
	'[contenteditable="true"], [draggable="true"], ' +
	'.item-card, .table-row, .pane-divider, [data-pane-keep]';

export interface PaneDismissContext {
	/** The pane is open (`?item=` is set). */
	open: boolean;
	/** Phone layout: the pane is a full-screen overlay, never dismissed this way. */
	mobile: boolean;
	close: () => void;
}

/** Wire to the host column's `onclick`; `e.currentTarget` is that column. */
export function dismissPaneOnBackgroundClick(e: MouseEvent, ctx: PaneDismissContext): void {
	// Desktop split pane only.
	if (!ctx.open || ctx.mobile) return;
	// Primary, unmodified click only. Modifier/aux clicks are for
	// selection, context menus, open-in-new-tab, etc. — not "click away".
	if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
	const target = e.target as Element | null;
	if (!target) return;
	// A click on any interactive/meaningful surface is that surface's own.
	if (target.closest(PANE_DISMISS_INTERACTIVE)) return;
	// A drag that SELECTED text also ends in a `click`; that's a text
	// selection, not a dismiss.
	const sel = typeof window !== 'undefined' ? window.getSelection() : null;
	if (sel && !sel.isCollapsed && sel.toString().trim() !== '') return;
	// A click on the scroll gutter lands on the column itself but outside
	// its content box — not a background click.
	const col = e.currentTarget as HTMLElement | null;
	if (col && target === col && e.offsetX > col.clientWidth) return;
	ctx.close();
}
