import { marked, Renderer, type Tokens } from 'marked';
import DOMPurify from 'dompurify';
import type { Item } from '$lib/types';
import { itemUrlId } from '$lib/types';

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
renderer.link = function (this: Renderer, { href, title, tokens }: Tokens.Link) {
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
	'start'
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
 */
export function renderMarkdown(
	content: string,
	items: Item[],
	workspaceSlug: string,
	username?: string,
	visibleCollectionSlugs?: Set<string>
): string {
	const withLinks = content.replace(/\[\[([^\]]+)\]\]/g, (_match, title: string) => {
		// Escape user-controlled title so it can't break out of attribute
		// quotes. sanitizeMarkdownHtml would strip the worst offenders after
		// the fact, but escaping up-front keeps the intermediate HTML valid
		// and avoids relying on the sanitizer to paper over bad markup.
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
	return sanitizeMarkdownHtml(marked(withLinks) as string);
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
	// Match [Title](/username/workspace/collection/slug-or-REF). Title may
	// contain backslash-escaped chars (\[, \], \\) that tiptap-markdown emits
	// when serializing link text. The capture allows `\.` sequences so we
	// don't terminate on an escaped `]` that's really part of the display.
	return markdown.replace(/\[((?:\\.|[^\]\\])+)\]\(\/(?:[^/]+\/){2,3}([^)]+)\)/g, (_match, rawText: string, slugOrRef: string) => {
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
