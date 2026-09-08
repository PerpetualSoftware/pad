/**
 * Markdown reference resolver for `pad-attachment:UUID`.
 *
 * Item content stores attachments as opaque references — `pad-attachment:UUID`
 * embedded in standard markdown image/link syntax:
 *
 *   ![alt text](pad-attachment:abcdef-123)    — image embed
 *   [filename.pdf](pad-attachment:abcdef-123) — file chip
 *
 * The actual download URL is computed at render time from the workspace slug
 * and the current backend, so item content survives a backend migration
 * (FS → S3) untouched. See DOC-865 (Attachments — architecture & migration).
 *
 * This module exports pure helpers that produce HTML strings for each
 * rendering case (image, file chip, missing placeholder). They're integrated
 * into the marked pipeline by `$lib/utils/markdown.ts` via the `image` and
 * `link` renderer overrides — see that file for the wiring.
 *
 * The Go-side companion lives at `internal/server/render/attachments.go`;
 * keep the two implementations in lock-step so server-rendered and
 * client-rendered output match byte-for-byte for the same input.
 */

/** Minimal attachment metadata required to render a reference. */
export interface AttachmentMeta {
	id: string;
	mime_type: string;
	filename: string;
	size_bytes: number;
	width?: number | null;
	height?: number | null;
	/**
	 * The derived (thumbnail) variants the SERVER holds for this attachment
	 * (BUG-2964), e.g. `['thumb-sm', 'thumb-md']`. Sourced from the
	 * `X-Pad-Attachment-Derived` response header via `fetchAttachmentMetadata`.
	 *
	 * PER-VARIANT, NOT A BOOLEAN (codex round 1): derivation writes each variant
	 * independently, so "some thumbnail exists" is not the same question as "the
	 * one this render will REQUEST exists". A boolean would say "image" when only
	 * `thumb-sm` was present and the render asked for `thumb-md`, the endpoint
	 * would fall back to the undecodable original, and the broken image would be
	 * back with more machinery in front of it.
	 *
	 * `undefined` means UNKNOWN — an older server, or a caller that never probed
	 * — and is not the same as `[]`. See `shouldEmbedAsImage`.
	 */
	derived_variants?: string[] | undefined;
}

/**
 * Looks up an attachment by UUID. Should return `null` or `undefined` when
 * the attachment is missing or deleted — the resolver renders a "missing"
 * placeholder in that case so the document doesn't show a broken-image icon.
 */
export type AttachmentResolver = (uuid: string) => AttachmentMeta | null | undefined;

/**
 * Builds the byte URL for an attachment reference, overriding the default
 * workspace-scoped path (`attachmentDownloadUrl`). Public share pages supply
 * one that points at the token-scoped `/api/v1/s/{token}/attachments/{id}`
 * endpoint and carries the short-lived signature for protected links
 * (BUG-2389 2b / TASK-2637). When absent, callers fall back to
 * `attachmentDownloadUrl(workspaceSlug, …)`, so every existing surface is
 * unaffected.
 */
export type AttachmentUrlBuilder = (
	uuid: string,
	variant?: 'thumb-sm' | 'thumb-md' | 'original'
) => string;

/** URL scheme used in markdown content. Mirrored on the Go side. */
const ATTACHMENT_URL_PREFIX = 'pad-attachment:';

const API_BASE = '/api/v1';

/** True if `href` is a `pad-attachment:UUID` reference. */
export function isAttachmentHref(href: string | null | undefined): boolean {
	return typeof href === 'string' && href.startsWith(ATTACHMENT_URL_PREFIX);
}

/**
 * Extract the UUID from a `pad-attachment:UUID` reference. Returns `null`
 * when `href` is not a pad-attachment reference, or when the UUID portion
 * is empty / whitespace.
 */
export function parseAttachmentHref(href: string | null | undefined): string | null {
	if (!isAttachmentHref(href)) return null;
	const uuid = (href as string).slice(ATTACHMENT_URL_PREFIX.length).trim();
	return uuid === '' ? null : uuid;
}

/**
 * Build the canonical download URL for an attachment. Suitable for
 * `<img src>` and anchor `href`. Optional variant (`thumb-sm` | `thumb-md`)
 * falls back to the original on the server when no derived row exists,
 * so callers can always request a thumbnail and get something renderable.
 */
export function attachmentDownloadUrl(
	workspaceSlug: string,
	attachmentId: string,
	variant?: 'thumb-sm' | 'thumb-md' | 'original'
): string {
	const base = `${API_BASE}/workspaces/${encodeURIComponent(workspaceSlug)}/attachments/${encodeURIComponent(attachmentId)}`;
	return variant ? `${base}?variant=${encodeURIComponent(variant)}` : base;
}

/** Format a byte count as a human-readable string ("832 B", "1.2 MB", "5 GB"). */
export function formatAttachmentSize(bytes: number): string {
	if (!Number.isFinite(bytes) || bytes < 0) return '';
	if (bytes < 1024) return `${bytes} B`;
	const units = ['KB', 'MB', 'GB', 'TB'];
	let n = bytes / 1024;
	let i = 0;
	while (n >= 1024 && i < units.length - 1) {
		n /= 1024;
		i++;
	}
	return `${n < 10 ? n.toFixed(1) : Math.round(n).toString()} ${units[i]}`;
}

/**
 * True if the MIME type renders inline as an image. Mirrors `image/*` from
 * the server-side allowlist (image/png, image/jpeg, image/gif, image/webp,
 * image/avif, image/heic, image/heif). Anything else falls back to a chip.
 *
 * NO LONGER THE EMBED DECISION on its own — see `shouldEmbedAsImage`. Kept
 * because it is still the honest answer to "is this labelled as an image",
 * which the unknown-server fallback needs.
 */
export function isImageMime(mime: string | null | undefined): boolean {
	return typeof mime === 'string' && mime.toLowerCase().trimStart().startsWith('image/');
}

/**
 * The types a browser paints inside an `<img>` (BUG-2964).
 *
 * DUPLICATED from `$lib/attachments/display.ts::IMG_PAINTABLE_MIMES` rather
 * than imported, and that is deliberate: this module's header requires it to
 * stay byte-for-byte in lock-step with the Go renderer in
 * `internal/server/render/attachments.go`, so its inputs have to be things the
 * Go side can hold too. An import would tie the markdown renderer to the
 * attachment-UI module and leave the Go copy with nothing to mirror. The two
 * lists are asserted equal by a test rather than by a comment.
 */
const IMG_PAINTABLE_MIMES: ReadonlySet<string> = new Set([
	'image/png',
	'image/jpeg',
	'image/gif',
	'image/webp',
	'image/avif',
	'image/svg+xml'
]);

/**
 * Should this attachment be embedded as an `<img>`, or as a downloadable file
 * chip? (BUG-2964)
 *
 * THE RULE IS A DISJUNCTION, and each half is load-bearing:
 *
 *   embed as <img>  iff  a derived variant exists          (the server can
 *                                                           serve decodable
 *                                                           bytes)
 *                   OR   the browser paints the original   (no derivative
 *                                                           needed)
 *
 * Why not MIME prefix, which is what this used to be: a pure-Go build derives
 * no thumbnail for HEIC, and the byte endpoint silently falls back to the
 * ORIGINAL when the variant row is missing — so `image/heic` produced an
 * `<img>` pointed at HEIC bytes, which Chrome and Firefox render as the
 * broken-image icon.
 *
 * Why not variant availability alone: the same pure-Go build derives no AVIF
 * thumbnail either, and AVIF is decoded by every current browser. Availability
 * alone would turn a perfectly good AVIF embed into a chip. There is a CONTROL
 * leg for exactly this.
 *
 * `wantVariant` MUST be the variant the caller is about to put in the URL.
 * Asking about a variant you will not request answers a question nobody has —
 * derivation writes each variant independently, so `thumb-sm` existing says
 * nothing about `thumb-md` (codex round 1). `'original'` or omitted means the
 * render points at the original, so only the paintable half can carry it.
 *
 * UNKNOWN (`derived_variants === undefined`) falls back to the old MIME-prefix
 * behaviour. That is the compatibility hinge: an older server sends no header,
 * and treating its silence as "no variants" would flip every embed in every
 * document against it.
 */
export function shouldEmbedAsImage(
	meta: AttachmentMeta,
	wantVariant?: 'thumb-sm' | 'thumb-md' | 'original'
): boolean {
	if (!isImageMime(meta.mime_type)) return false;
	if (meta.derived_variants === undefined) return true;
	if (
		wantVariant !== undefined &&
		wantVariant !== 'original' &&
		meta.derived_variants.includes(wantVariant)
	)
		return true;
	return IMG_PAINTABLE_MIMES.has((meta.mime_type ?? '').toLowerCase().split(';')[0].trim());
}

/**
 * Escape HTML-significant characters so user-controlled strings can be
 * safely interpolated into attribute values and text nodes. Local copy so
 * this module stays self-contained — DOMPurify still has the final say
 * downstream when these helpers are piped through `sanitizeMarkdownHtml`.
 */
function escapeHtml(s: string): string {
	return s
		.replace(/&/g, '&amp;')
		.replace(/</g, '&lt;')
		.replace(/>/g, '&gt;')
		.replace(/"/g, '&quot;')
		.replace(/'/g, '&#39;');
}

/**
 * Render an image attachment as an inline `<img>` element. Width/height
 * attributes are emitted when both are known and positive so the browser
 * can reserve layout space before the bytes arrive — avoiding the reflow
 * jank that would otherwise hit on every paste-and-render cycle in the
 * editor preview.
 *
 * The `data-attachment-id` attribute is the editor's hook for click-to-
 * zoom, rotate, and crop interactions (TASK-879/880).
 */
export function renderAttachmentImage(
	meta: AttachmentMeta,
	alt: string,
	workspaceSlug: string,
	variant: 'thumb-sm' | 'thumb-md' = 'thumb-md',
	urlBuilder?: AttachmentUrlBuilder
): string {
	const src = urlBuilder
		? urlBuilder(meta.id, variant)
		: attachmentDownloadUrl(workspaceSlug, meta.id, variant);
	const altText = alt && alt.trim() !== '' ? alt : meta.filename;
	const sizeAttrs =
		meta.width && meta.height && meta.width > 0 && meta.height > 0
			? ` width="${meta.width}" height="${meta.height}"`
			: '';
	return `<img src="${escapeHtml(src)}" data-attachment-id="${escapeHtml(meta.id)}" alt="${escapeHtml(altText)}"${sizeAttrs}>`;
}

/**
 * Render a non-image attachment as a Notion-style file chip. The chip is
 * an anchor element so users can click to download / open. `target=_blank`
 * + `rel=noopener noreferrer` matches our other external-link defaults.
 */
export function renderAttachmentChip(
	meta: AttachmentMeta,
	displayText: string | undefined,
	workspaceSlug: string,
	urlBuilder?: AttachmentUrlBuilder
): string {
	const href = urlBuilder ? urlBuilder(meta.id) : attachmentDownloadUrl(workspaceSlug, meta.id);
	const label = displayText && displayText.trim() !== '' ? displayText : meta.filename;
	const size = formatAttachmentSize(meta.size_bytes);
	const sizeSpan = size ? ` <span class="file-chip-size">· ${escapeHtml(size)}</span>` : '';
	return `<a class="file-chip" href="${escapeHtml(href)}" data-attachment-id="${escapeHtml(meta.id)}" download="${escapeHtml(meta.filename)}" target="_blank" rel="noopener noreferrer"><span class="file-chip-icon" aria-hidden="true">📄</span><span class="file-chip-name">${escapeHtml(label)}</span>${sizeSpan}</a>`;
}

/**
 * Render a placeholder for a missing/deleted attachment. Keeps the document
 * layout intact and tells the user *why* there's no image — a broken-image
 * icon would suggest a network failure, but this is a permanent metadata
 * state (the row is soft-deleted or the UUID was never valid).
 */
export function renderAttachmentMissing(uuid: string, alt: string): string {
	// (See also renderAttachmentUnavailable below for surfaces where the
	// attachment EXISTS but this viewer has no byte access.)
	const safeAlt = alt && alt.trim() !== '' ? escapeHtml(alt) : 'Missing attachment';
	return `<span class="attachment-missing" data-attachment-id="${escapeHtml(uuid)}" title="This attachment is missing or has been deleted">📎 ${safeAlt}</span>`;
}

/**
 * Resolve a `pad-attachment:UUID` image-syntax reference
 * (`![alt](pad-attachment:UUID)`). Falls back to a file chip when the
 * attachment exists but isn't an image MIME — the markdown author asked
 * for an embed, but a PDF or zip can't sensibly inline.
 */
/**
 * Placeholder for an attachment this SURFACE cannot serve — it exists,
 * the viewer just has no byte access here (e.g. public share pages until
 * the token-scoped read path ships, BUG-2389). Distinct from
 * renderAttachmentMissing: saying "missing or deleted" about an
 * attachment that exists would be false.
 */
export function renderAttachmentUnavailable(uuid: string, alt: string): string {
	const safeAlt = escapeHtml(alt && alt.trim() !== '' ? alt : 'attachment');
	// The explanation is VISIBLE text, not just a title attribute — share
	// pages get read on touch devices where hover tooltips don't exist.
	return `<span class="attachment-missing attachment-unavailable" data-attachment-id="${escapeHtml(uuid)}" title="Attachments aren't available on shared pages yet">📎 ${safeAlt} <span class="attachment-unavailable-note">(not available on shared pages)</span></span>`;
}

export function resolveAttachmentImage(
	href: string,
	alt: string,
	workspaceSlug: string,
	resolver: AttachmentResolver,
	variant: 'thumb-sm' | 'thumb-md' = 'thumb-md',
	missing: (uuid: string, alt: string) => string = renderAttachmentMissing,
	urlBuilder?: AttachmentUrlBuilder
): string {
	const uuid = parseAttachmentHref(href);
	if (uuid === null) return '';
	const meta = resolver(uuid);
	if (!meta) return missing(uuid, alt);
	// The SAME `variant` that renderAttachmentImage is about to put in the URL —
	// the decision and the request must not disagree about which one is meant.
	if (shouldEmbedAsImage(meta, variant))
		return renderAttachmentImage(meta, alt, workspaceSlug, variant, urlBuilder);
	return renderAttachmentChip(
		meta,
		alt && alt.trim() !== '' ? alt : meta.filename,
		workspaceSlug,
		urlBuilder
	);
}

/**
 * Resolve a `pad-attachment:UUID` link-syntax reference
 * (`[text](pad-attachment:UUID)`). Always renders as a file chip — link
 * syntax indicates the user wants a downloadable reference, not an inline
 * embed, even when the underlying MIME is an image.
 */
export function resolveAttachmentLink(
	href: string,
	text: string,
	workspaceSlug: string,
	resolver: AttachmentResolver,
	missing: (uuid: string, alt: string) => string = renderAttachmentMissing,
	urlBuilder?: AttachmentUrlBuilder
): string {
	const uuid = parseAttachmentHref(href);
	if (uuid === null) return '';
	const meta = resolver(uuid);
	if (!meta) return missing(uuid, text);
	return renderAttachmentChip(meta, text, workspaceSlug, urlBuilder);
}
