import type { SharePayload } from '$lib/types';

/**
 * The most pages one shared-collection load follows (BUG-3792). At the
 * server's 1000-item page that is 50,000 items; past it the load fails rather
 * than rendering a collection with items silently missing.
 */
export const MAX_SHARE_PAGES = 50;

/** A shared collection's page: SharePayload plus its place in the list. */
export type SharePage = SharePayload & { has_more?: boolean; next_offset?: number };

/**
 * Load every page of a shared collection and merge them (BUG-3792). The
 * server bounds each page and says `has_more` with the `next_offset` to ask
 * for; items arrive in the owner's order, so pages are appended. Attachment
 * refs are minted per page and merged. A single-item share, a refusal
 * (require_auth / require_password) or an older server without `has_more` is
 * one page.
 */
export async function walkSharePages(
	fetchPage: (offset: number) => Promise<SharePage>,
	maxPages = MAX_SHARE_PAGES
): Promise<SharePayload> {
	const first = await fetchPage(0);
	if (first.type !== 'collection' || !first.has_more) return first;
	const items = [...(first.items ?? [])];
	const refs = { ...(first.attachment_refs ?? {}) };
	let page = first;
	for (let n = 1; page.has_more; n++) {
		const next = page.next_offset;
		if (n >= maxPages || typeof next !== 'number' || next <= 0) {
			throw new Error(`shared collection did not finish within ${maxPages} pages`);
		}
		page = await fetchPage(next);
		items.push(...(page.items ?? []));
		Object.assign(refs, page.attachment_refs ?? {});
	}
	const merged: SharePage = { ...first, items, has_more: false };
	delete merged.next_offset;
	if (Object.keys(refs).length > 0) merged.attachment_refs = refs;
	return merged;
}
