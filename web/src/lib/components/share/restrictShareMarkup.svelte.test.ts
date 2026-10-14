// BUG-3799: restrictShareMarkup keeps exactly what the markdown renderer
// emits on the share path, on the tag it emits it on. The census below renders
// a corpus through the share pipeline, so a renderer that starts emitting a new
// class fails here rather than losing its styling on share pages silently.
import { describe, it, expect } from 'vitest';
import { restrictShareMarkup, shareClassAllowlist } from './shareRender';
import { renderMarkedWithAttachments, sanitizeMarkdownHtml } from '$lib/utils/markdown';
import { renderAttachmentUnavailable } from '$lib/markdown/attachments';

const classesIn = (html: string) => {
	const tpl = document.createElement('template');
	tpl.innerHTML = html;
	return [...tpl.content.querySelectorAll('[class]')].flatMap((el) => [...el.classList].map((c) => `${el.tagName.toLowerCase()}.${c}`)).sort();
};

describe('restrictShareMarkup (BUG-3799)', () => {
	it('strips classes the renderer never emits, and classes on the wrong tag', () => {
		const out = restrictShareMarkup('<div class="attachment-crop-modal-shell sr-only">x</div><span class="external-link">y</span><code class="language-js hljs">z</code><div class="language-js">w</div>');
		expect(classesIn(out)).toEqual(['code.language-js']);
		expect(out).toContain('x');
	});

	it('keeps only the task-list checkbox among inputs, and disables it', () => {
		const out = restrictShareMarkup('<p><input type="text" name="pw"> <input type="password"> <input> <input type="checkbox" checked> task</p>');
		const tpl = document.createElement('template');
		tpl.innerHTML = out;
		const inputs = [...tpl.content.querySelectorAll('input')];
		expect(inputs.map((i) => i.getAttribute('type')), 'a non-checkbox input survived').toEqual(['checkbox']);
		expect(inputs[0].hasAttribute('disabled'), 'the checkbox stayed enabled').toBe(true);
		expect(out).toContain('task');
	});

	it('keeps an element and its text when every class goes', () => {
		expect(restrictShareMarkup('<p class="sr-only">kept</p>')).toBe('<p>kept</p>');
	});

	it('fails closed without a DOM', () => {
		const doc = globalThis.document;
		// @ts-expect-error simulating SSR
		delete globalThis.document;
		try {
			expect(restrictShareMarkup('<p class="x">t</p>')).toBe('');
		} finally {
			globalThis.document = doc;
		}
	});

	it('census: every class the share pipeline emits survives', () => {
		const IMG = '11111111-1111-4111-8111-111111111111';
		const FILE = '22222222-2222-4222-8222-222222222222';
		const GONE = '33333333-3333-4333-8333-333333333333';
		const corpus = [
			'Para with [ext](https://getpad.dev).', '', '```js', 'a', '```', '', '```mermaid', 'graph TD; A-->B', '```', '',
			'- [ ] todo', '- [x] done', '', '| a | b |', '|:--|--:|', '| 1 | 2 |', '',
			`![pic](pad-attachment:${IMG})`, '', `[file.pdf](pad-attachment:${FILE})`, '', `![gone](pad-attachment:${GONE})`, '',
			'[[TASK-5]]',
		].join('\n');
		const raw = renderMarkedWithAttachments(corpus, {
			resolver: (uuid) =>
				uuid === IMG ? { id: IMG, mime_type: 'image/png', filename: 'p.png', size_bytes: 0, width: 10, height: 10 }
				: uuid === FILE ? { id: FILE, mime_type: 'application/pdf', filename: 'file.pdf', size_bytes: 1234, width: null, height: null }
				: null,
			urlBuilder: (uuid) => `/api/v1/s/tok/attachments/${uuid}`,
			workspaceSlug: '',
			missing: renderAttachmentUnavailable,
		});
		const sanitized = sanitizeMarkdownHtml(raw);
		const before = classesIn(sanitized);
		expect(before, 'premise: the corpus emits the census classes').toEqual(expect.arrayContaining([
			'a.external-link', 'a.file-chip', 'code.language-js', 'code.language-mermaid', 'span.external-icon',
			'span.file-chip-icon', 'span.file-chip-name', 'span.file-chip-size', 'span.attachment-unavailable',
		]));
		expect(classesIn(restrictShareMarkup(sanitized))).toEqual(before);
		// And the allowlist holds nothing the census did not find (code's
		// language-* is a pattern, checked above): an extra pair is a class an
		// editor could write that the renderer never would (Codex r2).
		const census = [...new Set(before.filter((c) => !c.startsWith('code.')))].sort();
		expect(shareClassAllowlist()).toEqual(census);
	});
});
