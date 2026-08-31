import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { WIKI_LINK_PATTERN_SOURCE, wikiLinksToMarkdown } from './markdown';
import type { Item } from '$lib/types';

// The JavaScript half of the cross-language wiki-link grammar harness
// (BUG-2834). The Go half is internal/links/grammar_parity_test.go.
//
// Both halves read the SAME corpus and assert against the expectations
// recorded IN it, rather than against each other. That indirection is the
// entire point. The two patterns used to be byte-identical source text, so a
// reviewer comparing them side by side concluded they agreed — and they did
// not, because the divergence was in the host languages' definition of `.`,
// which is invisible to inspection. A test that compared one implementation to
// the other would have inherited exactly that blind spot; a test that pins
// each implementation to an independently-stated spec cannot.
//
// If you add a case, add it to the corpus — not to one language's test.

const CORPUS_PATH = fileURLToPath(
	new URL('../../../../testdata/wiki_grammar_corpus.json', import.meta.url)
);

type GrammarCase = {
	name: string;
	why: string;
	content: string;
	expect: { match: boolean; body?: string };
};

const corpus: { cases: GrammarCase[] } = JSON.parse(readFileSync(CORPUS_PATH, 'utf8'));

// A truncated or unparsed corpus would make every assertion below vacuous
// while the suite still reported green. Asserted, not assumed — a harness that
// cannot fail is not an instrument.
if (corpus.cases.length < 20) {
	throw new Error(`shared grammar corpus looks truncated: ${corpus.cases.length} cases`);
}

// Render invisible code points as \uXXXX. This corpus is ENTIRELY about
// characters that are invisible or that break a line in a terminal, so a
// failure message printing them raw would misreport what it compared — the
// same class of mistake as the bug under test.
function show(s: string): string {
	return JSON.stringify(s).replace(/[\u0000-\u001f\u007f-\uffff]/g, (c) => {
		return '\\u' + c.charCodeAt(0).toString(16).padStart(4, '0').toUpperCase();
	});
}

describe('wiki-link grammar parity with the Go server (BUG-2834)', () => {
	// A fresh RegExp per case: the exported value is SOURCE TEXT precisely so
	// that no `lastIndex` state is shared between this suite and the renderer.
	function firstMatch(content: string): RegExpExecArray | null {
		return new RegExp(WIKI_LINK_PATTERN_SOURCE).exec(content);
	}

	for (const tc of corpus.cases) {
		it(tc.name, () => {
			const m = firstMatch(tc.content);

			if (!tc.expect.match) {
				expect(
					m,
					`expected NO match for ${show(tc.content)}\nwhy this case exists: ${tc.why}`
				).toBeNull();
				return;
			}

			expect(
				m,
				`expected a match for ${show(tc.content)}\nwhy this case exists: ${tc.why}`
			).not.toBeNull();
			expect(
				show(m![1]),
				`captured body mismatch for ${show(tc.content)}\nwhy this case exists: ${tc.why}`
			).toBe(show(tc.expect.body!));
		});
	}
});

// The suite above vouches for the exported PATTERN. On its own that is a
// direct-call test: it proves the source text is right and says nothing about
// whether anything USES it (CONVE-19 — wiring is a claim). The original bug was
// in rendering behaviour, not in a constant, so both call sites get asserted
// through their public entry points.
//
// This file covers `wikiLinksToMarkdown` (pure string in, string out).
// `renderMarkdown` is covered in markdown.grammarParity.svelte.test.ts, because
// it finishes through DOMPurify and returns '' without a DOM — in the node
// project it would appear to "fail" for a reason that has nothing to do with
// the grammar. Same node/jsdom split, and same reason, as
// markdown.shareAttachments{,.svelte}.test.ts.
describe('wikiLinksToMarkdown consumes the parity-fixed grammar (BUG-2834 binding)', () => {
	// An UNRESOLVED body is useless as a discriminator here: on a miss
	// wikiLinksToMarkdown deliberately returns the match verbatim, so a bracket
	// the grammar rejected and a bracket it accepted-but-could-not-resolve
	// produce byte-identical output. The fixture therefore has to RESOLVE, so
	// that "was the bracket matched at all" becomes observable as a link.
	//
	// resolveWikiBody step 3 matches on unescapeWikiBody(key), and that only
	// unescapes `\\`, `\]` and `\|` — a backslash before CR/LS/PS is none of
	// those, so it survives into the key and the item title must carry it too.
	function itemTitled(title: string): Item {
		return {
			id: 'id-1',
			title,
			collection_slug: 'tasks',
			slug: 'fixture'
		} as unknown as Item;
	}

	const divergent: Array<[string, number]> = [
		['CR U+000D', 0x000d],
		['LS U+2028', 0x2028],
		['PS U+2029', 0x2029]
	];

	for (const [label, cp] of divergent) {
		it(`links a body with a backslash before ${label}`, () => {
			const title = 'A\\' + String.fromCodePoint(cp) + 'B';
			const out = wikiLinksToMarkdown(`see [[${title}]] here`, [itemTitled(title)], 'ws');

			expect(out, `title=${show(title)}`).toContain('](/ws/tasks/');
			expect(out, `title=${show(title)}`).not.toContain('[[');
		});
	}

	// Negative control for the three legs above. LF is the code point BOTH
	// languages have always rejected, and the fix must not have changed it.
	// Without this leg, "the renderer now consumes more brackets" would be
	// satisfied by a pattern that consumes everything — including by
	// `[\s\S]`, which was the other candidate fix and is WRONG here.
	it('still refuses a backslash before LF, like the Go parser', () => {
		const title = 'A\\\nB';
		const out = wikiLinksToMarkdown(`see [[${title}]] here`, [itemTitled(title)], 'ws');

		expect(out).toContain('[[');
		expect(out).not.toContain('](/ws/tasks/');
	});
});
