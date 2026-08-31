import { describe, it, expect } from 'vitest';
import { renderMarkdown } from './markdown';
import type { Item } from '$lib/types';

// The `renderMarkdown` half of the BUG-2834 binding assertions.
//
// Lives in the jsdom project (`*.svelte.test.ts`) rather than beside the rest
// of the parity suite because renderMarkdown finishes through DOMPurify and
// returns '' with no DOM present — in the node project it fails for a reason
// that has nothing to do with the grammar, which is worse than not running.
// markdown.shareAttachments{,.svelte}.test.ts splits for the same reason.
//
// The corpus assertions and the wikiLinksToMarkdown binding live in
// markdown.grammarParity.test.ts. This file is only the second call site.

function show(s: string): string {
	return JSON.stringify(s).replace(/[\u0000-\u001f\u007f-\uffff]/g, (c) => {
		return '\\u' + c.charCodeAt(0).toString(16).padStart(4, '0').toUpperCase();
	});
}

describe('renderMarkdown consumes the parity-fixed grammar (BUG-2834 binding)', () => {
	const noItems: Item[] = [];

	// The three code points where Go's `.` and JavaScript's `.` disagreed.
	// Before the fix the renderer left these as literal `[[...]]` text while
	// the server had already indexed them as links — so the backlink panel and
	// the rendered document disagreed about whether a link existed.
	const divergent: Array<[string, number]> = [
		['CR U+000D', 0x000d],
		['LS U+2028', 0x2028],
		['PS U+2029', 0x2029]
	];

	for (const [label, cp] of divergent) {
		it(`consumes a bracket whose body has a backslash before ${label}`, () => {
			const body = 'A\\' + String.fromCodePoint(cp) + 'B';
			const html = renderMarkdown(`see [[${body}]] here`, noItems, 'ws');

			// With no matching item the bracket resolves to the broken-link
			// span. That is the honest outcome, and — the point of the test —
			// it is NOT the literal `[[` passthrough the unfixed renderer
			// produced. Asserting on the span rather than on a resolved link
			// keeps this leg independent of item-fixture shape.
			expect(html, `body=${show(body)}`).toContain('doc-link broken');
			expect(html, `body=${show(body)}`).not.toContain('[[');
		});
	}

	// Negative control. LF is the code point both languages have always
	// rejected and the fix must not have changed it; `scanBracketBody` in
	// internal/links/links.go depends on that agreement. Without this leg the
	// three assertions above would also pass for `[\s\S]`, the other candidate
	// fix, which would have over-matched.
	it('still refuses a backslash before LF, like the Go parser', () => {
		const html = renderMarkdown('see [[A\\\nB]] here', noItems, 'ws');

		expect(html).toContain('[[');
		expect(html).not.toContain('doc-link broken');
	});

	// Guards the DOM precondition itself. If DOMPurify ever silently returns
	// '' here again, every assertion above would still "pass" its not.toContain
	// leg while measuring nothing — the empty string contains neither '[[' nor
	// 'doc-link broken'. This is the leg that fails loudly instead.
	it('renders a plain wiki-link, proving the DOM path is live', () => {
		const html = renderMarkdown('see [[Anything]] here', noItems, 'ws');
		expect(html).not.toBe('');
		expect(html).toContain('doc-link broken');
	});
});
