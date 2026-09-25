import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

/**
 * BUG-3044 — every write the pane's save indicator counts is settled on every
 * path. A `saves.begin()` whose token is not settled in a `finally` leaves the
 * count up after any early return (a superseded write, an item switch, a
 * force-refresh skip), and 'saving' then blocks every SSE gate for the item
 * until navigation.
 *
 * A source guard, deliberately narrow: it pairs each begin with a settle of
 * the same token inside a `finally` block or a `.finally(` in the SAME
 * function. It does not prove the finally is reachable on every path; the
 * identity-fence gate (itemDetailIdentityFenceAst.test.ts) hashes each of
 * these units, so any edit to one is re-read there.
 */
const SOURCE = readFileSync(fileURLToPath(new URL('./ItemDetail.svelte', import.meta.url)), 'utf-8');

/** The body of the innermost `function name(` / `name: async (` / `setTimeout(() =>` that contains `at`, by brace matching. */
function enclosingBlock(src: string, at: number): string {
	let depth = 0;
	for (let i = at; i >= 0; i--) {
		if (src[i] === '}') depth++;
		else if (src[i] === '{') {
			if (depth === 0) {
				const head = src.slice(Math.max(0, i - 160), i);
				if (/(function \w+\([^)]*\)[^{]*|=>\s*|async \([^)]*\)[^{]*=>\s*)$/.test(head)) {
					let d = 0;
					for (let j = i; j < src.length; j++) {
						if (src[j] === '{') d++;
						else if (src[j] === '}' && --d === 0) return src.slice(i, j + 1);
					}
				}
			} else depth--;
		}
	}
	return '';
}

describe('ItemDetail settles every save it begins (BUG-3044)', () => {
	const begins = [...SOURCE.matchAll(/(?:const |)saveTok = saves\.begin\(\);/g)];

	it('the population is the nine save paths', () => {
		// Title, field, tags, assignment, role, the legacy content debounce, the
		// collab flush, the raw saver and the raw drain.
		expect(begins.length).toBe(9);
	});

	it.each(begins.map((m) => [SOURCE.slice(0, m.index).split('\n').length, m.index!] as const))(
		'the begin at line %i is settled in a finally of the same function',
		(_line, at) => {
			const body = enclosingBlock(SOURCE, at);
			expect(body, 'no enclosing function found').not.toBe('');
			const settled =
				/finally \{[^}]*saves\.settle\(saveTok\)/.test(body) || /\.finally\(\(\) => saves\.settle\(saveTok\)\)/.test(body);
			expect(settled, 'no settle of this token in a finally').toBe(true);
		}
	);

	it('writes nothing to the indicator except through the tracker', () => {
		// Its one declaration is derived from the tracker; nothing assigns it.
		expect(SOURCE).toMatch(/const saveStatus = \$derived\(saves\.status\);/);
		expect(SOURCE).not.toMatch(/(?<!const )\bsaveStatus\s*=[^=]/);
	});
});
