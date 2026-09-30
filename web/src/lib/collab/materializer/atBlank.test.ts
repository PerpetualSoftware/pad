import { describe, it, expect } from 'vitest';
import { upstreamAtBlank, patchedAtBlank } from './atBlank';

// The vendored atBlank patch must agree with upstream prosemirror-markdown in
// the engine a browser tab runs, not only in goja (internal/materialize has
// the goja leg). This runs under V8. The line-terminator cases are the ones
// where ECMAScript `$` could plausibly differ from an ends-with-LF check: it
// does not, without the `m` flag, and this pins that.
describe('atBlank patch equivalence under V8 (TASK-2198)', () => {
	const call = (fn: typeof upstreamAtBlank, out: string) => fn.call({ out });

	it('agrees on line-terminator edge cases', () => {
		const cases = ['', '\n', '\r', '\r\n', '\n\r', ' ', ' ', 'a', 'a\n', 'a\r', 'a\r\n', 'a ', 'a\n\n', ' \n', '\n '];
		for (const out of cases) {
			expect(call(patchedAtBlank, out), JSON.stringify(out)).toBe(call(upstreamAtBlank, out));
		}
	});

	it('agrees on generated strings', () => {
		const alphabet = ['a', ' ', '\n', '\r', '\t', ' ', '#', '`'];
		let seed = 3252;
		const rnd = () => (seed = (seed * 1103515245 + 12345) & 0x7fffffff) / 0x7fffffff;
		for (let i = 0; i < 5000; i++) {
			const len = Math.floor(rnd() * 12);
			let out = '';
			for (let j = 0; j < len; j++) out += alphabet[Math.floor(rnd() * alphabet.length)];
			expect(call(patchedAtBlank, out), JSON.stringify(out)).toBe(call(upstreamAtBlank, out));
		}
	});
});
