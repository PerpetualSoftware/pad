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

/**
 * The minimal item shape the same-item guard (`isSamePaneTarget` /
 * `resolvePaneTarget`) actually reads: `id` + `slug` for exact matches and
 * `item_number` for the case-insensitive ref-number equivalence. Narrowing the
 * `current` param to this (rather than a full `Item`) lets callers that only
 * hold a resolved identity — the full-page pane host, which has the master's
 * `{ id, ref, slug }` from `ItemDetail.onIdentity` and derives `item_number`
 * from `ref` (PLAN-2154 Architecture E / TASK-2174) — reuse the guard without
 * fabricating a whole `Item`. A full `Item` is still assignable, so every
 * existing caller is unaffected.
 */
export type PaneGuardItem = Pick<Item, 'id' | 'slug' | 'item_number'>;

// The cross-workspace wiki-link resolver route, `/-/r/{workspace}/{ref}`
// (`wikiLinksToMarkdown`/`renderMarkdown` in `$lib/utils/markdown`). Its
// trailing segment IS a ref, but for a possibly-DIFFERENT workspace — taking
// it at face value would drill the CURRENT workspace's pane to a same-
// numbered local item (wrong item), or false-positive the same-item guard
// when the numbers happen to coincide. The `/-/r/` sentinel can't appear in
// a same-workspace item URL (those are always `/{username}/{workspace}/...`,
// and usernames are letter-led — see `markdownToWikiLinks`), so it's an
// unambiguous signal to treat the WHOLE target as not pane-resolvable.
const CROSS_WORKSPACE_HREF_PREFIX = '/-/r/';

// A throwaway base so `href` can be parsed with the `URL` API regardless of
// shape — root-relative ("/-/r/…", what every href BUILDER in this codebase
// emits) or absolute ("http://host/-/r/…", what `HTMLAnchorElement.href`
// ALWAYS returns when a future click-interceptor reads it off a live DOM
// anchor rather than its raw attribute). Comparing `.pathname` after parsing
// catches both uniformly; a raw `string.startsWith` on the href only catches
// the root-relative case (Codex review).
const HREF_PARSE_BASE = 'http://pad.invalid';
// The host of `HREF_PARSE_BASE` — a resolved href keeps this host ONLY when it
// was a genuinely same-origin root-relative path. A value like
// "/\evil.example/…" (browsers treat "\" as "/") or "//evil.example/…"
// resolves to a DIFFERENT host and is really an external navigation, so
// comparing the parsed host against this sentinel is the robust "is this
// truly local?" test — a raw `startsWith('/')` string check is fooled by the
// backslash trick (Codex review).
const HREF_PARSE_HOST = new URL(HREF_PARSE_BASE).host;

/** True when `href` is (or resolves to) the cross-workspace resolver route. */
function isCrossWorkspaceHref(href: string): boolean {
	try {
		return new URL(href, HREF_PARSE_BASE).pathname.startsWith(CROSS_WORKSPACE_HREF_PREFIX);
	} catch {
		return false;
	}
}

/**
 * Extract the trailing path segment of a SAME-WORKSPACE internal item href,
 * e.g. "/alice/myws/tasks/TASK-5" -> "TASK-5", "/alice/myws/tasks/TASK-5/"
 * -> "TASK-5". Query string / hash are stripped first. Null for an href
 * with no non-empty segment (e.g. "", "/"). Callers must check
 * `isCrossWorkspaceHref` first — this function only strips the path, it
 * doesn't re-validate workspace locality.
 */
function lastHrefSegment(href: string): string | null {
	const path = href.split(/[?#]/)[0];
	const segments = path.split('/').filter(Boolean);
	return segments.length > 0 ? segments[segments.length - 1] : null;
}

// Ref-shaped candidate: PREFIX-NUMBER. Mirrors `internal/store/items.go`'s
// `parseItemRef` — case-insensitive LETTERS-ONLY prefix (no digits; the
// server's own loop rejects any non-A-Z byte in the prefix), a hyphen, then
// a positive integer suffix. A looser digit-permitting prefix would
// misclassify a digit-bearing slug like "roadmap2-5" as ref number 5 and
// false-positive the same-item guard against an unrelated item TASK-5
// (Codex review — PR diff pass).
const REF_SHAPE = /^([A-Za-z]+)-(\d+)$/;

/** Parse a ref-shaped candidate's item NUMBER (case-insensitive; prefix is
 *  discarded — see `isSamePaneTarget`). Null for a non-ref-shaped string or
 *  a zero/non-finite number. */
function parseRefNumber(candidate: string): number | null {
	const m = REF_SHAPE.exec(candidate);
	if (!m) return null;
	const number = Number(m[2]);
	return Number.isFinite(number) && number > 0 ? number : null;
}

/**
 * True when `href` is a root-relative link to a REF-identified item IN THE
 * CURRENT WORKSPACE (`wsSlug`) that is WELL-FORMED — its collection segment
 * is a known workspace collection AND that collection's prefix matches the
 * ref's prefix (i.e. exactly the shape every item-URL builder emits). This
 * is the strict gate that decides whether the ONE untrusted content-link
 * surface — `EditorLinkPopover`, whose href comes off a Tiptap `link` MARK —
 * may drill the split pane.
 *
 * Every OTHER `PaneTarget.href` producer (relationships, children, the graph
 * drawer) builds its href FROM real same-workspace item data, so it's
 * item-shaped by construction and doesn't need this check — `resolvePaneTarget`
 * trusts it outright, slug-only items included. The editor popover is
 * different: a `link` mark is indistinguishable between a wiki-link-inserted
 * item link and a human-typed arbitrary path (Codex review, TASK-2160). A
 * trailing-REF-shape test ALONE isn't enough, because the pane's `?item=`
 * is scoped to the CURRENT workspace and resolved by ref NUMBER (the
 * collection segment is dropped): a link to a different workspace's item as
 * a plain path (`/bob/otherws/tasks/TASK-9` — NOT a `/-/r/` resolver link),
 * to a non-item route that merely ends in a ref-shaped segment
 * (`/alice/myws/tags/TASK-5`), or to a self-inconsistent path whose
 * collection doesn't match the ref (`/alice/myws/playbooks/TASK-9`) would
 * otherwise drill the current workspace's same-numbered item. So the
 * segments are checked POSITIONALLY against the two shapes every item-URL
 * builder in this codebase emits (`itemUrlId` via `renderMarkdown`/
 * `wikiLinksToMarkdown`/`Editor.execLink`):
 *
 *   [username, workspace, collection, REF]   (4 segments — with username)
 *   [workspace, collection, REF]             (3 segments — no username)
 *
 * requiring `workspace === wsSlug`, a `collectionPrefixes` entry for
 * `collection`, a REF whose prefix equals that collection's prefix
 * (case-insensitive — mirrors the server's case-insensitive `parseItemRef`),
 * and a POSITIVE ref number (rejecting `TASK-0`, which the server's
 * `parseItemRef` also rejects). A cross-workspace resolver link is rejected
 * up front. Anything else falls back to a normal `goto` — the link still
 * works, it just doesn't drill the pane (a graceful degradation, not a
 * break). Empty `wsSlug` or `collectionPrefixes` (context not yet loaded)
 * also declines, for the same safe fallback.
 */
export function isSameWorkspaceItemHref(
	href: string,
	wsSlug: string,
	collectionPrefixes: ReadonlyMap<string, string>,
): boolean {
	if (!wsSlug || collectionPrefixes.size === 0) return false;
	if (isCrossWorkspaceHref(href)) return false;
	// Parse against the sentinel base and require the result to be
	// same-origin: this rejects an href that only LOOKS root-relative but
	// resolves cross-origin (`/\evil.example/…`, `//evil.example/…`) — those
	// are external navigations, not local item links (Codex review).
	const rawPath = href.split(/[?#]/)[0];
	let url: URL;
	try {
		url = new URL(href, HREF_PARSE_BASE);
	} catch {
		return false;
	}
	if (url.host !== HREF_PARSE_HOST) return false;
	// Require the raw path to ALREADY be in canonical form. The URL parser
	// silently normalizes a path (backslash→slash, dot-segments, percent
	// casing), so an href like `/alice/myws/tasks\TASK-5` would validate as
	// `.../tasks/TASK-5` here — yet the RAW href is what the host later resolves
	// (`resolvePaneTarget` splits on "/" only), yielding a bogus `tasks\TASK-5`
	// segment and drilling an invalid item. Every builder emits clean ASCII
	// item URLs, so any href that isn't already canonical isn't one — navigate
	// it normally instead (Codex review, regression guard).
	if (url.pathname !== rawPath) return false;
	// `url.pathname` always leads with "/", so split()[0] is the empty string
	// before it; drop it. Do NOT `filter(Boolean)` the rest — collapsing
	// empties would accept malformed paths with a double slash / trailing slash
	// (`/alice/myws//tasks/TASK-9`) as valid item routes (Codex review). Any
	// remaining empty segment means the path isn't a clean item URL.
	const segments = url.pathname.split('/').slice(1);
	if (segments.some((s) => s === '')) return false;
	let ws: string;
	let coll: string;
	let ref: string;
	if (segments.length === 4) {
		[, ws, coll, ref] = segments;
	} else if (segments.length === 3) {
		[ws, coll, ref] = segments;
	} else {
		return false;
	}
	if (ws !== wsSlug) return false;
	const expectedPrefix = collectionPrefixes.get(coll);
	if (!expectedPrefix) return false;
	// The ref must be exactly `<collection-prefix>-<positive-int>` — a
	// self-consistent item-URL as every builder emits. Comparing against the
	// KNOWN prefix (rather than a generic REF_SHAPE) accepts digit-bearing
	// collection prefixes like `R2-1` (the server's ref grammar allows a
	// letter-led alphanumeric prefix) while still rejecting a digit-bearing
	// SLUG like `roadmap2-5` (its "prefix" won't equal the collection's), a
	// self-inconsistent path like `/docs/TASK-9` (prefix mismatch), and a
	// zero ref like `TASK-0` (server `parseItemRef` rejects zero). Prefixes
	// never contain a hyphen, so the LAST hyphen splits prefix from number.
	const dash = ref.lastIndexOf('-');
	if (dash <= 0 || dash === ref.length - 1) return false;
	const refPrefix = ref.slice(0, dash);
	const numStr = ref.slice(dash + 1);
	if (refPrefix.toLowerCase() !== expectedPrefix.toLowerCase()) return false;
	if (!/^\d+$/.test(numStr)) return false;
	return Number(numStr) > 0;
}

/**
 * The raw ref-or-slug candidate a `PaneTarget` carries, before any
 * same-item normalization — `ref` preferred over `slug` over an `href`'s
 * trailing segment (mirrors `itemUrlId`/`formatItemRef`'s ref-over-slug
 * preference). Null when the target carries nothing resolvable.
 *
 * A cross-workspace `href` (`isCrossWorkspaceHref`) makes the WHOLE target
 * untrustworthy, not just the href field: it's unambiguous evidence the
 * link doesn't point at a same-workspace item, so a `ref`/`slug` set
 * alongside it is more likely mis-derived than an intentional override —
 * trusting it anyway risks silently opening the wrong local item (Codex
 * review). By this type's own contract (`$lib/types`), a genuinely
 * same-workspace target never carries a cross-workspace href in the first
 * place.
 */
function rawPaneTargetCandidate(target: PaneTarget): string | null {
	if (target.href && isCrossWorkspaceHref(target.href)) return null;
	if (target.ref) return target.ref;
	if (target.slug) return target.slug;
	if (target.href) return lastHrefSegment(target.href);
	return null;
}

/** True when `candidate` is `current`'s own item number, expressed as a
 *  ref-shaped string (case-insensitive, prefix ignored — see `REF_SHAPE`'s
 *  doc comment). False when `current` has no item number. */
function matchesRefNumber(candidate: string, current: PaneGuardItem): boolean {
	if (!current.item_number) return false;
	const number = parseRefNumber(candidate);
	return number !== null && number === current.item_number;
}

/**
 * True when `target` refers to the SAME item as `current` — the item
 * presently loaded (e.g. `ItemDetail`'s own `item` state for the pane
 * showing it).
 *
 * Field-provenance-aware: a target names an item through exactly one
 * "channel" at a time, and only checked with that channel's own semantics,
 * so a `ref`-sourced candidate is judged ONLY as a ref (id, or its item
 * NUMBER — case-insensitive, prefix ignored, matching a stale/renamed
 * prefix from before a collection move, mirroring the server's
 * `GetItemByRef` number-only fallback) and never falls through to a raw
 * string compare against `current.slug` — which could coincidentally equal
 * a DIFFERENT item's ref string (e.g. current is slugged "plan-6" while a
 * target `{ ref: "plan-6" }` names some OTHER item numbered 6) and wrongly
 * suppress a valid navigation. A `slug`-sourced candidate is likewise
 * judged only as a slug (or id). An `href`-sourced candidate carries no
 * declared identity — its trailing segment could be EITHER shape — so it's
 * tried as a ref FIRST when it's ref-shaped (never falling through to the
 * slug compare in that case either), and as a slug only when it isn't;
 * this mirrors the server's own ref-before-slug resolution order for a bare
 * `?item=` value (PLAN-2154 / TASK-2158; Codex review — PR diff pass, x2).
 *
 * `current` is optional/nullable so callers that don't have a loaded item
 * in hand (e.g. the collection host, which only sees the target) can call
 * this — and `resolvePaneTarget` — without one; the guard simply never
 * fires.
 */
export function isSamePaneTarget(target: PaneTarget, current: PaneGuardItem | null | undefined): boolean {
	if (!current) return false;
	// A cross-workspace href is unambiguous evidence of non-locality — see
	// `rawPaneTargetCandidate`'s doc comment for why this precedes ref/slug.
	if (target.href && isCrossWorkspaceHref(target.href)) return false;
	if (target.ref) {
		return target.ref === current.id || matchesRefNumber(target.ref, current);
	}
	if (target.slug) {
		return target.slug === current.id || target.slug === current.slug;
	}
	if (target.href) {
		const candidate = lastHrefSegment(target.href);
		if (!candidate) return false;
		if (candidate === current.id) return true;
		// Ref-shaped candidates are evaluated ONLY as refs — mirroring the
		// server's ref-before-slug resolution order — never falling through
		// to a slug compare that could coincidentally match current.slug's
		// literal string while the ref actually names a DIFFERENT item (the
		// same collision fixed above for `target.ref`, now made consistent
		// for the href channel too; Codex review — PR diff pass).
		if (REF_SHAPE.test(candidate)) return matchesRefNumber(candidate, current);
		return candidate === current.slug;
	}
	return false;
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
export function resolvePaneTarget(target: PaneTarget, current?: PaneGuardItem | null): string | null {
	if (isSamePaneTarget(target, current)) return null;
	return rawPaneTargetCandidate(target);
}

/**
 * Build the `PaneTarget` for an item's structural PARENT — the single-hop
 * breadcrumb `ItemDetail` un-hides for the embedded pane (PLAN-2154
 * Architecture C / D3, TASK-2165). Reads only fields already present on the
 * item response (`parent_ref`/`parent_slug`/`parent_collection_slug` —
 * `$lib/types` Item), so it reconstructs correctly even on a cold-loaded
 * shared `?item=` pane URL with zero additional fetches — the same property
 * that makes `resolvePaneTarget` above framework-agnostic and exhaustively
 * unit-testable.
 *
 * `parent_slug` + `parent_collection_slug` are the presence gate (mirroring
 * the pre-existing full-page breadcrumb's own `{#if item.parent_collection_slug
 * && item.parent_slug}` condition): `parent_ref` is carried when available
 * but not required, since a legacy/no-number item can have a parent
 * addressable only by slug. Returns `undefined` when the item has no
 * structural parent to show.
 */
export function breadcrumbParentTarget(
	item: Pick<Item, 'parent_ref' | 'parent_slug' | 'parent_collection_slug'> | null | undefined,
): PaneTarget | undefined {
	if (!item?.parent_collection_slug || !item?.parent_slug) return undefined;
	return {
		ref: item.parent_ref ?? undefined,
		slug: item.parent_slug,
		collectionSlug: item.parent_collection_slug,
	};
}
