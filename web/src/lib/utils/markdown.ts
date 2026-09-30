import { marked, Renderer, type Tokens } from 'marked';
import DOMPurify from 'dompurify';
import type { Item } from '$lib/types';
import { FOLLOWS_TITLE_PREFIX } from './followsTitle';
import { itemUrlId } from '$lib/types';
import {
	type AttachmentResolver,
	type AttachmentUrlBuilder,
	isAttachmentHref,
	resolveAttachmentImage,
	resolveAttachmentLink
} from '$lib/markdown/attachments';

/**
 * The wiki-link bracket grammar — the ONE copy on the JS side.
 *
 * Body production: a backslash-escaped character, or any character that is
 * neither `]` nor `\`. Kept deliberately permissive so anything the editor can
 * save is also indexable; see `wikiLinkPattern` in internal/links/extract.go,
 * which is the server-side half of the same grammar.
 *
 * ## Why `\\[^\n]` and not `\\.` (BUG-2834)
 *
 * The Go and JS patterns used to be BYTE-IDENTICAL source text — both spelled
 * the escape alternative `\\.` — and they still did not mean the same thing,
 * because the two languages do not agree on `.`:
 *
 *   - Go (RE2): `.` matches everything except LF (U+000A).
 *   - JavaScript: `.` additionally excludes CR, U+2028 and U+2029 — the full
 *     ECMAScript LineTerminator set.
 *
 * So a body containing a backslash immediately before CR / U+2028 / U+2029 was
 * INDEXED by the server and NOT RENDERED here: the backlink panel claimed a
 * link the document refused to draw. CRLF line endings make the CR case the
 * plausible one — a body ending in a backslash right before a CRLF break.
 *
 * `[^\n]` states Go's definition explicitly, so the two sides now agree by
 * construction rather than by looking alike. Measured, not reasoned: the three
 * divergent code points plus VT / FF / U+0085 (which always agreed, and which
 * bound the divergence to exactly the LineTerminator set) are pinned in
 * testdata/wiki_grammar_corpus.json and asserted from both languages.
 *
 * LF stays excluded on BOTH sides — that was never the divergence, and
 * `scanBracketBody` in internal/links/links.go depends on it.
 *
 * Exported as SOURCE TEXT rather than as a shared RegExp object: a `/g` regex
 * carries `lastIndex` state, and handing the same object to both a `replace()`
 * and a test's `exec()` would couple them through it.
 */
export const WIKI_LINK_PATTERN_SOURCE = String.raw`\[\[((?:\\[^\n]|[^\]\\])+)\]\]`;

// The shared instance for this module's two rewrite sites. Safe to reuse
// despite the `/g` flag because `String.prototype.replace` resets `lastIndex`
// both before and after a global match — unlike `exec`/`test`, which do not.
const WIKI_LINK_PATTERN = new RegExp(WIKI_LINK_PATTERN_SOURCE, 'g');

// Mirror of marked's internal cleanUrl() — percent-encodes the href so the
// rendered HTML stays well-formed even when input contains spaces, quotes, or
// other URL-unsafe characters. The %25 → % round-trip avoids double-encoding
// hrefs that already contain percent-encoded bytes (e.g. `%20`). Returns null
// when encodeURI throws on a malformed surrogate, matching marked's default
// behavior of degrading to plain text rather than emitting a broken anchor.
// DOMPurify still has the final say on URL safety; this is defense-in-depth
// plus correctness for the intermediate HTML.
function cleanUrl(href: string): string | null {
	try {
		return encodeURI(href).replace(/%25/g, '%');
	} catch {
		return null;
	}
}

// Per-call attachment context. Set by renderMarkdown before invoking marked()
// and cleared in the finally block. Marked's parsing is synchronous, so a
// module-level slot is safe — every call drives the renderer through
// completion before returning. We avoid a per-call `new Renderer()` because
// the global renderer is also reached by `marked.parseInline` and the share
// page's bare `marked()` calls; threading context through the global instance
// keeps both paths consistent without forcing every call site to opt in.
type AttachmentRenderContext = {
	resolver: AttachmentResolver;
	workspaceSlug: string;
	// Thumbnail variant for inline image embeds. Comments render small
	// (thumb-sm, 256px) thumbnails; other surfaces default to thumb-md.
	imageVariant: 'thumb-sm' | 'thumb-md';
	// Optional placeholder override for unresolved references. Share
	// pages pass renderAttachmentUnavailable — "missing or deleted"
	// would be false there; the attachment exists, the surface just has
	// no byte access yet (BUG-2389).
	missing?: (uuid: string, alt: string) => string;
	// Optional byte-URL override. Share pages pass one that targets the
	// token-scoped /api/v1/s/{token}/attachments/{id} endpoint (with the
	// signed suffix for protected links); without it, URLs are built from
	// workspaceSlug via attachmentDownloadUrl (BUG-2389 2b / TASK-2637).
	urlBuilder?: AttachmentUrlBuilder;
};
let currentAttachmentCtx: AttachmentRenderContext | null = null;

// Custom renderer to open external links in new tabs.
//
// Use a regular `function` (not an arrow) so `this` resolves to the Renderer
// instance — marked invokes overrides via `override.apply(rendererInstance, args)`,
// which gives us access to `this.parser.parseInline(tokens)`. We render the
// already-parsed inline tokens; re-parsing the raw text via
// `marked.parseInline(tokens.map(t => t.raw).join(''))` would re-tokenize bare
// URLs in the link text as autolinks and recurse infinitely through this same
// `link` renderer (e.g. for content like `https://example.com` inside a comment).
const renderer = new marked.Renderer();
renderer.link = function (this: Renderer, { href, title, text: rawText, tokens }: Tokens.Link) {
	// pad-attachment:UUID links resolve to a file chip when a resolver is in
	// scope. Without one (e.g. the bare `marked()` calls on the share page
	// before that route opts in) the reference falls through to the default
	// link rendering — the user sees the literal `pad-attachment:UUID` href,
	// which is graceful degradation for SSR/preview environments that don't
	// have the attachment registry hydrated yet.
	//
	// We pass the link token's raw `text` (not parseInline output) to the
	// chip helper because renderAttachmentChip HTML-escapes its label
	// argument. Feeding pre-rendered HTML (e.g. <strong>Report</strong>
	// from `[**Report**](pad-attachment:…)`) would double-escape into
	// `&lt;strong&gt;Report&lt;/strong&gt;`. Plain text matches the Go
	// side's regex-based extraction and keeps both renderers byte-aligned;
	// markdown emphasis inside chip labels degrades to literal markers,
	// which is acceptable for the filename-style labels chips usually carry.
	if (currentAttachmentCtx && isAttachmentHref(href)) {
		return resolveAttachmentLink(
			href,
			rawText,
			currentAttachmentCtx.workspaceSlug,
			currentAttachmentCtx.resolver,
			currentAttachmentCtx.missing,
			currentAttachmentCtx.urlBuilder
		);
	}
	const text = this.parser.parseInline(tokens);
	const cleanHref = cleanUrl(href);
	if (cleanHref === null) {
		// encodeURI failed (malformed surrogate). Drop the link and emit just
		// the parsed link text — same fallback marked's default renderer uses.
		return text;
	}
	const isExternal = /^https?:\/\//.test(cleanHref);
	const titleAttr = title ? ` title="${escapeHtml(title)}"` : '';
	if (isExternal) {
		return `<a href="${cleanHref}"${titleAttr} target="_blank" rel="noopener noreferrer" class="external-link">${text}<span class="external-icon" aria-hidden="true"> ↗</span></a>`;
	}
	return `<a href="${cleanHref}"${titleAttr}>${text}</a>`;
};

renderer.image = function (this: Renderer, { href, title, text }: Tokens.Image) {
	// pad-attachment:UUID image references resolve to <img> for image MIMEs
	// or a file chip for non-image MIMEs. Without a resolver in scope, fall
	// through to the default image rendering so the literal href is at
	// least visible (rendered as a broken image, which is the right UX
	// signal: "we couldn't resolve this attachment").
	if (currentAttachmentCtx && isAttachmentHref(href)) {
		return resolveAttachmentImage(
			href,
			text,
			currentAttachmentCtx.workspaceSlug,
			currentAttachmentCtx.resolver,
			currentAttachmentCtx.imageVariant,
			currentAttachmentCtx.missing,
			currentAttachmentCtx.urlBuilder
		);
	}
	// Default image rendering. Mirrors marked's stdout behavior so
	// overriding renderer.image here doesn't regress existing markdown.
	const cleanHref = cleanUrl(href);
	if (cleanHref === null) return escapeHtml(text);
	const titleAttr = title ? ` title="${escapeHtml(title)}"` : '';
	return `<img src="${cleanHref}" alt="${escapeHtml(text)}"${titleAttr}>`;
};

marked.use({ renderer });

// Tags produced by marked + our wiki-link renderer. Anything outside this
// allowlist (script, iframe, object, svg, form, etc.) gets stripped.
const MARKDOWN_ALLOWED_TAGS = [
	'a', 'abbr', 'b', 'blockquote', 'br', 'code', 'del', 'div', 'em',
	'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'hr', 'i', 'img', 'ins', 'kbd',
	'li', 'ol', 'p', 'pre', 's', 'span', 'strong', 'sub', 'sup',
	'table', 'tbody', 'td', 'th', 'thead', 'tr', 'ul', 'input'
] as const;

// Attributes we emit from markdown + wiki-links. DOMPurify already
// strips javascript: and data: hrefs via its default URL policy.
const MARKDOWN_ALLOWED_ATTR = [
	'href', 'title', 'target', 'rel', 'class', 'aria-hidden',
	'alt', 'src', 'id', 'name', 'align', 'type', 'checked', 'disabled',
	// <ol start="N"> — marked emits this for lists that don't begin at 1.
	'start',
	// Attachment renderer attributes. `data-attachment-id` is the editor's
	// hook for click-to-zoom / rotate / crop interactions; `download` lets
	// the browser save file chips with their canonical filename; `width`
	// and `height` reserve layout space for inline images so the page
	// doesn't reflow when the bytes arrive. ALLOW_DATA_ATTR stays false so
	// only this single data-* attribute is permitted.
	'data-attachment-id', 'download', 'width', 'height'
] as const;

/**
 * Sanitize HTML produced from markdown. Removes any tags/attributes outside
 * our markdown allowlist — most importantly <script>, inline event handlers
 * (onerror, onclick, ...), and javascript:/data: URLs. All rendered-markdown
 * output that ends up in `{@html}` MUST pass through this first.
 *
 * Runs client-side only. In SSR/prerender contexts there is no DOM, so we
 * return an empty string rather than emitting unsanitized HTML — these
 * contexts don't render user-generated markdown anyway (items and comments
 * load via the API at runtime), so the empty fallback is a safe no-op.
 */
export function sanitizeMarkdownHtml(html: string): string {
	if (typeof window === 'undefined') return '';
	return DOMPurify.sanitize(html, {
		ALLOWED_TAGS: [...MARKDOWN_ALLOWED_TAGS],
		ALLOWED_ATTR: [...MARKDOWN_ALLOWED_ATTR],
		ALLOW_DATA_ATTR: false,
		// Keep target="_blank" on external links (marked renderer sets it).
		ADD_ATTR: ['target'],
		// Disallow unknown protocols outright.
		ALLOWED_URI_REGEXP: /^(?:(?:https?|mailto|ftp|tel):|[^a-z]|[a-z+.\-]+(?:[^a-z+.\-:]|$))/i
	});
}

// HTML block allowlist. Permits everything markdown allows plus structural
// elements (section/article/figure/etc.), media (iframe/video/audio), and
// inline `style`. Used only by `sanitizeHtmlBlock` — the markdown surface
// (comments + rendered item bodies) keeps the tighter allowlist above.
const HTML_BLOCK_ALLOWED_TAGS = [
	...MARKDOWN_ALLOWED_TAGS,
	'iframe',
	'section', 'article', 'aside', 'header', 'footer', 'main', 'nav',
	'figure', 'figcaption', 'picture', 'source',
	'video', 'audio'
] as const;

// Additional attributes for HTML blocks. Inline `style` is permitted here
// (intentionally — styled callouts, custom typography, embed wrappers are
// the whole point of HTML blocks). Media + iframe attributes cover the
// embed allowlist hosts. DOMPurify's URL filter still strips javascript:
// from any href/src.
const HTML_BLOCK_ALLOWED_ATTR = [
	...MARKDOWN_ALLOWED_ATTR,
	'style',
	// iframe attributes
	'frameborder', 'allow', 'allowfullscreen', 'loading', 'referrerpolicy', 'sandbox',
	// media attributes (autoplay deliberately omitted — embedded autoplay
	// is hostile to the reader and the embed hosts handle it via their own
	// query parameters when intentional)
	'controls', 'loop', 'muted', 'playsinline', 'poster', 'preload'
] as const;

/**
 * iframe `src` allowlist — only iframes pointing at one of these embed
 * hosts survive sanitization. Adding a new host = one PR amending this
 * array + a test case. Keep it small.
 *
 * The leading `^https:` is intentional: http: embeds are rejected (no
 * mixed-content for embeds), and the host literals match the canonical
 * embed URLs each provider documents.
 */
const IFRAME_HOST_ALLOWLIST: readonly RegExp[] = [
	/^https:\/\/(www\.)?youtube(-nocookie)?\.com\/embed\//,
	/^https:\/\/player\.vimeo\.com\/video\//,
	/^https:\/\/(www\.)?loom\.com\/embed\//,
	/^https:\/\/codesandbox\.io\/embed\//
];

/**
 * Returns true iff `src` matches one of the {@link IFRAME_HOST_ALLOWLIST}
 * regexes. Trims whitespace; case sensitivity is delegated to each regex
 * (currently all anchored to lowercase `https://`, matching DOMPurify's
 * URL normalization).
 */
export function isAllowedIframeSrc(src: string): boolean {
	const trimmed = src.trim();
	return IFRAME_HOST_ALLOWLIST.some(re => re.test(trimmed));
}

/**
 * Sanitize raw HTML for rendering inside an HTML block node. Different
 * from `sanitizeMarkdownHtml`: permits iframes (allowlisted hosts only),
 * inline `style`, and additional structural/media tags. Strips scripts,
 * inline event handlers, javascript:/data: URLs, and any iframe whose
 * `src` fails {@link isAllowedIframeSrc}.
 *
 * SSR-safe: returns "" when `window` is undefined, mirroring
 * `sanitizeMarkdownHtml`.
 *
 * Storage is NOT modified — sanitization is render-time only. The user's
 * raw HTML stays in the editor's node attrs so they can inspect/edit
 * exactly what they typed (including content this function strips).
 */
export function sanitizeHtmlBlock(html: string): string {
	if (typeof window === 'undefined') return '';
	// DOMPurify's tag allowlist accepts iframes here, but we need a host
	// allowlist on top — that lives in this hook. Synchronous sanitize +
	// try/finally cleanup means the hook never leaks across calls (and
	// JavaScript's single-threaded execution rules out interleaving with
	// concurrent sanitizeMarkdownHtml calls).
	// DOMPurify's UponSanitizeElementHook signature widens currentNode to
	// Node (not Element) since text/comment nodes also pass through. We
	// only act on iframes — narrow safely via nodeName before touching
	// attribute APIs.
	const hook = (currentNode: Node) => {
		if (currentNode.nodeName === 'IFRAME') {
			const el = currentNode as Element;
			const src = el.getAttribute('src') ?? '';
			if (!isAllowedIframeSrc(src)) {
				el.parentNode?.removeChild(el);
			}
		}
	};
	DOMPurify.addHook('uponSanitizeElement', hook);
	try {
		return DOMPurify.sanitize(html, {
			ALLOWED_TAGS: [...HTML_BLOCK_ALLOWED_TAGS],
			ALLOWED_ATTR: [...HTML_BLOCK_ALLOWED_ATTR],
			ALLOW_DATA_ATTR: false,
			ADD_ATTR: ['target'],
			ALLOWED_URI_REGEXP: /^(?:(?:https?|mailto|ftp|tel):|[^a-z]|[a-z+.\-]+(?:[^a-z+.\-:]|$))/i
		});
	} finally {
		DOMPurify.removeHook('uponSanitizeElement');
	}
}

/**
 * Escape HTML-significant characters so a user-controlled string can be
 * safely interpolated into attribute values / text nodes.
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
 * Render markdown with wiki-link resolution. Output is HTML-sanitized via
 * {@link sanitizeMarkdownHtml} before being returned; consumers can safely
 * pipe the result to `{@html}` without additional escaping.
 *
 * @param visibleCollectionSlugs - Set of collection slugs the user can see.
 *   undefined = all visible (no filtering). Empty set = nothing visible (anonymous).
 * @param attachmentResolver - Optional lookup that resolves
 *   `pad-attachment:UUID` references to image / file-chip / missing HTML.
 *   When omitted, those references render as plain markdown links pointing
 *   at the literal `pad-attachment:UUID` href — clearly broken in the UI,
 *   which is the right signal for unresolved environments.
 * @param attachmentImageVariant - Thumbnail variant for inline image
 *   embeds. Defaults to `thumb-md` (1024px); comments pass `thumb-sm`
 *   (256px) so pasted screenshots render as compact thumbnails.
 */
export function renderMarkdown(
	content: string,
	items: Item[],
	workspaceSlug: string,
	username?: string,
	visibleCollectionSlugs?: Set<string>,
	attachmentResolver?: AttachmentResolver,
	attachmentImageVariant: 'thumb-sm' | 'thumb-md' = 'thumb-md'
): string {
	// Body may contain backslash-escaped chars (`\]`, `\\`, `\|`) — the SAME
	// pattern object as wikiLinksToMarkdown, so the two renderers cannot drift
	// apart in accepted syntax (BUG-1744 aligned them; BUG-2834 removed the
	// second copy that let them look aligned while diverging).
	const withLinks = content.replace(WIKI_LINK_PATTERN, (_match, body: string) => {
		// Cross-workspace form: [[workspace-slug::REF]] or [[workspace-slug::REF|Display]].
		// `::` is the unambiguous separator. The workspace prefix is recognized
		// only when both the slug AND the right-hand side match their expected
		// shapes — otherwise the body falls through to legacy [[Title]] handling.
		const xw = parseCrossWorkspaceBody(body);
		if (xw) {
			if (xw.workspace === workspaceSlug) {
				// Same-workspace cross-ws form: behave identically to [[REF]],
				// resolving against the in-memory item list. Falls through to
				// the broken-link span if the ref isn't loaded.
				const sameWsItem = findItemByRef(items, xw.ref);
				const display = xw.display ?? (sameWsItem?.title ?? xw.ref);
				const safeDisplay = escapeHtml(display);
				if (sameWsItem && sameWsItem.collection_slug) {
					if (visibleCollectionSlugs !== undefined && !visibleCollectionSlugs.has(sameWsItem.collection_slug)) {
						return `<span class="doc-link locked" title="You don't have access to this item">🔒 ${safeDisplay}</span>`;
					}
					const prefix = username ? `/${username}/${workspaceSlug}` : `/${workspaceSlug}`;
					return `<a href="${prefix}/${sameWsItem.collection_slug}/${itemUrlId(sameWsItem)}" class="doc-link">${safeDisplay}</a>`;
				}
				return `<span class="doc-link broken">${safeDisplay}</span>`;
			}
			// Cross-workspace: emit a link to the resolver route. Validation
			// happens server-side at click time — the renderer has no
			// visibility into the target workspace's items.
			//
			// URL shape: `/-/r/{workspace}/{ref}`. The leading `/-/r/` is
			// the resolver's sentinel prefix (Codex round-2 P1.4 / Option
			// B); it can't collide with any user-namespace URL because
			// username + collection slugs both require letter-led. The
			// `username` arg is ignored for cross-workspace links because
			// the resolver derives the canonical user-namespace segment
			// from the target workspace's owner — without that, links
			// would silently 404 when the rendering page's username
			// doesn't own the target workspace.
			//
			// URL components are NOT percent-encoded here. parseCrossWorkspaceBody
			// already vetted `xw.workspace` against WORKSPACE_SLUG_PATTERN
			// (`^[a-z0-9][a-z0-9-]*$`) and `xw.ref` against REF_PATTERN
			// (`^[A-Za-z][A-Za-z0-9]*-\d+$`) — both produce URL-safe ASCII
			// by construction. wikiLinksToMarkdown emits the same bytes
			// verbatim, so the encode/no-encode choice is symmetric across
			// both functions (Codex round-1 sanity sweep).
			const safeDisplay = escapeHtml(xw.display ?? `${xw.workspace}::${xw.ref}`);
			return `<a href="/-/r/${xw.workspace}/${xw.ref}" class="doc-link cross-workspace">${safeDisplay}</a>`;
		}
		// Same-workspace resolution, shared with wikiLinksToMarkdown (the
		// Tiptap editor path) so the two renderers resolve [[REF]] /
		// [[REF|Display]] / [[Title]] identically — renderMarkdown previously
		// lacked the ref-based lookup, so refs in comments rendered as broken
		// links (BUG-1744).
		//
		// Escape user-controlled text so it can't break out of attribute
		// quotes. sanitizeMarkdownHtml would strip the worst offenders after
		// the fact, but escaping up-front keeps the intermediate HTML valid
		// and avoids relying on the sanitizer to paper over bad markup.
		const { item, displayText } = resolveWikiBody(body, items);
		const safeText = escapeHtml(displayText);
		if (item && item.collection_slug) {
			// Check visibility: if a visibility set is provided, check it
			if (visibleCollectionSlugs !== undefined && !visibleCollectionSlugs.has(item.collection_slug)) {
				return `<span class="doc-link locked" title="You don't have access to this item">🔒 ${safeText}</span>`;
			}
			const prefix = username ? `/${username}/${workspaceSlug}` : `/${workspaceSlug}`;
			return `<a href="${prefix}/${item.collection_slug}/${itemUrlId(item)}" class="doc-link">${safeText}</a>`;
		}
		return `<span class="doc-link broken">${safeText}</span>`;
	});
	// Wire the attachment resolver into the global renderer for the duration
	// of this synchronous parse. The try/finally ensures we never leak the
	// context across calls, even when marked throws on malformed input.
	// Save/restore (not clear-to-null) so a nested render can't strand an
	// outer caller's context; no-resolver still means "no attachment
	// resolution during THIS parse", hence the explicit null arm.
	const prevAttachmentCtx = currentAttachmentCtx;
	currentAttachmentCtx = attachmentResolver
		? {
				resolver: attachmentResolver,
				workspaceSlug,
				imageVariant: attachmentImageVariant
			}
		: null;
	try {
		return sanitizeMarkdownHtml(marked(withLinks) as string);
	} finally {
		currentAttachmentCtx = prevAttachmentCtx;
	}
}

/**
 * Render plain markdown through the shared `marked` pipeline WITH an
 * attachment context but WITHOUT renderMarkdown's wiki-link / item-URL
 * machinery (BUG-2389). Built for the public share route: wiki-links
 * there deliberately stay inert text (they'd point into the
 * authenticated app), but `pad-attachment:` references must stop
 * rendering as broken images. Returns UNSANITIZED HTML — the share page
 * owns its own single DOMPurify pass, and keeping one {@html} source
 * there means no second, divergent sanitization path.
 */
export function renderMarkedWithAttachments(
	content: string,
	ctx: {
		resolver: AttachmentResolver;
		workspaceSlug: string;
		imageVariant?: 'thumb-sm' | 'thumb-md';
		missing?: (uuid: string, alt: string) => string;
		urlBuilder?: AttachmentUrlBuilder;
	}
): string {
	// Save/restore rather than clear-to-null so a nested render (a resolver
	// or missing hook that itself renders markdown) can't strand the outer
	// call without its context mid-parse. Synchronous by contract: marked is
	// not configured async here, and this wrapper must stay sync — an async
	// marked would outlive the finally.
	const prev = currentAttachmentCtx;
	currentAttachmentCtx = {
		resolver: ctx.resolver,
		workspaceSlug: ctx.workspaceSlug,
		imageVariant: ctx.imageVariant ?? 'thumb-md',
		missing: ctx.missing,
		urlBuilder: ctx.urlBuilder
	};
	try {
		return marked(content) as string;
	} finally {
		currentAttachmentCtx = prev;
	}
}

/**
 * Render a STANDALONE markdown document through the shared `marked` pipeline,
 * sanitized (IDEA-2712 / GitHub #1169). The attachment-preview entry point.
 *
 * A third thin wrapper in FRONT of the one pipeline, for the same reason
 * {@link renderMarkedWithAttachments} is the second: a surface with different
 * needs gets a wrapper, never a second renderer. Two markdown pipelines
 * disagreeing about one format is the failure this shape exists to prevent.
 *
 * WHAT IT DELIBERATELY OMITS, and why the omissions are the point:
 *
 *  - **No wiki-link resolution.** An attached `.md` was authored somewhere
 *    else, so its `[[brackets]]` are not references into whatever workspace
 *    happens to be showing it. Resolving them would silently retarget a
 *    foreign document's links at local items — the same hazard BUG-2830
 *    closed from the rename direction, entered through the front door. They
 *    stay inert text, the posture the public share route already takes.
 *  - **No attachment context.** A `pad-attachment:` href inside an attached
 *    document is not ours to resolve either. The context is CLEARED for the
 *    duration rather than merely left alone (codex R3): this module's context
 *    is module-level state, so a call nested inside another render — an
 *    attachment resolver or a `missing` hook that itself renders markdown —
 *    would otherwise inherit the OUTER document's workspace and resolver, and
 *    quietly resolve a foreign file's references against them. Save, clear,
 *    restore, mirroring {@link renderMarkedWithAttachments}'s save/restore for
 *    the same reason it does it.
 *
 *    With no context installed, a `pad-attachment:` image href falls through to
 *    marked's default `<img src="pad-attachment:…">`, which `sanitizeMarkdownHtml`
 *    then DROPS — DOMPurify's URL policy does not know that scheme. So the
 *    reference disappears rather than rendering as a broken-image icon; either
 *    way nothing resolves, but the earlier comment here said "broken image" and
 *    that was a guess about the sanitizer's behaviour, not a reading of it.
 *
 * Sanitization is INHERITED, not re-derived: the same `sanitizeMarkdownHtml`
 * pass every other `{@html}` source in the app goes through, so the tag and
 * attribute allowlist for an attached document is by construction the one
 * that governs item content.
 */
export function renderMarkdownDocument(content: string): string {
	// Save/restore rather than assume none is installed — see the doc comment.
	// Synchronous by contract, like the sibling wrapper: `marked` is not
	// configured async here, so the `finally` cannot be outlived.
	const prev = currentAttachmentCtx;
	currentAttachmentCtx = null;
	try {
		return sanitizeMarkdownHtml(marked(content) as string);
	} finally {
		currentAttachmentCtx = prev;
	}
}

export function wordCount(content: string): number {
	return content.trim().split(/\s+/).filter(w => w.length > 0).length;
}

export function relativeTime(dateStr: string): string {
	const date = new Date(dateStr);
	const now = new Date();
	const diffMs = now.getTime() - date.getTime();
	const diffMins = Math.floor(diffMs / 60000);
	const diffHours = Math.floor(diffMs / 3600000);
	const diffDays = Math.floor(diffMs / 86400000);

	if (diffMins < 1) return 'just now';
	if (diffMins < 60) return `${diffMins}m ago`;
	if (diffHours < 24) return `${diffHours}h ago`;
	if (diffDays < 7) return `${diffDays}d ago`;
	return date.toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' });
}

/**
 * Fix markdown output from tiptap-markdown which escapes [[ ]] as \[\[ \]\].
 * Must be called on getMarkdown() output before saving.
 */
/**
 * Wrap text as a markdown blockquote — the same thing a person typing `> `
 * in front of each line would produce, and deliberately nothing more
 * (IDEA-2843 / GitHub #1228).
 *
 * Every line is prefixed, blank lines included: an unprefixed blank line ENDS
 * a blockquote in markdown, so quoting a two-paragraph selection without it
 * would silently drop the second paragraph out of the quote and leave it
 * looking like the commenter's own words. That is the whole reason this is a
 * function rather than a template literal at the call site.
 *
 * Trailing whitespace on the prefix of an empty line is trimmed, so the
 * result is what a linter-clean hand-typed quote looks like.
 */
export function toBlockquote(text: string): string {
	return text
		.replace(/\r\n?/g, '\n')
		.split('\n')
		.map((line) => (line.length > 0 ? `> ${line}` : '>'))
		.join('\n');
}

export function unescapeDocLinks(markdown: string): string {
	return markdown.replace(/\\\[\\\[([^\]]+)\\\]\\\]/g, '[[$1]]');
}

// Wiki-link reference pattern: uppercase/alphanumeric prefix, hyphen, digits.
// Matches the item ref format produced by formatItemRef (e.g. TASK-5, BUG-585).
// Anchored so it rejects anything else and falls back to title-based lookup,
// which keeps legacy [[Title]] links working unchanged.
const REF_PATTERN = /^[A-Za-z][A-Za-z0-9]*-\d+$/;

// Workspace slug pattern. Mirrors store.slugify exactly — lowercase
// alphanumerics + hyphens, with NO leading-letter constraint (slugify
// preserves digit-leading inputs, e.g. "2026 Roadmap" → "2026-roadmap").
// The earlier `^[a-z]` constraint was a frontend-only tightening that
// caused `[[2026-roadmap::TASK-1]]` to fall through as a legacy title
// link (Codex round-4). Anchored so other non-slug shapes (uppercase,
// punctuation) still fall through to legacy title handling.
const WORKSPACE_SLUG_PATTERN = /^[a-z0-9][a-z0-9-]*$/;

// Cross-workspace wiki-link body parser. Returns null when the body isn't a
// `workspace::REF` (optionally `|Display`) shape, so callers can fall through
// to legacy [[Title]] semantics without losing pre-existing links whose titles
// happen to contain `::`. Both the slug and the ref must validate — partial
// matches (e.g. `[[claude::not-a-ref]]`) return null.
function parseCrossWorkspaceBody(body: string): { workspace: string; ref: string; display: string | null } | null {
	const sepIdx = body.indexOf('::');
	if (sepIdx <= 0) return null;
	const workspace = body.slice(0, sepIdx);
	const rest = body.slice(sepIdx + 2);
	if (!WORKSPACE_SLUG_PATTERN.test(workspace)) return null;
	// Display override splits on the first unescaped pipe in `rest`, same as
	// the single-workspace splitter. We reuse splitWikiBody for consistency.
	const { key: refRaw, displayOverride: displayRaw } = splitWikiBody(rest);
	const ref = unescapeWikiBody(refRaw).trim();
	if (!REF_PATTERN.test(ref)) return null;
	const display = displayRaw == null ? null : unescapeWikiBody(displayRaw);
	return { workspace, ref, display };
}

// Locate an item by its PREFIX-NUMBER ref in the in-memory list. Returns
// undefined if no match — same fall-through semantics as the legacy path.
function findItemByRef(items: Item[], ref: string): Item | undefined {
	return items.find(i =>
		!!i.item_number && !!i.collection_prefix &&
		`${i.collection_prefix}-${i.item_number}`.toLowerCase() === ref.toLowerCase()
	);
}

/**
 * The TEXT a wiki-link body should read as where no link may be emitted
 * (TASK-2248: the public share page, whose viewers cannot follow an internal
 * link). Same grammar as the renderers above, resolved in the same order:
 *   - `[[ws::REF|Display]]` → Display; `[[ws::REF]]` → `ws::REF`;
 *   - `[[…|Display]]` → Display;
 *   - `[[REF]]` → the title `titleByRef` holds for it (case-insensitive ref),
 *     else the ref itself;
 *   - `[[Title]]` → Title. `[[collection/Title]]` is left whole: with no
 *     title lookup here, stripping the qualifier would also cut a real title
 *     containing a slash ("A/B testing").
 * Never a URL, and never a hint about what exists beyond `titleByRef`.
 */
export function wikiDisplayText(body: string, titleByRef: ReadonlyMap<string, string>): string {
	const xw = parseCrossWorkspaceBody(body);
	if (xw) return xw.display ?? `${xw.workspace}::${xw.ref}`;
	const { key: rawKey, displayOverride: rawDisplay } = splitWikiBody(body);
	if (rawDisplay != null) return unescapeWikiBody(rawDisplay);
	const key = unescapeWikiBody(rawKey).trim();
	if (REF_PATTERN.test(key)) return titleByRef.get(key.toUpperCase()) ?? key;
	return key;
}

/**
 * Resolve a same-workspace wiki-link body against the in-memory items list.
 * Callers handle the cross-workspace `[[ws::REF]]` form first; this covers
 * everything else. Shared by renderMarkdown (comments, previews) and
 * wikiLinksToMarkdown (the Tiptap editor path) so the two renderers cannot
 * drift on resolution order again (BUG-1744 — renderMarkdown lacked the
 * ref-based lookup, so [[REF]] links in comments rendered as broken).
 *
 * Resolution order — ref storage is canonical, so it must win over any
 * legacy title that happens to match the ref literal (otherwise [[BUG-585]]
 * could silently retarget onto a user-created item titled "BUG-585"):
 *   1. [[REF-123]] / [[REF-123|Display]] — ref lookup. An unresolved
 *      ref-shaped body falls through, because something like [[ISO-9001]]
 *      may legitimately be a pre-existing title link.
 *   2. Exact full-body title match, BEFORE the pipe split — handles stored
 *      legacy titles that contain a literal `|` (e.g. "[[A|B]]" where the
 *      item's real title is "A|B"), including the collection-qualified form.
 *   3. Exact title match on the pipe-split key (case-insensitive).
 *   4. The [[collection/Title]] disambiguation syntax.
 *
 * `item` is null when nothing resolves (or the match lacks a collection
 * slug); `displayText` is what the caller should render either way.
 */
function resolveWikiBody(body: string, items: Item[]): { item: Item | null; displayText: string; followsTitle: boolean } {
	// Split optional display override on the FIRST unescaped pipe. We do
	// this up-front so REF_PATTERN can check the key alone (a ref like
	// "BUG-585" contains no pipe, so this is a no-op for ref storage).
	const { key: rawKey, displayOverride: rawDisplay } = splitWikiBody(body);
	const key = unescapeWikiBody(rawKey);
	const displayOverride = rawDisplay == null ? null : unescapeWikiBody(rawDisplay);

	// 1. Ref-based lookup.
	if (REF_PATTERN.test(key.trim())) {
		const byRef = findItemByRef(items, key.trim());
		if (byRef && byRef.collection_slug) {
			return { item: byRef, displayText: displayOverride ?? byRef.title, followsTitle: displayOverride == null };
		}
		// Intentional fall-through to the legacy title lookups below.
	}

	// 2. Full-body legacy titles containing a literal pipe. Only relevant
	//    when the body actually has a pipe — otherwise the already-split
	//    `key` is identical to the full body.
	if (rawDisplay != null) {
		const fullBody = unescapeWikiBody(body);
		const fullTitleItem = items.find(i => i.title.toLowerCase() === fullBody.toLowerCase());
		if (fullTitleItem && fullTitleItem.collection_slug) {
			return { item: fullTitleItem, displayText: fullTitleItem.title, followsTitle: true };
		}
		// Collection-qualified legacy form whose title contains a pipe.
		if (fullBody.includes('/')) {
			const [qualColl, ...qualRest] = fullBody.split('/');
			const qualTitle = qualRest.join('/');
			const qualItem = items.find(i =>
				i.title.toLowerCase() === qualTitle.toLowerCase() &&
				i.collection_slug === qualColl
			);
			if (qualItem && qualItem.collection_slug) {
				return { item: qualItem, displayText: qualItem.title, followsTitle: true };
			}
		}
	}

	// 3. Exact title match on the key.
	const titleLower = key.toLowerCase();
	let item = items.find(i => i.title.toLowerCase() === titleLower);
	let displayText = displayOverride ?? key;

	// 4. The [[collection/Title]] disambiguation syntax.
	if (!item && key.includes('/')) {
		const [collFilter, ...rest] = key.split('/');
		const searchTitle = rest.join('/');
		const found = items.find(i =>
			i.title.toLowerCase() === searchTitle.toLowerCase() &&
			i.collection_slug === collFilter
		);
		if (found) {
			item = found;
			if (displayOverride == null) displayText = searchTitle;
		}
	}

	return { item: item && item.collection_slug ? item : null, displayText, followsTitle: displayOverride == null };
}

/**
 * Convert wiki-link storage syntax into markdown links for Tiptap rendering.
 * Supports three forms, in preference order:
 *   - [[REF-123]]              → ref lookup; visible text = current item title
 *   - [[REF-123|Display Text]] → ref lookup; visible text = Display Text
 *   - [[Title]]                → legacy title lookup (also accepts [[coll/Title]])
 * The ref-based forms are safe for titles containing any characters (brackets,
 * slashes, quotes, etc.) because the stored key is the opaque item ref.
 */
export function wikiLinksToMarkdown(content: string, items: Item[], workspaceSlug: string, username?: string): string {
	// Body may contain backslash-escaped chars (`\]`, `\\`, `\|`) so the tokens
	// we emit can carry arbitrary display text. Shares WIKI_LINK_PATTERN with
	// renderMarkdown — see its definition for why the escape alternative is
	// spelled `\\[^\n]` rather than `\\.` (BUG-2834).
	return content.replace(WIKI_LINK_PATTERN, (_match, body: string) => {
		const prefix = username ? `/${username}/${workspaceSlug}` : `/${workspaceSlug}`;

		// Cross-workspace form: [[workspace::REF]] / [[workspace::REF|Display]].
		// If the prefix matches the current workspace, strip it and fall through
		// to same-workspace ref handling so the link resolves to the canonical
		// item URL. Otherwise emit a link to the resolver route — the rendered
		// editor lacks the target workspace's items, so client-side validation
		// is impossible and we defer to the server's 302/404.
		const xw = parseCrossWorkspaceBody(body);
		if (xw) {
			if (xw.workspace === workspaceSlug) {
				const sameWsItem = findItemByRef(items, xw.ref);
				if (sameWsItem && sameWsItem.collection_slug) {
					const text = xw.display ?? sameWsItem.title;
					return `[${escapeMarkdownLinkText(text)}](${prefix}/${sameWsItem.collection_slug}/${itemUrlId(sameWsItem)}${followsTitleMarker(xw.display == null, text, sameWsItem)})`;
				}
				// Ref didn't resolve in the current workspace — leave the
				// original wiki-link verbatim, matching the legacy fall-through
				// behavior at the bottom of this function.
				return _match;
			}
			// Cross-workspace: emit the resolver URL (`/-/r/{ws}/{ref}`).
			// Same shape renderMarkdown emits — Codex round-2 P1.4 / Option B.
			const display = xw.display ?? `${xw.workspace}::${xw.ref}`;
			return `[${escapeMarkdownLinkText(display)}](/-/r/${xw.workspace}/${xw.ref})`;
		}

		// Same-workspace resolution — shared with renderMarkdown via
		// resolveWikiBody (see its doc comment for the resolution order).
		const { item, displayText, followsTitle } = resolveWikiBody(body, items);
		if (item && item.collection_slug) {
			return `[${escapeMarkdownLinkText(displayText)}](${prefix}/${item.collection_slug}/${itemUrlId(item)}${followsTitleMarker(followsTitle, displayText, item)})`;
		}
		// Unresolved: leave the original [[X]] text alone. Emitting a
		// [text](broken) link here would hijack content that legitimately
		// contains `[[` — for example a `[[` that appears inside another
		// markdown link's text span. The regex is greedy and may grab a
		// range that was never intended as a wiki-link, so the safe thing
		// on miss is to restore the match verbatim.
		return _match;
	});
}

/**
 * Convert markdown links back to wiki-link storage syntax.
 * When the link's URL resolves to an item with a ref, emit [[REF]] (or
 * [[REF|Display]] if the visible text differs from the item's current
 * title). Ref-based storage is preferred because it survives item renames
 * and is robust against special characters in titles.
 * Items without a ref fall back to the legacy [[Title]] form.
 */
export function markdownToWikiLinks(markdown: string, items: Item[]): string {
	// Cross-workspace ref URLs: /-/r/{workspace}/{REF} → [[workspace::REF]].
	// Run BEFORE the same-workspace match below because the regex below
	// accepts two-or-three-segment URL paths and would otherwise misclassify
	// the resolver URL. The `/-/r/` sentinel prefix can't appear in any
	// user-namespace URL because username + collection slugs are letter-led
	// (Codex round-2 P1.4 / Option B).
	//
	// Strip the optional `|Display` ONLY when the link text equals the
	// renderer's DEFAULT (`${ws}::${ref}`). A user who explicitly wrote
	// `[[other::TASK-1|TASK-1]]` must round-trip back to itself — comparing
	// against the bare ref (`displayText === ref`) drops the override, after
	// which the next render would emit the default `other::TASK-1` and
	// silently change the visible link text (Codex round-1 P2.1).
	const withXwRefs = markdown.replace(
		/\[((?:\\.|[^\]\\])+)\]\(\/-\/r\/([a-z0-9][a-z0-9-]*)\/([A-Za-z][A-Za-z0-9]*-\d+)\)/g,
		(_match, rawText: string, ws: string, ref: string) => {
			const displayText = unescapeMarkdownLinkText(rawText);
			const renderDefault = `${ws}::${ref}`;
			if (displayText === renderDefault) {
				return `[[${ws}::${ref}]]`;
			}
			return `[[${ws}::${ref}|${escapeWikiBody(displayText)}]]`;
		}
	);

	// Match [Title](/username/workspace/collection/slug-or-REF). Title may
	// contain backslash-escaped chars (\[, \], \\) that tiptap-markdown emits
	// when serializing link text. The capture allows `\.` sequences so we
	// don't terminate on an escaped `]` that's really part of the display.
	return withXwRefs.replace(/\[((?:\\.|[^\]\\])+)\]\((\/(?:[^/]+\/){2,3}([^)]+?))(?: "((?:\\"|[^"])*)")?\)/g, (_match, rawText: string, path: string, slugOrRef: string, rawMarker: string | undefined) => {
		const item = items.find(i => {
			if (i.slug === slugOrRef) return true;
			if (i.item_number && i.collection_prefix) {
				return `${i.collection_prefix}-${i.item_number}` === slugOrRef;
			}
			return false;
		});
		// The follows-title marker is load-time state and never belongs in stored
		// content, so a link whose target is gone keeps its text and href only.
		// A title that is not ours is the user's: the link is left exactly as it
		// was, which is what this function did with any titled link before the
		// marker existed.
		if (rawMarker !== undefined && !rawMarker.startsWith(FOLLOWS_TITLE_PREFIX)) return _match;
		if (!item) return rawMarker === undefined ? _match : `[${rawText}](${path})`;
		const marker =
			rawMarker === undefined ? undefined : rawMarker.slice(FOLLOWS_TITLE_PREFIX.length).replace(/\\"/g, '"');

		// tiptap-markdown emits backslash-escaped brackets in the link text
		// (e.g. "Use \[\[ to link"); unescape before comparing/emitting.
		const displayText = unescapeMarkdownLinkText(rawText);

		const ref = (item.item_number && item.collection_prefix)
			? `${item.collection_prefix}-${item.item_number}`
			: null;

		if (ref) {
			// Prefer ref-based storage. Omit |Display if it matches the
			// current item title (renaming the item updates the link text
			// automatically on next load).
			// Text still equal to the follows-title marker is a link nobody edited
			// that loaded under an earlier title: it follows the title (BUG-3315).
			if (displayText === item.title || followsMarker(displayText, marker)) {
				return `[[${ref}]]`;
			}
			return `[[${ref}|${escapeWikiBody(displayText)}]]`;
		}
		// Legacy fallback for items without a ref.
		return `[[${escapeWikiBody(followsMarker(displayText, marker) ? item.title : displayText)}]]`;
	});
}

// The load-time marker for a link that FOLLOWS its target's title (BUG-3315),
// carried in the link mark's existing `title` attribute:
// `[Title](href "pad-follows-title:Title")`. The prefix is what tells it from a
// title a user wrote, which is never read, stripped or hidden (codex r2).
// It records the text as loaded, and only for a link with no explicit override
// whose text IS the title. The save then tells "nobody edited this, it was the
// title then" (follows the title, so `[[REF]]`) from a deliberate or edited
// display. It never reaches stored content: markdownToWikiLinks consumes it, and
// SafeLink keeps it out of the DOM so it is not shown as a stale tooltip.
// Inside a CommonMark link title, `\`, `"` and `&` would be read as an escape,
// the end of the title and an entity, so each is backslash-escaped.
function followsTitleMarker(followsTitle: boolean, text: string, item: Item): string {
	if (!followsTitle || text !== item.title) return '';
	return ` "${FOLLOWS_TITLE_PREFIX}${text.replace(/[\\"&]/g, '\\$&')}"`;
}

// Whether a link's text is still the text its follows-title marker recorded.
// After a trip through the editor the marker comes back as the serializer
// writes a title (only `"` escaped), so it is compared RAW. Straight from
// wikiLinksToMarkdown it still carries the load's `\\`, `\"` and `\&`
// escapes, so it is also compared with backslash escapes undone (codex r3).
function followsMarker(text: string, marker: string | undefined): boolean {
	if (marker === undefined) return false;
	return text === marker || text === marker.replace(/\\([\\"&])/g, '$1');
}

// Escape a link's display text so the editor's markdown parser reads it as
// LITERAL text. Brackets would terminate or unbalance the span, and the rest are
// the characters its inline rules act on (BUG-3315): `*` `_` and `~` become
// emphasis or strikethrough, `` ` `` code, `<` an HTML tag (html is enabled, so
// `<topic>` was dropped), and `&` an entity (`&gt;` was decoded). Every one of
// them is ASCII punctuation, which CommonMark lets a backslash escape anywhere.
// A single pass, so an escape's own backslash is never escaped again.
function escapeMarkdownLinkText(s: string): string {
	return s.replace(/[\\[\]*_~`<>&]/g, '\\$&');
}

// Escape the characters that would terminate a [[...]] wiki-link body, or
// collide with the `|` display separator. Order matters: backslash first.
function escapeWikiBody(s: string): string {
	return s.replace(/\\/g, '\\\\').replace(/([\]|])/g, '\\$1');
}

// Inverse of escapeWikiBody. Accepts `\]`, `\|`, and `\\` escapes.
function unescapeWikiBody(s: string): string {
	return s.replace(/\\(\\|\]|\|)/g, '$1');
}

// Split a wiki-link body on the FIRST unescaped `|`. Returns the raw key
// and the raw display override (both still escape-encoded — caller should
// unescape them). If there's no pipe, displayOverride is null.
function splitWikiBody(body: string): { key: string; displayOverride: string | null } {
	let i = 0;
	while (i < body.length) {
		const ch = body[i];
		if (ch === '\\' && i + 1 < body.length) {
			i += 2;
			continue;
		}
		if (ch === '|') {
			return { key: body.slice(0, i), displayOverride: body.slice(i + 1) };
		}
		i++;
	}
	return { key: body, displayOverride: null };
}

// Recover a link's literal display text from the SERIALIZER's output. That is not
// the inverse of escapeMarkdownLinkText, because the serializer escapes its own
// way. It backslash-escapes ASCII punctuation (including the \[\[ / \]\] it
// inserts so its output never looks like our wiki-link sentinels), and writes
// `<`, `>` and `&` as entities. Undoing only the bracket escapes, as this used to,
// left `&gt;` and `\*` in the text, so a title containing them never compared
// equal to itself, and the first save of any item linking one pinned the mangled
// text as an override (BUG-3315). A single pass, so `\&gt;` (a literal `&gt;`)
// is not decoded twice.
const LINK_TEXT_ENTITIES: Record<string, string> = { lt: '<', gt: '>', amp: '&', quot: '"', '#39': "'" };
function unescapeMarkdownLinkText(s: string): string {
	return s.replace(/\\([!-/:-@[-`{-~])|&(lt|gt|amp|quot|#39);/g, (_m, punct: string | undefined, ent: string | undefined) =>
		punct ?? LINK_TEXT_ENTITIES[ent as string]
	);
}

/**
 * Convert [[broken]] placeholder links back to wiki syntax.
 *
 * It is the LAST step of every save pipeline (both ItemDetail save paths and
 * the materializer's flushPipeline), which skip markdownToWikiLinks when the
 * link index is empty. So it is also where a follows-title marker (BUG-3315,
 * followsTitleMarker) that nothing consumed is dropped from same-origin links:
 * the marker is load-time state and must never reach stored content.
 */
export function cleanBrokenLinks(markdown: string): string {
	return markdown
		.replace(/\[([^\]]+)\]\(broken\)/g, '[[$1]]')
		.replace(/(\[(?:\\.|[^\]\\])+\]\(\/[^)]*?) "pad-follows-title:(?:\\"|[^"])*"\)/g, '$1)');
}

export function parseTags(tagsJson: string): string[] {
	try {
		const parsed = JSON.parse(tagsJson);
		return Array.isArray(parsed) ? parsed : [];
	} catch {
		return [];
	}
}
