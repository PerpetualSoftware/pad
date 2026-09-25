// BUG-3046 — a SOURCE guard that every open-children confirmation is asked
// with a liveness predicate. The store behaviour is tested for real in
// openChildrenDialogLiveness.svelte.test.ts; what that cannot say is that the
// callers PASS one (CONVE-19). ItemDetail and the collection page are not
// renderable in a unit test, the same limit itemDetailOrdersFieldWrites.test.ts
// states.
//
// WHAT IT CANNOT DO: it counts arguments, not their meaning. A predicate that
// always answers true satisfies it. It catches the thing that goes wrong over
// time: a new call site that omits the predicate.
import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

const CALLERS = [
	'../components/items/ItemDetail.svelte',
	'../../routes/[username]/[workspace]/[collection]/+page.svelte',
];

/** Top-level argument count of each `name(` call, by a balanced-bracket scan. */
function argCounts(src: string, name: string): number[] {
	const out: number[] = [];
	for (let at = src.indexOf(name + '('); at >= 0; at = src.indexOf(name + '(', at + 1)) {
		let depth = 0;
		let args = 1;
		let i = at + name.length;
		for (; i < src.length; i++) {
			const c = src[i];
			if (c === '(' || c === '[' || c === '{') depth++;
			else if (c === ')' || c === ']' || c === '}') {
				depth--;
				if (depth === 0) break;
			} else if (c === ',' && depth === 1) args++;
		}
		out.push(args);
	}
	return out;
}

describe('every open-children prompt carries a liveness predicate (BUG-3046)', () => {
	for (const rel of CALLERS) {
		it(rel.split('/').slice(-2).join('/'), () => {
			const src = readFileSync(resolve(__dirname, rel), 'utf8');
			const counts = argCounts(src, 'confirmOpenChildrenOrThrow');
			expect(counts.length, 'the file still calls it (re-anchor this guard if not)').toBeGreaterThan(0);
			expect(counts, 'err, parentRef, retry, isLive').toEqual(counts.map(() => 4));
		});
	}
});
