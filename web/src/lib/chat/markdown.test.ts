// TASK-4315 PR 2a: with no DOM the Chat renderer yields escaped text, never HTML.
import { describe, it, expect } from 'vitest';
import { renderChatMarkdown } from './markdown';
import { sanitizeMarkdownHtml } from '$lib/utils/markdown';

describe('no window', () => {
	it('is the node project', () => {
		expect(typeof window).toBe('undefined');
	});
	it('returns the body as escaped text, never markup', () => {
		const out = renderChatMarkdown('<img src=x onerror=alert(1)> **b** [[TASK-1]] & "q"', [], 'docapp');
		expect(out).not.toContain('<');
		expect(out).toContain('&lt;img src=x onerror=alert(1)&gt;');
		expect(out).toContain('**b**');
		expect(out).toContain('&amp;');
	});
	it('the sanitizer returns an empty string for the Chat profile too', () => {
		expect(sanitizeMarkdownHtml('<b>x</b>', 'chat')).toBe('');
	});
	it('an empty body is empty', () => {
		expect(renderChatMarkdown('', [], 'docapp')).toBe('');
	});
});
