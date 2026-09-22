import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import {
	copyTitle,
	MAX_ITEM_TITLE_RUNES,
	serverTrimmedTitle,
	titleEditError,
	titleLimitError
} from './titleLimit';

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

// The update-door half (lead NO-GO on #1437): the server re-validates a title
// only when it CHANGES, so a legacy over-limit title must stay saveable as long
// as it is sent back unchanged. Mirrors TestPatchItemVerbatimEchoOfALegacyTitleIsANoOp.
describe('titleEditError (BUG-3115)', () => {
	const legacy = 'm'.repeat(MAX_ITEM_TITLE_RUNES + 45);

	it('an unchanged legacy over-limit title is not refused', () => {
		expect(titleEditError(legacy, legacy)).toBeNull();
	});

	it('a legacy title stored with Go-trimmable edges, sent trimmed, is still an echo', () => {
		// U+0085 is trimmed by Go but not by JS: the stored row carries it, the
		// door sends what JS trim leaves, and the server TrimSpaces before comparing.
		expect(titleEditError(`${legacy}\u0085`.trim(), legacy)).toBeNull();
		expect(titleEditError(`\u0085${legacy}`, legacy)).toBeNull();
	});

	it('CONTROL: a changed over-limit title is still refused', () => {
		expect(titleEditError(legacy + 'x', legacy)).toBe(
			`Title is too long: ${legacy.length + 1} characters, maximum ${MAX_ITEM_TITLE_RUNES}`
		);
		expect(titleEditError('n'.repeat(MAX_ITEM_TITLE_RUNES + 1), 'short')).not.toBeNull();
	});

	it('a changed title within the limit passes', () => {
		expect(titleEditError('renamed', legacy)).toBeNull();
	});
});

// A duplicate's generated title (BUG-3149): the source is cut so that
// " (copy)" always survives and the result always passes the server's check.
describe('copyTitle (BUG-3149)', () => {
	const runes = (s: string) => Array.from(s).length;

	it('a short title is copied whole', () => {
		expect(copyTitle('Ship')).toBe('Ship (copy)');
	});

	it('a title that fits exactly is not cut', () => {
		const src = 'a'.repeat(MAX_ITEM_TITLE_RUNES - 7);
		expect(copyTitle(src)).toBe(`${src} (copy)`);
	});

	it('a 255-rune source gives exactly 255 runes ending " (copy)"', () => {
		const out = copyTitle('a'.repeat(MAX_ITEM_TITLE_RUNES));
		expect(runes(out)).toBe(MAX_ITEM_TITLE_RUNES);
		expect(out.endsWith(' (copy)')).toBe(true);
		expect(titleLimitError(out)).toBeNull();
	});

	it('an astral source is cut by code point, never mid-pair', () => {
		const out = copyTitle('😀'.repeat(MAX_ITEM_TITLE_RUNES));
		expect(runes(out)).toBe(MAX_ITEM_TITLE_RUNES);
		expect(out).toBe('😀'.repeat(MAX_ITEM_TITLE_RUNES - 7) + ' (copy)');
		expect(titleLimitError(out)).toBeNull();
	});

	it('a cut landing just after a space does not double the space', () => {
		// The space sits at the last kept position, so the cut exposes it.
		const src = 'a'.repeat(MAX_ITEM_TITLE_RUNES - 8) + ' ' + 'b'.repeat(20);
		expect(copyTitle(src)).toBe('a'.repeat(MAX_ITEM_TITLE_RUNES - 8) + ' (copy)');
	});

	it('the re-trim uses Go\'s space set, not JS\'s', () => {
		// U+0085 is a Go space and is trimmed; U+FEFF is not and is kept.
		const keep = MAX_ITEM_TITLE_RUNES - 8;
		expect(copyTitle('a'.repeat(keep) + '\u0085' + 'b'.repeat(20))).toBe(
			'a'.repeat(keep) + ' (copy)'
		);
		expect(copyTitle('a'.repeat(keep) + '﻿' + 'b'.repeat(20))).toBe(
			'a'.repeat(keep) + '﻿ (copy)'
		);
	});

	it('the source is measured after the server\'s trim', () => {
		const src = '  ' + 'a'.repeat(MAX_ITEM_TITLE_RUNES - 7) + '\u0085';
		expect(copyTitle(src)).toBe('a'.repeat(MAX_ITEM_TITLE_RUNES - 7) + ' (copy)');
	});
});
