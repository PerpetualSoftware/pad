import { marked, type Tokens } from 'marked';
import type { Item } from '$lib/types';
import { itemUrlId } from '$lib/types';

// Custom renderer to open external links in new tabs
const renderer = new marked.Renderer();
renderer.link = ({ href, title, tokens }: Tokens.Link) => {
	const text = marked.parseInline(tokens.map(t => t.raw).join(''));
	const isExternal = href && /^https?:\/\//.test(href);
	const titleAttr = title ? ` title="${title}"` : '';
	if (isExternal) {
		return `<a href="${href}"${titleAttr} target="_blank" rel="noopener noreferrer" class="external-link">${text}<span class="external-icon" aria-hidden="true"> ↗</span></a>`;
	}
	return `<a href="${href}"${titleAttr}>${text}</a>`;
};

marked.use({ renderer });

/**
 * Render markdown with wiki-link resolution.
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
		const item = items.find(i => i.title === title);
		if (item && item.collection_slug) {
			// Check visibility: if a visibility set is provided, check it
			if (visibleCollectionSlugs !== undefined && !visibleCollectionSlugs.has(item.collection_slug)) {
				return `<span class="doc-link locked" title="You don't have access to this item">🔒 ${title}</span>`;
			}
			const prefix = username ? `/${username}/${workspaceSlug}` : `/${workspaceSlug}`;
			return `<a href="${prefix}/${item.collection_slug}/${itemUrlId(item)}" class="doc-link">${title}</a>`;
		}
		return `<span class="doc-link broken">${title}</span>`;
	});
	return marked(withLinks) as string;
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

		// Split optional display override on the FIRST unescaped pipe.
		const { key: rawKey, displayOverride: rawDisplay } = splitWikiBody(body);
		const key = unescapeWikiBody(rawKey);
		const displayOverride = rawDisplay == null ? null : unescapeWikiBody(rawDisplay);

		// 1. Ref-based lookup (e.g. [[BUG-585]]) — the preferred form.
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
			// Unresolved ref — keep the original [[…]] so it round-trips.
			return _match;
		}

		// 2. Legacy: exact full-title match. Titles can contain slashes, so
		//    this must win over the collection-filter fallback below.
		const titleLower = key.toLowerCase();
		let item = items.find(i => i.title.toLowerCase() === titleLower);
		let displayText = displayOverride ?? key;

		// 3. Legacy: the [[collection/Title]] disambiguation syntax.
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
