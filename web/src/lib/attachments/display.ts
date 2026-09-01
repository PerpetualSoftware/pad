/**
 * Display helpers shared by every attachment surface (TASK-2383, TASK-2417).
 *
 * These were private to `$lib/components/settings/StorageTab.svelte` until the
 * item attachment strip needed the same mime table; extracted here so there is
 * exactly one icon mapping and one byte formatter to maintain.
 *
 * TASK-2417 (PLAN-2392 DR-3) folded the editor chip's two private icon helpers
 * into `iconForAttachment` below, so the live attachment surfaces — strip,
 * StorageTab, editor chip — now share one mapper. (The static markdown
 * renderer's byte formatter and CopyItemDialog's are deliberately out of
 * scope; this is "three live helpers become one", not "one formatter exists in
 * the repo".)
 */

import familyFixture from './mime-families.json';
import { GENERIC_ICON_ID, isAttachmentIconId, type AttachmentIconId } from './icons/index';

// Same algorithm as web/src/routes/console/billing/+page.svelte. Picks a
// unit so the displayed value is < 1024; bump thresholds nudged down half
// the previous unit so 1,048,575 bytes reads as "1.0 MB" rather than the
// misleading "1024 KB" you'd get from a straight Math.round at the KB tier.
//
// Note this renders `0 B` for a zero byte count and does not special-case
// non-finite input — deliberately (DR-3b). A surface that would rather show
// nothing than "0 B" (the editor chip) keeps that conditional at its own call
// site; the helper does not grow a mode.
/**
 * A filename safe to put in a sentence.
 *
 * `filename` is nominally always present, but a row can carry an empty one —
 * an upload with no name, a legacy row — and every surface then renders the
 * gap differently: a blank tile, an accessible name that says nothing, and a
 * confirmation reading "Delete ?", which is the one place it actually matters
 * because the user is being asked to approve something unnamed (final review
 * round 4).
 *
 * Deliberately generic rather than clever: the id is not a name, and inventing
 * one from the MIME would claim knowledge the row does not have.
 */
export function displayFilename(filename: string | null | undefined): string {
	const trimmed = (filename ?? '').trim();
	return trimmed || 'Untitled file';
}

export function formatBytes(bytes: number): string {
	if (bytes < 0) return `${bytes} B`;
	const KB = 1024;
	const MB = KB * 1024;
	const GB = MB * 1024;
	const bumpGB = GB - MB / 2;
	const bumpMB = MB - KB / 2;
	if (bytes >= bumpGB) return formatUnit(bytes / GB, 'GB');
	if (bytes >= bumpMB) return formatUnit(bytes / MB, 'MB');
	if (bytes >= KB) return formatUnit(bytes / KB, 'KB');
	return `${bytes} B`;
}

function formatUnit(value: number, unit: string): string {
	if (value >= 10) return `${Math.round(value)} ${unit}`;
	return `${value.toFixed(1)} ${unit}`;
}

/**
 * MIME → icon family, from the shared fixture. A Go test asserts the server's
 * upload allowlist is fully covered by that file (PLAN-2392 DR-3a), so the two
 * lists cannot drift apart silently.
 */
const MIME_FAMILIES: Record<string, string> = familyFixture.mime;

/**
 * Extension → icon family for the fallback path. The editor chip renders
 * before its HEAD probe returns a MIME, and a stored MIME can be a generic
 * `application/octet-stream`, so the filename is a real second source rather
 * than a theoretical one. Deliberately broader than the allowlist: an
 * extension costs nothing to recognize even for a format we would refuse as an
 * upload.
 */
const EXTENSION_FAMILIES: Record<string, AttachmentIconId> = {
	png: 'image',
	jpg: 'image',
	jpeg: 'image',
	gif: 'image',
	webp: 'image',
	avif: 'image',
	heic: 'image',
	heif: 'image',
	bmp: 'image',
	tif: 'image',
	tiff: 'image',
	svg: 'image',
	mp4: 'video',
	webm: 'video',
	mov: 'video',
	mkv: 'video',
	avi: 'video',
	mp3: 'audio',
	wav: 'audio',
	ogg: 'audio',
	flac: 'audio',
	aac: 'audio',
	m4a: 'audio',
	pdf: 'pdf',
	doc: 'document',
	docx: 'document',
	odt: 'document',
	rtf: 'document',
	xls: 'spreadsheet',
	xlsx: 'spreadsheet',
	ods: 'spreadsheet',
	csv: 'spreadsheet',
	tsv: 'spreadsheet',
	ppt: 'presentation',
	pptx: 'presentation',
	odp: 'presentation',
	zip: 'archive',
	tar: 'archive',
	gz: 'archive',
	tgz: 'archive',
	bz2: 'archive',
	'7z': 'archive',
	rar: 'archive',
	txt: 'text',
	md: 'text',
	json: 'text',
	xml: 'text',
	yaml: 'text',
	yml: 'text',
	toml: 'text',
	html: 'text',
	js: 'text',
	ts: 'text',
	css: 'text',
	sh: 'text',
	go: 'text',
	py: 'text',
};

/**
 * Pattern → family, checked after an exact fixture hit and the media-type
 * prefixes. Order matters: the specific `…ml` / `opendocument.*` markers come
 * first, so `application/vnd.ms-excel.sheet.macroEnabled.12` lands on
 * spreadsheet rather than on a broader match.
 *
 * The archive patterns are token-delimited on purpose. A bare `includes('zip')`
 * claims `application/vnd.airzip.filesecure.azf`, which is an encrypted
 * FileSECURE document and not an archive at all (Codex review).
 */
const MIME_PATTERNS: ReadonlyArray<readonly [RegExp, AttachmentIconId]> = [
	[/wordprocessingml/, 'document'],
	[/spreadsheetml/, 'spreadsheet'],
	[/presentationml/, 'presentation'],
	[/opendocument\.text/, 'document'],
	[/opendocument\.spreadsheet/, 'spreadsheet'],
	[/opendocument\.presentation/, 'presentation'],
	[/(^|[.\-+/])ms-?excel([.\-+]|$)/, 'spreadsheet'],
	[/(^|[.\-+/])ms-?powerpoint([.\-+]|$)/, 'presentation'],
	[/(^|[.\-+/])ms-?word([.\-+]|$)/, 'document'],
	[/(^|[.\-+/])compressed([.\-+]|$)/, 'archive'],
	[/(^|[.\-+/])g?zip([.\-+]|$)/, 'archive'],
	[/(^|[.\-+/])rar([.\-+]|$)/, 'archive'],
	[/(^|[.\-+/])x-tar([.\-+]|$)/, 'archive'],
	// Generic office tier, last so the specific rules above win. These are the
	// three prefixes the deleted `categoryIcon` treated as "a document": every
	// vendor office format it didn't recognize more precisely (vnd.ms-project,
	// opendocument.graphics, …) landed on the document glyph rather than the
	// unknown one, and that is still the better answer than generic.
	[/^application\/vnd\.openxmlformats/, 'document'],
	[/^application\/vnd\.oasis/, 'document'],
	[/^application\/vnd\.ms-/, 'document'],
];

/** Strip parameters and case, matching the server's `NormalizeMIME`. */
function normalizeMime(mime: string | null | undefined): string {
	if (!mime) return '';
	const semi = mime.indexOf(';');
	return (semi >= 0 ? mime.slice(0, semi) : mime).trim().toLowerCase();
}

function extensionOf(filename: string | null | undefined): string {
	if (!filename) return '';
	return filename.toLowerCase().match(/\.([a-z0-9]+)$/)?.[1] ?? '';
}

/**
 * Pick the file-type icon for an attachment: MIME first, filename extension
 * second, generic file last. Returns an icon *identifier* — resolve it with
 * `AttachmentIcon.svelte` or `iconSvg()` from `./icons`.
 *
 * Renamed from `categoryIcon` in TASK-2417 (PLAN-2392 DR-3): it no longer
 * returns an emoji, it takes the filename as a second source, and it
 * deliberately does NOT correspond to the server's `Category` enum — that
 * bucketing drives storage quotas and is too coarse to tell a spreadsheet from
 * a slide deck.
 *
 * There is no "unknown" result: an unrecognized format gets the generic file
 * icon, never a question mark (DR-3a). The upload allowlist accepts formats
 * the extension table can't be relied on to catch — a valid upload may have no
 * extension at all — so a calm default is the only honest fallback.
 */
export function iconForAttachment(
	mime: string | null | undefined,
	filename?: string | null,
): AttachmentIconId {
	const m = normalizeMime(mime);

	const mapped = MIME_FAMILIES[m];
	if (mapped && isAttachmentIconId(mapped)) return mapped;

	// Type-prefix fallback for media the allowlist doesn't enumerate — a
	// future `image/jxl` should still read as a picture.
	if (m.startsWith('image/')) return 'image';
	if (m.startsWith('video/')) return 'video';
	if (m.startsWith('audio/')) return 'audio';

	// Pattern fallback for office/archive families the allowlist doesn't
	// enumerate. The helpers this replaced matched these by pattern, and rows
	// predating the current allowlist can still carry them (`x-rar-compressed`
	// was in the old `categoryIcon`), so dropping to the generic icon here
	// would be a visible regression on existing data rather than a
	// simplification.
	for (const [pattern, family] of MIME_PATTERNS) {
		if (pattern.test(m)) return family;
	}

	const byExtension = EXTENSION_FAMILIES[extensionOf(filename)];
	if (byExtension) return byExtension;

	// Last, and after the extension check: `text/*` is the widest of the
	// prefixes, so an unlisted text MIME (`text/x-comma-separated-values`)
	// should not out-vote a filename that says exactly what the file is. An
	// EXACT MIME hit above still wins over the extension — a mislabelled
	// filename shouldn't override a known type.
	if (m.startsWith('text/')) return 'text';

	return GENERIC_ICON_ID;
}

export function isImage(mime: string): boolean {
	return mime.startsWith('image/');
}

/** Human label per icon family, for the "what IS this file" line. */
const FAMILY_LABELS: Record<AttachmentIconId, string> = {
	image: 'Image',
	video: 'Video',
	audio: 'Audio',
	document: 'Document',
	spreadsheet: 'Spreadsheet',
	presentation: 'Presentation',
	pdf: 'PDF',
	archive: 'Archive',
	text: 'Text',
	generic: 'File',
};

/**
 * A short, human file-type description — "PDF", "PNG image", "XLSX
 * spreadsheet", "File" (PLAN-2392 DR-2 / DR-18).
 *
 * Built on `iconForAttachment` on purpose: the icon and the words beside it
 * must never disagree about what a file is, and reading the family from the
 * same mapper is the only way to guarantee that. The raw MIME is deliberately
 * NOT what surfaces show — `application/vnd.openxmlformats-officedocument.
 * spreadsheetml.sheet` is not a type a human reads — but it stays available
 * to call sites for a `title`.
 *
 * The extension is dropped when it merely repeats the family ("PDF · PDF")
 * and kept when it adds the specific format ("PNG image"). With neither a
 * usable MIME nor an extension the answer is the family fallback, "File" —
 * never an empty string, so the line never renders as a stray separator.
 */
export function describeAttachmentType(
	mime: string | null | undefined,
	filename?: string | null,
): string {
	const family = FAMILY_LABELS[iconForAttachment(mime, filename)];
	const ext = extensionOf(filename).toUpperCase();
	if (!ext || ext === family.toUpperCase()) return family;
	return `${ext} ${family.toLowerCase()}`;
}

/**
 * The exact raster types the in-app image viewer may open (PLAN-2392
 * DR-16). Deliberately an allowlist and NOT an `image/` prefix test:
 * `image/svg+xml` carries active content, and a legacy row, a
 * mislabelled upload or an extensionless SVG sniffed as XML can all
 * arrive wearing an `image/*` label. Formats a browser may not decode
 * at all (`image/tiff`, `image/heic`) are excluded for the separate
 * reason that a viewer that silently shows nothing is worse than the
 * file panel.
 *
 * `isImage` survives unchanged as the general "is this a picture"
 * predicate (icon choice, grouping); this is the narrower question of
 * what may be handed to the viewer.
 */
const VIEWER_MIMES: ReadonlySet<string> = new Set([
	'image/png',
	'image/jpeg',
	'image/gif',
	'image/webp',
	'image/avif'
]);

/**
 * Additional types a browser renders honestly in a new tab (PLAN-2392
 * DR-5). PDF and plain text only — every other `text/*` subtype
 * (markdown, CSV, XML) is downloaded or rendered inconsistently across
 * browsers, and office documents, archives and the types the server
 * force-downloads (HTML, JS) never preview. Those surfaces offer
 * Download alone.
 */
const BROWSER_PREVIEW_MIMES: ReadonlySet<string> = new Set([
	'application/pdf',
	'text/plain'
]);

/** May this MIME be opened in the in-app image viewer? (DR-16) */
export function canOpenInViewer(mime: string | null | undefined): boolean {
	return VIEWER_MIMES.has(normalizeMime(mime));
}

/**
 * May this MIME be handed to the browser to display — the viewer's
 * raster set plus PDF and plain text? (DR-5)
 */
export function canBrowserPreview(mime: string | null | undefined): boolean {
	const m = normalizeMime(mime);
	return VIEWER_MIMES.has(m) || BROWSER_PREVIEW_MIMES.has(m);
}

/**
 * The types the IN-APP text preview renders (IDEA-2712 / GitHub #1169).
 *
 * A THIRD predicate, deliberately not an edit to either sibling above.
 * The three answer different questions and only look alike:
 *
 *  - `canOpenInViewer` (DR-16) — may the in-app IMAGE viewer decode this?
 *  - `canBrowserPreview` (DR-5) — may the BROWSER be handed this in a new
 *    tab? For `text/markdown` the honest answer stays NO: the browser
 *    downloads it. Widening DR-5 to reach #1169 would route markdown to
 *    the Open-in-new-tab action (`actions.ts`) and reproduce the exact
 *    download-instead-of-render behaviour the issue reports.
 *  - this one — do WE fetch the bytes and render them ourselves? That is
 *    a question about our own renderer, and it is the only one #1169 asks.
 *
 * NOT PART OF THE SERVER MIRROR. `internal/attachments/mime.go`'s
 * `inlineSafe` declares itself the mirror of `VIEWER_MIMES` +
 * `BROWSER_PREVIEW_MIMES` — the set the server may send with
 * `Content-Disposition: inline`. This set is deliberately outside that
 * pair and must never be added to it: we never ask the browser to inline
 * these bytes, we `fetch()` them (which a download disposition does not
 * impede) and render sanitized HTML ourselves. Keeping it out of the
 * mirror is what lets `text/markdown` preview in-app while still being
 * served as an attachment.
 *
 * MIME-EXACT, NEVER BY CATEGORY. `CategoryText` server-side CONTAINS the
 * force-download bucket — `text/html`, `text/javascript` and
 * `application/javascript` are all `CategoryText` (mime.go). A
 * category test would therefore admit exactly the types PLAN-2393 DR-6
 * forbids inlining. An allowlist excludes them by construction rather
 * than by anyone remembering to.
 *
 * SMALLER THAN WHAT WE COULD RENDER, on purpose. `text/csv`,
 * `text/tab-separated-values`, JSON, XML, YAML and TOML are all
 * allowlisted uploads and all renderable as raw text — and all left out,
 * because each has an obviously better rendering (a table; syntax
 * highlighting) that this unit does not build. Shipping them as raw text
 * now would make that better rendering a REGRESSION for anyone who got
 * used to the raw view.
 */
const TEXT_PREVIEW_MIMES: ReadonlySet<string> = new Set(['text/markdown', 'text/plain']);

/**
 * May the in-app text preview render this MIME? See
 * {@link TEXT_PREVIEW_MIMES} for the set and the three reasons it is
 * neither a category test nor a widening of `canBrowserPreview`.
 *
 * Fails closed like its siblings: unknown, unresolved and null MIMEs get
 * no renderer, so the surface shows its existing file panel.
 */
export function canPreviewAsText(mime: string | null | undefined): boolean {
	return TEXT_PREVIEW_MIMES.has(normalizeMime(mime));
}

/**
 * Within the text-preview set, is this the MARKDOWN one? (IDEA-2712)
 *
 * The arm needs to tell the two members apart — markdown goes through the
 * shared `marked` pipeline, plain text goes into a `<pre>` as text.
 *
 * THE EXTENSION IS CONSULTED, AND IT IS NOT BELT-AND-BRACES — IT IS THE PATH
 * THAT ACTUALLY FIRES. An uploaded `.md` is stored as **`text/plain`**, not
 * `text/markdown`. `attachments.ValidateUpload` sniffs the bytes with
 * `http.DetectContentType`, which answers `text/plain` for any prose, and
 * returns the SNIFFED entry; the extension is used only to REJECT a mismatch,
 * and `.md` → `text/markdown` shares `CategoryText` with `text/plain`, so
 * nothing rejects and the sniffed type is what lands in the row. Measured, not
 * assumed: `ValidateUpload([]byte("# Heading\n..."), "preview.md")` returns
 * `mime="text/plain"`.
 *
 * So a MIME-only test is a branch that never runs for the files this feature
 * exists for. Every unit test that hand-sets `mime_type: 'text/markdown'`
 * passes anyway, which is precisely why this was found in a browser and not
 * here: those fixtures encode what the author believed the system stores.
 *
 * The MIME check stays FIRST because it is the stronger signal when present —
 * an explicitly-typed row (a CLI upload that declares the type, or a future
 * server that trusts the extension) should be honoured regardless of filename.
 * The extension is the fallback, and it is gated on the MIME already being in
 * the text-preview set so a filename can never widen what previews.
 */
const MARKDOWN_EXTENSIONS: ReadonlySet<string> = new Set(['md', 'markdown']);

export function isMarkdownAttachment(
	mime: string | null | undefined,
	filename?: string | null
): boolean {
	if (normalizeMime(mime) === 'text/markdown') return true;
	// Gate on the set, never on the filename alone: the extension chooses a
	// RENDERER among types already admitted, and must not admit anything.
	if (!canPreviewAsText(mime)) return false;
	return MARKDOWN_EXTENSIONS.has(extensionOf(filename));
}

/**
 * Byte ceiling above which the text preview offers the existing download
 * affordance instead of rendering (IDEA-2712 / GitHub #1169).
 *
 * THE RECEIPT. Measured 2026-09-01 over two independent populations of
 * real markdown:
 *
 *   | sample                                   |  n | p50    | p90    | max   |
 *   |------------------------------------------|----|--------|--------|-------|
 *   | `*.md` in this repo (no node_modules)    | 25 | 3.6 KB |  42 KB | 82 KB |
 *   | `docs` collection item bodies (workspace) | 63 | 5.0 KB |  13 KB | 23 KB |
 *
 * Authored repo docs and Pad-native documents agree: real markdown is
 * single-digit-to-tens of KB. 1 MiB is ~12x the largest document observed
 * in either sample and ~25x the repo's p99.
 *
 * LOAD CONDITIONS. The cost this bounds is main-thread: `marked` plus
 * DOMPurify plus DOM construction, synchronously, in the viewer.
 *
 * ERROR DIRECTION, chosen deliberately: err toward RENDERING. An unusually
 * large but genuine document still previews; the cap exists for the
 * pathological case a 25 MiB upload bound admits (a log renamed `.md`, a
 * generated dump), not to enforce taste about document length. It sits
 * well under `defaultAttachmentMaxBytes` (25 MiB, handlers_attachments.go)
 * so it has a live range to act in.
 *
 * WHAT IT DOES NOT COVER: a small file that is expensive to render anyway
 * — deeply nested lists, thousands of inline links. Byte count is a proxy
 * for render cost, not a measure of it. If that bites, the answer is a
 * render-time budget, not a smaller byte cap; shrinking this number cannot
 * deliver that fix.
 *
 * Enforced from `size_bytes` METADATA where the surface HAS it — then no bytes
 * are requested at all. Where it does not (`LightboxImage.size_bytes` is
 * `number | null` by declaration, and most producers seed null, leaving the
 * metadata HEAD to fill it) the load necessarily starts first: the response
 * bound holds the line, and the late size re-decides and aborts what is in
 * flight. So the cap always bounds what is RENDERED; it saves the TRANSFER only
 * when the size is known before the load. Claiming otherwise would be false for
 * the common seeding (codex R1 #1).
 */
export const TEXT_PREVIEW_MAX_BYTES = 1 << 20; // 1 MiB
