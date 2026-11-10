import { describe, expect, it } from 'vitest';
import { renderMarkedWithAttachments, renderMarkdownDocument } from './markdown';

const ctx = {
	resolver: (id: string) => ({ id, mime_type: 'image/png', filename: 'image.png', size_bytes: 12 }),
	workspaceSlug: 'ws', urlBuilder: () => '/resolved/image.png',
};
describe('share body line breaks (BUG-4447)', () => {
	for (const body of ['a\nb', '> a\n> b', '- a\n  b']) {
		it(`shows ${JSON.stringify(body)} on separate lines`, () => {
			expect(renderMarkedWithAttachments(body, ctx)).toMatch(/a<br>\s*b/);
		});
	}
	it('resolves attachments alongside line breaks', () => {
		const html = renderMarkedWithAttachments('a\nb\n\n![shot](pad-attachment:0b1f3c9e-1d2a-4e5f-8a9b-0c1d2e3f4a5b)', ctx);
		expect(html).toMatch(/a<br>\s*b/);
		expect(html).toContain('/resolved/image.png');
	});
	it('keeps fenced code and standalone document rendering unchanged', () => {
		const html = renderMarkedWithAttachments('```\na\nb\n```', ctx);
		expect(html).toContain('a\nb\n</code>');
		expect(html).not.toContain('<br>');
		expect(renderMarkdownDocument('a\nb')).not.toContain('<br>');
	});
});
