// Pure predicate factored out of `ItemCard.svelte`'s `handleCardClick`
// (PLAN-2105 / TASK-2111 row-click interception) so it's unit-testable
// without mounting the component (TASK-2116).
//
// The interception is opt-in (`onItemOpen` prop, wired only by the
// collection page) and must NEVER swallow:
//  - a click with no `onItemOpen` handler wired — every other surface
//    (starred / tags / roles / share) keeps plain full-page anchor nav.
//  - modifier-clicks (cmd/ctrl/shift/alt) — so cmd/ctrl-click still opens
//    the full page in a new tab (the "popout" state, PLAN-2105).
//  - non-left-button clicks (middle-click new-tab, right-click context
//    menu / copy-link).
//  - a click some other handler (a sub-control — star/PR badge/status
//    cycle/tag chip/reorder menu) already called `preventDefault()` on.
// Anything else is a plain left-click, which opens the item in the split
// pane instead of navigating.

/**
 * The subset of `MouseEvent` the predicate reads. Narrowed to a plain
 * interface (rather than requiring `MouseEvent` itself) so specs can pass
 * plain object literals instead of constructing DOM events.
 */
export interface CardClickLike {
	button: number;
	metaKey: boolean;
	ctrlKey: boolean;
	shiftKey: boolean;
	altKey: boolean;
	defaultPrevented: boolean;
}

/**
 * True when a card click should open the item in the split pane instead of
 * following its `href`. Mirrors `ItemCard.handleCardClick`'s bail-out order
 * exactly — see that function for the call site.
 */
export function shouldOpenInPane(e: CardClickLike, hasOnItemOpen: boolean): boolean {
	if (!hasOnItemOpen) return false;
	if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return false;
	if (e.defaultPrevented) return false;
	return true;
}
