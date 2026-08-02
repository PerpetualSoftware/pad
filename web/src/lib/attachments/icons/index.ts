/**
 * Attachment file-type icon set (PLAN-2392 DR-3b).
 *
 * One monochrome, `currentColor`-driven icon per format family. Monochrome
 * and stroke-only is deliberate: the icons inherit the surrounding text
 * color, so they theme with light/dark for free and stay visible under
 * `forced-colors`, where a hard-coded palette would not.
 *
 * TWO RENDER PATHS, ONE DEFINITION. Svelte call sites mount
 * `AttachmentIcon.svelte`; the editor's chip NodeView builds its DOM
 * imperatively and cannot mount a component, so it calls `iconSvg()` for
 * the same markup. Both read `ATTACHMENT_ICON_PATHS` and `ICON_SVG_ATTRS`
 * below — add an icon here and both surfaces get it.
 *
 * Every icon is a single `<path d>` with multiple subpaths so the two
 * render paths stay trivially equivalent (the component can render the
 * path declaratively rather than reaching for `{@html}`).
 *
 * Icons are decorative: they always carry `aria-hidden="true"`, and every
 * call site labels the attachment with its filename in real text.
 */

export const ATTACHMENT_ICON_IDS = [
	'image',
	'video',
	'audio',
	'document',
	'spreadsheet',
	'presentation',
	'pdf',
	'archive',
	'text',
	'generic',
] as const;

export type AttachmentIconId = (typeof ATTACHMENT_ICON_IDS)[number];

/** The fallback for anything unrecognized — a plain file, never a "❓" (DR-3a). */
export const GENERIC_ICON_ID: AttachmentIconId = 'generic';

// The blank-page outline the document-ish icons are built on: a page with
// a folded top-right corner.
const PAGE = 'M13.5 2.5H7A2 2 0 0 0 5 4.5v15a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z M13.5 2.5V8H19';

export const ATTACHMENT_ICON_PATHS: Record<AttachmentIconId, string> = {
	// Framed picture: sun + horizon line.
	image:
		'M3.5 5.5A2 2 0 0 1 5.5 3.5h13a2 2 0 0 1 2 2v13a2 2 0 0 1-2 2h-13a2 2 0 0 1-2-2z ' +
		'M10 9.5a1.5 1.5 0 1 1-3 0 1.5 1.5 0 0 1 3 0z ' +
		'M3.5 17l4.5-4.5 3 3 3.5-3.5 6 6',
	// Screen with a play triangle.
	video:
		'M3.5 6.5a2 2 0 0 1 2-2h13a2 2 0 0 1 2 2v11a2 2 0 0 1-2 2h-13a2 2 0 0 1-2-2z ' +
		'M10 8.5l6 3.5-6 3.5z',
	// Two-stem musical note.
	audio: 'M9 18V6l10-2v12 M9 18a2.5 2.5 0 1 1-5 0 2.5 2.5 0 0 1 5 0z M19 16a2.5 2.5 0 1 1-5 0 2.5 2.5 0 0 1 5 0z',
	// Page with lines of prose.
	document: `${PAGE} M8 12.5h8 M8 16h8 M8 19h5`,
	// Page with a cell grid.
	spreadsheet: `${PAGE} M7.5 11.5h9v8h-9z M7.5 15.5h9 M12 11.5v8`,
	// Projector screen on a stand.
	presentation: 'M3 4h18 M4.5 4v10a2 2 0 0 0 2 2h11a2 2 0 0 0 2-2V4 M12 16v3 M9 20.5h6',
	// Page with a bookmark ribbon.
	pdf: `${PAGE} M9.5 11.5h5v7l-2.5-2.2-2.5 2.2z`,
	// Crate with a lid and a latch.
	archive: 'M3.5 8h17v10.5a2 2 0 0 1-2 2h-13a2 2 0 0 1-2-2z M2.5 3.5h19V8h-19z M10 11.5h4',
	// Angle brackets with a slash — text and code share one family.
	text: 'M8.5 8.5L4.5 12l4 3.5 M15.5 8.5l4 3.5-4 3.5 M13.5 5l-3 14',
	// Bare page — the fallback.
	generic: PAGE,
};

/**
 * SVG element attributes shared by both render paths. `stroke="currentColor"`
 * + `fill="none"` is what makes the set theme-aware and forced-colors-safe;
 * `focusable="false"` keeps IE/legacy-Edge-style tab stops out of the chip.
 */
export const ICON_SVG_ATTRS = {
	viewBox: '0 0 24 24',
	fill: 'none',
	stroke: 'currentColor',
	'stroke-width': '1.5',
	'stroke-linecap': 'round',
	'stroke-linejoin': 'round',
	'aria-hidden': 'true',
	focusable: 'false',
} as const;

/** Narrow an arbitrary string to a known icon id, falling back to generic. */
export function isAttachmentIconId(id: string): id is AttachmentIconId {
	return (ATTACHMENT_ICON_IDS as readonly string[]).includes(id);
}

/**
 * Default icon box. `1em` rather than a pixel count so an icon scales with
 * whatever type size its surface uses — the same behavior the emoji this set
 * replaces had for free.
 */
const DEFAULT_ICON_SIZE = '1em';

const SIZE_PATTERN = /^[0-9]+(\.[0-9]+)?(px|em|rem|%)$/;

/** Coerce a caller-supplied size to a safe CSS length. Numbers mean pixels. */
export function iconSize(size: number | string | undefined): string {
	if (typeof size === 'number') {
		return Number.isFinite(size) && size > 0 ? `${size}px` : DEFAULT_ICON_SIZE;
	}
	if (typeof size === 'string' && SIZE_PATTERN.test(size)) return size;
	return DEFAULT_ICON_SIZE;
}

/**
 * Render an icon as SVG markup for imperative consumers (the editor's chip
 * NodeView). Svelte call sites should use `AttachmentIcon.svelte` instead.
 *
 * Everything interpolated here is either a repo constant or run through
 * `iconSize`, so the result is safe to assign to `innerHTML`.
 */
export function iconSvg(id: string, options: { size?: number | string } = {}): string {
	const iconId = isAttachmentIconId(id) ? id : GENERIC_ICON_ID;
	const size = iconSize(options.size);
	const attrs = Object.entries(ICON_SVG_ATTRS)
		.map(([key, value]) => `${key}="${value}"`)
		.join(' ');
	return (
		`<svg xmlns="http://www.w3.org/2000/svg" width="${size}" height="${size}" ` +
		`style="display:block" ${attrs}>` +
		`<path d="${ATTACHMENT_ICON_PATHS[iconId]}"/>` +
		`</svg>`
	);
}
