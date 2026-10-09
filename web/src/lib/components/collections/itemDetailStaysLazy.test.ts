// TASK-2226: a SOURCE guard that the collection page's static import graph
// does not reach ItemDetail (and with it the editor stack) through PaneHost or
// the page itself. Measured on the build it replaced: 1,609 KB in the route's
// static closure, 988 KB of it editor code; 541 KB and 0 KB after.
// WHAT A SOURCE GUARD CANNOT DO: it checks these two files' import lines, not
// the whole graph. A new static importer of ItemDetail elsewhere in the
// route's closure would not trip it; the build's own closure is the full proof.
import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

const files = {
	PaneHost: resolve(__dirname, './PaneHost.svelte'),
	'[collection] page': resolve(__dirname, '../../../routes/[username]/[workspace]/[collection]/+page.svelte')
};
const staticImport = /^\s*import\s+(?!type\b)[^;]*from\s+['"][^'"]*\/(ItemDetail|Editor)\.svelte['"]/m;

describe('ItemDetail stays out of the collection route bundle (TASK-2226)', () => {
	for (const [name, path] of Object.entries(files)) {
		it(`${name} has no static import of ItemDetail or Editor`, () => {
			expect(readFileSync(path, 'utf8')).not.toMatch(staticImport);
		});
	}

	it('CONTROL: the pattern catches the import it guards against', () => {
		expect("\timport ItemDetail from '$lib/components/items/ItemDetail.svelte';").toMatch(staticImport);
		expect("\timport type X from '$lib/components/items/ItemDetail.svelte';").not.toMatch(staticImport);
	});
});
