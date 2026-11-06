// TASK-4230: a web call that addresses an ITEM must pass its id (or a ref the
// server handed back), never `.slug`. A slug shaped like a ref (`release-2`)
// overlaps the ref namespace, and the server resolves an identifier as UUID,
// then current ref, then exact slug (internal/store/items.go ResolveItem), so
// an id is the one spelling that cannot land on another item.
// This scan covers the call shapes where the argument is written inline;
// a slug held in a variable is out of its reach.
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
const ITEM_CALL = /api\.(?:items\.(?:get|update|delete|progress|copy|copyPreflight|children|push|patch)|links\.list)\(\s*[^,()]+,\s*[A-Za-z_.]+\.slug\b/g;

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
	});
});
