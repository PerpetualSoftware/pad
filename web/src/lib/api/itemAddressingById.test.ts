// TASK-4230: a web call that addresses an ITEM must pass its id (or a ref the
// server handed back), never `.slug`. A slug shaped like a ref (`release-2`)
// overlaps the ref namespace, and the server resolves an identifier as UUID,
// then current ref, then exact slug (internal/store/items.go ResolveItem), so
// an id is the one spelling that cannot land on another item.
// The scan covers an item object's `.slug` handed straight to an item-addressing
// api method, to a helper or store that forwards it, or to a prop that a
// component forwards to one. It cannot see a slug laundered through a local
// variable; those were enumerated by hand (see the TASK-4230 trail).
import { describe, expect, it } from 'vitest';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';

const ROOT = new URL('../../', import.meta.url).pathname;

function sources(dir: string): string[] {
	return readdirSync(dir).flatMap((name) => {
		const p = join(dir, name);
		if (statSync(p).isDirectory()) return sources(p);
		return /\.(svelte|ts)$/.test(name) && !/\.test\.ts$/.test(name) ? [p] : [];
	});
}

// items.* and links.list take the item as their second argument.
const ITEM_CALL = new RegExp(
	[
		// api.items.* / api.links.* taking the item as the second argument.
		String.raw`api\.(?:items\.(?:get|update|delete|progress|copy|copyPreflight|children|push|patch|move|restore|star|unstar|decisions|backlinks|relationBacklinks)|links\.(?:list|create)|comments\.(?:list|create))\(\s*[^,()]+,\s*[A-Za-z_.]+\.slug\b`,
		// Helpers and stores that forward their item argument to one of those.
		String.raw`(?:refreshLinksPreservingOnFailure|starredStore\.toggle)\(\s*[^,()]+,\s*[A-Za-z_.]+\.slug\b`,
		// Props a component forwards to an item-addressing call (ShareDialog's
		// targetSlug is also a collection slug, so only an item's is matched).
		String.raw`(?:itemRef|parentRef|parentSlug)=\{\s*[A-Za-z_.?]+\.slug\b`,
		String.raw`targetSlug=\{\s*item[A-Za-z]*\.slug\b`,
	].join('|'),
	'g'
);

describe('item-addressing web calls pass an id, not a slug (TASK-4230)', () => {
	it('finds none in src', () => {
		const hits: string[] = [];
		for (const file of sources(ROOT)) {
			const text = readFileSync(file, 'utf8');
			for (const m of text.matchAll(ITEM_CALL)) hits.push(`${file.slice(ROOT.length)}: ${m[0]}`);
		}
		expect(hits).toEqual([]);
	});

	it('the pattern does see a slug argument (not vacuous)', () => {
		expect('await api.items.update(ws, item.slug, body)'.match(ITEM_CALL)).not.toBeNull();
		expect('await api.links.list(wsSlug, itemData.slug)'.match(ITEM_CALL)).not.toBeNull();
		expect('await api.items.update(ws, item.id, body)'.match(ITEM_CALL)).toBeNull();
		expect('api.items.move(sourceWs, sourceItem.slug, t)'.match(ITEM_CALL)).not.toBeNull();
		expect('starredStore.toggle(wsSlug, item.slug, item.id)'.match(ITEM_CALL)).not.toBeNull();
		expect('<DecisionChips {wsSlug} itemRef={item.slug} />'.match(ITEM_CALL)).not.toBeNull();
		expect('<DecisionChips {wsSlug} itemRef={itemUrlId(item)} />'.match(ITEM_CALL)).toBeNull();
	});
});
