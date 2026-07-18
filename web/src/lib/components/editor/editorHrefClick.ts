// Pure decision logic factored out of `EditorLinkPopover.svelte`'s
// `handleHrefClick` (PLAN-2154 Architecture B.3 / TASK-2160) so the pane-
// navigate branching is unit-testable without mounting a live Tiptap editor
// — `handleHrefClick` only fires from real editor `selectionUpdate`/click
// events, which need a full `Editor` instance to simulate.
//
// `EditorLinkPopover.handleHrefClick` is the SINGLE `goto` chokepoint for
// editor content-body/wiki-links: every anchor rendered inside the Tiptap
// document is inert `data-href` with clicks globally `preventDefault`ed, so
// this is the only place a click on one of those links actually navigates.
// `planHrefClick` decides WHAT that navigation should be; the component
// stays responsible for the DOM side effects (preventDefault, hiding the
// popover, actually calling `goto`/`onOpenTarget`/`window.location.assign`).

import { isSameWorkspaceItemHref } from '$lib/collections/paneTarget';

/**
 * The subset of `MouseEvent` the decision reads. Narrowed to a plain
 * interface (rather than requiring `MouseEvent` itself) so specs can pass
 * plain object literals instead of constructing DOM events — mirrors
 * `itemCardClick.ts`'s `CardClickLike`.
 */
export interface HrefClickLike {
	button: number;
	ctrlKey: boolean;
	metaKey: boolean;
	shiftKey: boolean;
}

/**
 * The navigation context the pane-drill decision needs. `hasOnOpenTarget`
 * mirrors whether `ItemDetail`'s `onOpenTarget` seam is wired at all (an
 * unembedded full-page view with no pane host passes none); `wsSlug` +
 * `collectionPrefixes` (a map of collection slug → ref prefix) let the
 * classifier confirm an internal link actually points at a well-formed item
 * in the CURRENT workspace before drilling the pane (see
 * `isSameWorkspaceItemHref`).
 */
export interface HrefClickContext {
	hasOnOpenTarget: boolean;
	wsSlug: string;
	collectionPrefixes: ReadonlyMap<string, string>;
}

export type HrefClickPlan =
	// Modifier/middle-click, or no href at all: let the browser's native
	// anchor behavior handle it (new tab, download, etc). The component
	// must NOT preventDefault/stopPropagation in this case.
	| { kind: 'passthrough' }
	// An internal, same-workspace item link with a pane-navigate handler
	// wired: drill the pane in place instead of navigating away.
	| { kind: 'pane'; href: string }
	// An internal link with no pane handler, OR one that isn't a
	// current-workspace item link (a cross-workspace resolver link, a
	// different workspace's item as a plain path, a non-item app route, or
	// an arbitrary internal path a user typed via "Edit link" — all
	// indistinguishable from a wiki-link at the Tiptap mark level, so they're
	// let through as a normal nav rather than misrouted into `?item=`):
	// SPA-navigate via `goto`.
	| { kind: 'goto'; href: string }
	// A non-internal (external / absolute) URL: full-page navigation.
	| { kind: 'external'; href: string };

/**
 * Decide how a left-click on the link popover's href anchor should be
 * handled.
 *
 * The `pane` branch requires `isSameWorkspaceItemHref` — unlike
 * relationships/children/graph, this popover's href comes off a Tiptap
 * `link` mark with no signal distinguishing a wiki-link-inserted item link
 * from a human-typed arbitrary internal path (Codex review, TASK-2160; see
 * that function's doc comment in `$lib/collections/paneTarget`).
 */
export function planHrefClick(
	e: HrefClickLike,
	href: string,
	ctx: HrefClickContext,
): HrefClickPlan {
	if (!href) return { kind: 'passthrough' };
	// Let the browser handle new-tab modifiers and middle-click itself.
	if (e.button !== 0 || e.ctrlKey || e.metaKey || e.shiftKey) return { kind: 'passthrough' };

	const isInternal = href.startsWith('/') && !href.startsWith('//');
	if (
		isInternal &&
		ctx.hasOnOpenTarget &&
		isSameWorkspaceItemHref(href, ctx.wsSlug, ctx.collectionPrefixes)
	) {
		return { kind: 'pane', href };
	}
	if (isInternal) return { kind: 'goto', href };
	return { kind: 'external', href };
}
