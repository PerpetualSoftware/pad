import { marked } from 'marked';
import type { Item } from '$lib/types';
import { itemUrlId } from '$lib/types';

export function renderMarkdown(content: string, items: Item[], workspaceSlug: string): string {
	const withLinks = content.replace(/\[\[([^\]]+)\]\]/g, (_match, title: string) => {
		const item = items.find(i => i.title === title);
		if (item && item.collection_slug) {
			return `<a href="/${workspaceSlug}/${item.collection_slug}/${itemUrlId(item)}" class="doc-link">${title}</a>`;
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

/**
 * Convert [[Item Title]] to markdown links for Tiptap rendering.
 * Tiptap doesn't understand [[]] syntax, so we convert to standard
 * markdown links before feeding content to the editor.
 */
export function wikiLinksToMarkdown(content: string, items: Item[], workspaceSlug: string): string {
	return content.replace(/\[\[([^\]]+)\]\]/g, (_match, title: string) => {
		// Support optional collection/ prefix: [[tasks/My Task]]
		let searchTitle = title;
		let collFilter: string | null = null;
		if (title.includes('/')) {
			const [coll, ...rest] = title.split('/');
			collFilter = coll;
			searchTitle = rest.join('/');
		}

		const item = items.find(i => {
			const titleMatch = i.title.toLowerCase() === searchTitle.toLowerCase();
			if (collFilter && i.collection_slug) {
				return titleMatch && i.collection_slug === collFilter;
			}
			return titleMatch;
		});

		if (item && item.collection_slug) {
			return `[${searchTitle}](/${workspaceSlug}/${item.collection_slug}/${itemUrlId(item)})`;
		}
		// Unresolved — render as styled text (editor will show it as plain text)
		return `[${searchTitle}](broken)`;
	});
}

/**
 * Convert markdown links back to [[Item Title]] syntax for storage.
 * Reverses wikiLinksToMarkdown() so we store [[]] not []() in the database.
 */
export function markdownToWikiLinks(markdown: string, items: Item[]): string {
	// Match [Title](/workspace/collection/slug-or-REF) pattern
	return markdown.replace(/\[([^\]]+)\]\(\/[^/]+\/[^/]+\/([^)]+)\)/g, (_match, title: string, slugOrRef: string) => {
		const item = items.find(i => {
			if (i.slug === slugOrRef) return true;
			// Also match PREFIX-NUMBER refs
			if (i.item_number && i.collection_prefix) {
				return `${i.collection_prefix}-${i.item_number}` === slugOrRef;
			}
			return false;
		});
		if (item) {
			return `[[${title}]]`;
		}
		return _match;
	});
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
