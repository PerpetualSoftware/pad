/**
 * The attachment SURFACE-RENDERER registry (PLAN-2392 phase 3c-i / TASK-2476,
 * the seam PLAN-2393 extends).
 *
 * A surface (today just the image viewer) draws an attachment through one of a
 * small, CLOSED set of renderers. This function is the single decision of WHICH
 * one — a string id, not a component handshake: the consumer maps the id to its
 * own markup arm internally, so a new renderer is a new id here plus an arm
 * there, nothing wired across a boundary.
 *
 * TODAY THERE ARE TWO. `'raster-image'` is exactly the DR-16 raster allowlist
 * (`canOpenInViewer`). `'text'` is the in-app text preview (`canPreviewAsText`,
 * IDEA-2712 / GitHub #1169) — markdown and plain text, rendered by us from bytes
 * we fetch, never handed to the browser to inline. Everything else — an
 * unsafe/active type like SVG, an office document, an archive, or an UNRESOLVED
 * (null) MIME — returns `null`, which the consumer renders as its no-bytes ICON
 * FALLBACK. `'pdf'` remains reserved for PLAN-2393; the `null` arm stays the
 * fallback for what no renderer claims.
 *
 * ORDER IS NOT ARBITRARY: the two predicates are disjoint by construction (one
 * is a raster-image allowlist, the other a text allowlist), so the sequence below
 * cannot change an answer. It is written image-first only to match the union's
 * declaration order.
 *
 * WHY IT WRAPS `canOpenInViewer` RATHER THAN RESTATING THE ALLOWLIST. DR-16 puts
 * "what may this MIME become on screen" in ONE module (the display helpers) on
 * purpose — a second copy of the raster list is a second thing to forget when
 * the allowlist changes. This is the RENDERER-selection view of that one answer,
 * not a competing source of truth: `'raster-image'` is defined AS the set
 * `canOpenInViewer` admits.
 *
 * IT FAILS CLOSED, like the predicate it wraps: a null / unknown / unresolved
 * MIME is not a renderer, so the caller shows the fallback, never a guessed arm.
 */
import { canOpenInViewer, canPreviewAsText } from '$lib/attachments/display';

/**
 * The renderers a surface can draw an attachment through. A string-id union so
 * a renderer can be added without a component contract; the consumer switches on
 * the id. `'text'` arrived that way (IDEA-2712); `'pdf'` is still PLAN-2393's.
 */
export type SurfaceRendererId = 'raster-image' | 'text';

/**
 * The renderer for a MIME, or `null` when none claims it (→ the icon fallback).
 * `'raster-image'` is exactly the DR-16 raster allowlist and `'text'` exactly the
 * `canPreviewAsText` allowlist; unsafe, unknown and unresolved (null) MIMEs all
 * return `null`. In particular the force-download bucket (`text/html`,
 * `text/javascript`, `application/javascript` — all `CategoryText` server-side)
 * claims no renderer, per PLAN-2393 DR-6.
 */
export function getSurfaceRenderer(mime: string | null): SurfaceRendererId | null {
	if (canOpenInViewer(mime)) return 'raster-image';
	if (canPreviewAsText(mime)) return 'text';
	return null;
}
