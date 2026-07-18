// PaneTarget → canonical `?item=` resolution + same-item guard (PLAN-2154
// Architecture B / TASK-2158). This is the seam future content-link
// surfaces (relationships, breadcrumb, editor wiki-links, the graph drawer —
// TASK-2159/2160) thread `onOpenTarget` calls through: they hand up a
// `PaneTarget` (ref/slug/href/collectionSlug — never a full `Item`).
// `isSamePaneTarget` is called INSIDE `ItemDetail` (via its `fireOpenTarget`
// wrapper), where the loaded `item` is on hand, to drop a self-referential
// target before it ever reaches the host. `resolvePaneTarget` is called at
// the collection host to turn whatever survives into the canonical string
// `navigatePaneTo` (the pane's DRILL entry point in `paneController.ts`)
// expects.
//
// Kept framework-agnostic (no `$state`/`page`) and free of any `Item`
// FETCH — resolution only reads fields already present on the caller's
// hands, so it's exhaustively unit-testable without mounting a route.

import type { Item, PaneTarget } from '$lib/types';
import { formatItemRef } from '$lib/types';

/**
 * Extract the trailing path segment of an internal item href, e.g.
 * "/alice/myws/tasks/TASK-5" -> "TASK-5", "/alice/myws/tasks/TASK-5/" ->
 * "TASK-5". Query string / hash are stripped first. Null for an href with
 * no non-empty segment (e.g. "", "/").
 */
function lastHrefSegment(href: string): string | null {
	const path = href.split(/[?#]/)[0];
	const segments = path.split('/').filter(Boolean);
	return segments.length > 0 ? segments[segments.length - 1] : null;
}

/**
 * The raw ref-or-slug candidate a `PaneTarget` carries, before any
 * same-item normalization — `ref` preferred over `slug` over an `href`'s
 * trailing segment (mirrors `itemUrlId`/`formatItemRef`'s ref-over-slug
 * preference). Null when the target carries nothing resolvable.
 */
function rawPaneTargetCandidate(target: PaneTarget): string | null {
	if (target.ref) return target.ref;
	if (target.slug) return target.slug;
	if (target.href) return lastHrefSegment(target.href);
	return null;
}

/**
 * True when `target` refers to the SAME item as `current` — the item
 * presently loaded (e.g. `ItemDetail`'s own `item` state for the pane
 * showing it). Compares `current`'s stable `id` and BOTH its ref and slug
 * forms against the target's candidate, so a target that names the current
 * item by slug while it's open under its ref (or vice versa, or via an
 * href built from either) is still caught — a bare
 * `itemUrlId(current) === candidate` string compare misses that alias
 * (PLAN-2154 / TASK-2158).
 *
 * `current` is optional/nullable so callers that don't have a loaded item
 * in hand (e.g. the collection host, which only sees the target) can call
 * this — and `resolvePaneTarget` — without one; the guard simply never
 * fires.
 */
export function isSamePaneTarget(target: PaneTarget, current: Item | null | undefined): boolean {
	if (!current) return false;
	const candidate = rawPaneTargetCandidate(target);
	if (!candidate) return false;
	if (candidate === current.id) return true;
	const currentRef = formatItemRef(current);
	if (currentRef && candidate === currentRef) return true;
	return candidate === current.slug;
}

/**
 * Resolve a `PaneTarget` — whatever a content-link surface carries — to the
 * canonical `?item=` value the pane controller expects (`navigatePaneTo` in
 * the collection host, backed by `planPaneDrill`). Preference order is
 * ref > slug > an href's trailing path segment, mirroring
 * `itemUrlId`/`formatItemRef`.
 *
 * When `current` (the item already loaded where this target was clicked) is
 * supplied and the target resolves to THAT item — by id, or by either its
 * ref or slug even when the target names it differently than however it's
 * currently open (`isSamePaneTarget`) — this returns `null` rather than any
 * ref/slug string. That's the same-item guard: null is `navigatePaneTo`'s
 * caller's uniform "nothing to do" signal (mirrors "target carries nothing
 * resolvable" below), so a self-referential alias — e.g. the pane is open
 * via `?item=my-slug` and a link names the same item by its `TASK-5` ref —
 * is a clean no-op. Deliberately NOT the item's own canonical ref: that
 * could itself mismatch the pane's actual (possibly slug-shaped) open `?item=`
 * value and cause `planPaneDrill`'s same-ref check to wrongly treat it as a
 * new target (Codex review).
 *
 * Returns null when the target carries nothing resolvable.
 */
export function resolvePaneTarget(target: PaneTarget, current?: Item | null): string | null {
	if (isSamePaneTarget(target, current)) return null;
	return rawPaneTargetCandidate(target);
}
