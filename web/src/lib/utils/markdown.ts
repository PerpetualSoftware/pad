import { marked, Renderer, type Tokens } from 'marked';
import DOMPurify from 'dompurify';
import type { Item } from '$lib/types';
import { itemUrlId } from '$lib/types';
import {
	type AttachmentResolver,
	isAttachmentHref,
	resolveAttachmentImage,
	resolveAttachmentLink
} from '$lib/markdown/attachments';

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
			currentAttachmentCtx.resolver
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
			currentAttachmentCtx.resolver
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
 */
export function renderMarkdown(
	content: string,
	items: Item[],
	workspaceSlug: string,
	username?: string,
	visibleCollectionSlugs?: Set<string>,
	attachmentResolver?: AttachmentResolver
): string {
	const withLinks = content.replace(/\[\[([^\]]+)\]\]/g, (_match, body: string) => {
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
		// Escape user-controlled title so it can't break out of attribute
		// quotes. sanitizeMarkdownHtml would strip the worst offenders after
		// the fact, but escaping up-front keeps the intermediate HTML valid
		// and avoids relying on the sanitizer to paper over bad markup.
		const title = body;
		const safeTitle = escapeHtml(title);
		const item = items.find(i => i.title === title);
		if (item && item.collection_slug) {
			// Check visibility: if a visibility set is provided, check it
			if (visibleCollectionSlugs !== undefined && !visibleCollectionSlugs.has(item.collection_slug)) {
				return `<span class="doc-link locked" title="You don't have access to this item">🔒 ${safeTitle}</span>`;
			}
			const prefix = username ? `/${username}/${workspaceSlug}` : `/${workspaceSlug}`;
			return `<a href="${prefix}/${item.collection_slug}/${itemUrlId(item)}" class="doc-link">${safeTitle}</a>`;
		}
		return `<span class="doc-link broken">${safeTitle}</span>`;
	});
	// Wire the attachment resolver into the global renderer for the duration
	// of this synchronous parse. The try/finally ensures we never leak the
	// context across calls, even when marked throws on malformed input.
	if (attachmentResolver) {
		currentAttachmentCtx = { resolver: attachmentResolver, workspaceSlug };
	}
	try {
		return sanitizeMarkdownHtml(marked(withLinks) as string);
	} finally {
		currentAttachmentCtx = null;
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
	// we emit can carry arbitrary display text. The capture is (\\.|[^\]\\])+,
	// i.e. "a backslash-escaped char OR any non-`]`/non-`\` char".
	return content.replace(/\[\[((?:\\.|[^\]\\])+)\]\]/g, (_match, body: string) => {
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
					return `[${escapeMarkdownLinkText(text)}](${prefix}/${sameWsItem.collection_slug}/${itemUrlId(sameWsItem)})`;
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

		// Split optional display override on the FIRST unescaped pipe. We do
		// this up-front so REF_PATTERN can check the key alone (a ref like
		// "BUG-585" contains no pipe, so this is a no-op for ref storage).
		const { key: rawKey, displayOverride: rawDisplay } = splitWikiBody(body);
		const key = unescapeWikiBody(rawKey);
		const displayOverride = rawDisplay == null ? null : unescapeWikiBody(rawDisplay);

		// 1. Ref-based lookup FIRST. Ref storage is our canonical form, so
		//    it must win over any legacy title that happens to match the
		//    ref literal — otherwise `[[BUG-585]]` could silently retarget
		//    onto a user-created item whose title is "BUG-585". If the ref
		//    doesn't resolve we FALL THROUGH to the legacy title path,
		//    because a ref-shaped body like `[[ISO-9001]]` may legitimately
		//    be a pre-existing title link.
		if (REF_PATTERN.test(key.trim())) {
			const ref = key.trim();
			const byRef = items.find(i =>
				!!i.item_number && !!i.collection_prefix &&
				`${i.collection_prefix}-${i.item_number}`.toLowerCase() === ref.toLowerCase()
			);
			if (byRef && byRef.collection_slug) {
				const text = displayOverride ?? byRef.title;
				return `[${escapeMarkdownLinkText(text)}](${prefix}/${byRef.collection_slug}/${itemUrlId(byRef)})`;
			}
			// Intentional fall-through to the legacy title lookups below.
		}

		// 2. Legacy: exact full-body title match, BEFORE the pipe split.
		//    Handles pre-existing stored titles that contain a literal `|`
		//    (e.g. "[[A|B]]" where the item's real title is "A|B"). Only
		//    relevant when the body actually has a pipe — otherwise the
		//    already-split `key` is identical to the full body.
		if (rawDisplay != null) {
			const fullBody = unescapeWikiBody(body);
			const fullTitleItem = items.find(i => i.title.toLowerCase() === fullBody.toLowerCase());
			if (fullTitleItem && fullTitleItem.collection_slug) {
				return `[${escapeMarkdownLinkText(fullTitleItem.title)}](${prefix}/${fullTitleItem.collection_slug}/${itemUrlId(fullTitleItem)})`;
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
					return `[${escapeMarkdownLinkText(qualItem.title)}](${prefix}/${qualItem.collection_slug}/${itemUrlId(qualItem)})`;
				}
			}
		}

		// 3. Legacy: exact title match on the key.
		const titleLower = key.toLowerCase();
		let item = items.find(i => i.title.toLowerCase() === titleLower);
		let displayText = displayOverride ?? key;

		// 4. Legacy: the [[collection/Title]] disambiguation syntax.
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

		if (item && item.collection_slug) {
			return `[${escapeMarkdownLinkText(displayText)}](${prefix}/${item.collection_slug}/${itemUrlId(item)})`;
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
	return withXwRefs.replace(/\[((?:\\.|[^\]\\])+)\]\(\/(?:[^/]+\/){2,3}([^)]+)\)/g, (_match, rawText: string, slugOrRef: string) => {
		const item = items.find(i => {
			if (i.slug === slugOrRef) return true;
			if (i.item_number && i.collection_prefix) {
				return `${i.collection_prefix}-${i.item_number}` === slugOrRef;
			}
			return false;
		});
		if (!item) return _match;

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
			if (displayText === item.title) {
				return `[[${ref}]]`;
			}
			return `[[${ref}|${escapeWikiBody(displayText)}]]`;
		}
		// Legacy fallback for items without a ref.
		return `[[${escapeWikiBody(displayText)}]]`;
	});
}

// Escape the characters that would terminate or unbalance a markdown link's
// text span. `\` must be doubled first so it doesn't interfere with the
// subsequent bracket escapes.
function escapeMarkdownLinkText(s: string): string {
	return s.replace(/\\/g, '\\\\').replace(/([\[\]])/g, '\\$1');
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

// Inverse of escapeMarkdownLinkText. Also undoes the \[\[ / \]\] escapes that
// tiptap-markdown inserts to prevent its own output from looking like our
// wiki-link sentinels.
function unescapeMarkdownLinkText(s: string): string {
	return s.replace(/\\(\[|\]|\\)/g, '$1');
}

/**
 * Convert [[broken]] placeholder links back to wiki syntax
 */
export function cleanBrokenLinks(markdown: string): string {
	return markdown.replace(/\[([^\]]+)\]\(broken\)/g, '[[$1]]');
}

export function parseTags(tagsJson: string): string[] {
	try {
		const parsed = JSON.parse(tagsJson);
		return Array.isArray(parsed) ? parsed : [];
	} catch {
		return [];
	}
}
