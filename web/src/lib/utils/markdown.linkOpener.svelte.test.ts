import { describe, it, expect } from 'vitest';
import { renderMarkdown, sanitizeMarkdownHtml, sanitizeHtmlBlock } from './markdown';

// BUG-3359: raw HTML in markdown bypasses the safe link renderer, and the
// sanitizer kept the author's target and rel, so <a target="_blank"
// rel="opener"> reached {@html} with window.opener live in the new tab
// (reverse tabnabbing). Any link that opens elsewhere now leaves the
// sanitizer with rel containing noopener and noreferrer and never opener.

function anchors(html: string): HTMLAnchorElement[] {
	const div = document.createElement('div');
	div.innerHTML = html;
	return Array.from(div.querySelectorAll('a'));
}

function relTokens(a: HTMLAnchorElement): string[] {
	return (a.getAttribute('rel') ?? '').toLowerCase().split(/\s+/).filter(Boolean);
}

const payloads = [
	'<a href="https://evil.example/" target="_blank" rel="opener">x</a>',
	'<a href="https://evil.example/" target="_blank" rel="OPENER nofollow">x</a>',
	'<a href="https://evil.example/" target="_blank">x</a>',
	'<a href="https://evil.example/" target="other">x</a>',
	'<a href="https://evil.example/" target="_top" rel="opener">x</a>'
];

describe('BUG-3359: links that open elsewhere never keep the opener', () => {
	const sanitizers: [string, (html: string) => string][] = [
		['sanitizeMarkdownHtml', sanitizeMarkdownHtml],
		['sanitizeHtmlBlock', sanitizeHtmlBlock],
		['renderMarkdown (raw HTML in markdown)', (html) => renderMarkdown(html, [], 'ws')]
	];
	for (const [name, sanitize] of sanitizers) {
		for (const payload of payloads) {
			it(`${name}: ${payload}`, () => {
				const [a] = anchors(sanitize(payload));
				expect(a, 'the link itself survives').toBeTruthy();
				const rel = relTokens(a);
				expect(rel).not.toContain('opener');
				expect(rel).toContain('noopener');
				expect(rel).toContain('noreferrer');
			});
		}
	}

	it('keeps an author rel token that is not opener', () => {
		const [a] = anchors(sanitizeMarkdownHtml(payloads[1]));
		expect(relTokens(a)).toContain('nofollow');
	});

	it('leaves a same-tab link alone', () => {
		for (const html of ['<a href="https://ok.example/">x</a>', '<a href="https://ok.example/" target="_self" rel="nofollow">x</a>']) {
			const [a] = anchors(sanitizeMarkdownHtml(html));
			expect(relTokens(a)).not.toContain('noopener');
		}
	});

	it('still renders markdown links as before', () => {
		const [a] = anchors(renderMarkdown('[x](https://ok.example/)', [], 'ws'));
		expect(a.getAttribute('target')).toBe('_blank');
		expect(relTokens(a)).toEqual(expect.arrayContaining(['noopener', 'noreferrer']));
	});
});
