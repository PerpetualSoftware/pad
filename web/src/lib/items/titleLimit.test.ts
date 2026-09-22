import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { MAX_ITEM_TITLE_RUNES, serverTrimmedTitle, titleLimitError } from './titleLimit';

// The JS half of the item-title limit parity harness (BUG-3115). The Go half
// is internal/models/title_limit_parity_test.go; both assert against the same
// corpus, so neither language's behaviour is what the other is measured by.

const CORPUS_PATH = fileURLToPath(
	new URL('../../../../testdata/item_title_limit.json', import.meta.url)
);

type Case = {
	name: string;
	why: string;
	prefix: string;
	unit: string;
	count: number;
	suffix: string;
	runes: number;
	ok: boolean;
};
const corpus: { max_runes: number; go_space: number[]; cases: Case[] } = JSON.parse(
	readFileSync(CORPUS_PATH, 'utf8')
);

if (corpus.cases.length < 8 || corpus.go_space.length < 20) {
	throw new Error('title-limit corpus looks truncated');
}

describe('titleLimit (BUG-3115)', () => {
	it('uses the limit the server enforces', () => {
		expect(MAX_ITEM_TITLE_RUNES).toBe(corpus.max_runes);
	});

	for (const c of corpus.cases) {
		it(`${c.name} — ${c.why}`, () => {
			const title = c.prefix + c.unit.repeat(c.count) + c.suffix;
			expect(titleLimitError(title)).toBe(
				c.ok ? null : `Title is too long: ${c.runes} characters, maximum ${corpus.max_runes}`
			);
		});
	}

	it('trims exactly the code points Go trims', () => {
		for (const cp of corpus.go_space) {
			const s = String.fromCodePoint(cp);
			expect(serverTrimmedTitle(`${s}x${s}`), `U+${cp.toString(16)}`).toEqual(['x']);
		}
		// Controls: JS's trim strips U+FEFF, Go's does not; a zero-width space
		// is not whitespace to either.
		for (const cp of [0xfeff, 0x200b]) {
			const s = String.fromCodePoint(cp);
			expect(serverTrimmedTitle(`${s}x`).length, `U+${cp.toString(16)}`).toBe(2);
		}
	});
});
