// The quote grammar for the selection toolbar's Comment action (IDEA-2843).
//
// The triage pinned it: "the same markdown blockquote a manual quote would
// produce, nothing new". The cases below are the ones where "prefix each line"
// and "produce a valid blockquote" come apart.
import { describe, it, expect } from 'vitest';
import { toBlockquote } from './markdown';

describe('toBlockquote', () => {
	it('prefixes a single line', () => {
		expect(toBlockquote('the passage')).toBe('> the passage');
	});

	it('prefixes BLANK lines too, or the quote ends early', () => {
		// An unprefixed blank line terminates a blockquote in markdown, so the
		// second paragraph would render as the commenter's own words — the
		// quote silently losing half its content while looking fine.
		expect(toBlockquote('para one\n\npara two')).toBe('> para one\n>\n> para two');
	});

	it('emits a bare > for a blank line rather than trailing whitespace', () => {
		expect(toBlockquote('a\n\nb')).not.toContain('> \n');
	});

	it('normalizes CRLF, so a Windows selection quotes the same', () => {
		expect(toBlockquote('one\r\ntwo')).toBe('> one\n> two');
	});

	it('leaves markdown inside the selection alone', () => {
		// The selection is quoted, not re-rendered: a list stays a list inside
		// the quote rather than being escaped or flattened.
		expect(toBlockquote('- a\n- b')).toBe('> - a\n> - b');
	});
});
