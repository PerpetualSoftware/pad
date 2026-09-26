// TASK-2248 U1 (audit C83 / C84): the share page's post-pass makes wiki-links
// and internal links inert for an anonymous viewer. jsdom, because the pass
// runs on a DOM.
import { describe, it, expect } from 'vitest';
import DOMPurify from 'dompurify';
import { marked } from 'marked';
import { inertInternalReferences, isInternalHref } from './shareRender';

const ORIGIN = location.origin;
const PAGE = `${ORIGIN}/s/tok`;
const titles = new Map([['TASK-5', 'Ship the navbar']]);
const render = (md: string) => inertInternalReferences(DOMPurify.sanitize(marked(md) as string), titles, PAGE);
const doc = (html: string) => {
	const d = document.createElement('div');
	d.innerHTML = html;
	return d;
};

describe('share render: wiki-links read as emphasized text, never links (C83)', () => {
	it('every wiki form becomes its display text', () => {
		const d = doc(render('A [[Plain Title]], [[TASK-5]], [[BUG-9]], [[TASK-5|custom]], [[other-ws::PLAN-2]] and [[other-ws::PLAN-2|there]].'));
		const ems = [...d.querySelectorAll('em.share-wiki-ref')].map((e) => e.textContent);
		expect(ems).toEqual(['Plain Title', 'Ship the navbar', 'BUG-9', 'custom', 'other-ws::PLAN-2', 'there']);
		expect(d.textContent).not.toContain('[[');
		expect(d.querySelector('a')).toBeNull();
	});

	it('a ref resolves case-insensitively, and only to a title the payload holds', () => {
		const d = doc(render('[[task-5]] and [[TASK-6]]'));
		expect([...d.querySelectorAll('em')].map((e) => e.textContent)).toEqual(['Ship the navbar', 'TASK-6']);
	});

	it('a qualified [[collection/Title]] stays whole: a real title may contain a slash', () => {
		expect(doc(render('[[A/B testing]]')).querySelector('em')!.textContent).toBe('A/B testing');
	});

	it('code spans and code blocks are left as written', () => {
		const d = doc(render('`[[TASK-5]]`\n\n```\n[[TASK-5]]\n```'));
		expect(d.querySelector('em')).toBeNull();
		expect([...d.querySelectorAll('code')].map((c) => c.textContent?.trim())).toEqual(['[[TASK-5]]', '[[TASK-5]]']);
	});

	it('a display text that READS as markup stays text: the pass never turns text into elements', () => {
		const d = doc(render('[[TASK-6|&lt;img src=x onerror=alert(1)&gt;]]'));
		expect(d.querySelector('img')).toBeNull();
		expect(d.querySelector('em')!.textContent).toBe('<img src=x onerror=alert(1)>');
	});
});

describe('share render: internal links become plain text, viewer links stay (C84)', () => {
	it('a root-relative item link is downgraded to text with a requires-access title', () => {
		const d = doc(render('See [the plan](/dave/docapp/plans/PLAN-2) now.'));
		expect(d.querySelector('a')).toBeNull();
		const span = d.querySelector('span.share-internal-ref')!;
		expect(span.textContent).toBe('the plan');
		expect(span.getAttribute('title')).toMatch(/requires access/i);
	});

	it('an absolute same-origin link is internal too', () => {
		expect(doc(render(`[x](${ORIGIN}/dave/docapp)`)).querySelector('a')).toBeNull();
	});

	it('share pages and token-scoped attachment bytes stay links', () => {
		const d = doc(render('[other share](/s/abc) and [file](/api/v1/s/tok/attachments/u1)'));
		expect([...d.querySelectorAll('a')].map((a) => a.getAttribute('href'))).toEqual(['/s/abc', '/api/v1/s/tok/attachments/u1']);
	});

	it('an external link stays, opening in a new tab without an opener', () => {
		const a = doc(render('[site](https://getpad.dev/docs)')).querySelector('a')!;
		expect(a.getAttribute('href')).toBe('https://getpad.dev/docs');
		expect(a.getAttribute('target')).toBe('_blank');
		expect(a.getAttribute('rel')).toBe('noopener noreferrer');
	});

	it('protocol-relative and page-local hrefs are not internal routes', () => {
		expect(isInternalHref('//example.com/x', PAGE)).toBe(false);
		expect(isInternalHref('#section', PAGE)).toBe(false);
		expect(isInternalHref('?q=1', PAGE)).toBe(false);
		expect(isInternalHref('mailto:a@b.c', PAGE)).toBe(false);
		expect(isInternalHref('/dave/ws', PAGE)).toBe(true);
		expect(isInternalHref(' /dave/ws', PAGE)).toBe(true);
	});

	// Codex round 1: a raw-prefix test let each of these through. The browser
	// normalises them before navigating, so the check must too.
	it('backslashes, dot segments and letter case are judged by where the browser goes', () => {
		// `\/dave/ws` and `/\dave/ws` are both `//dave/ws` to a browser: a
		// protocol-relative link to a HOST named dave, so another origin, not a
		// route of this app. Measured with WHATWG URL, not reasoned.
		expect(isInternalHref('\\/dave/ws', PAGE)).toBe(false);
		expect(isInternalHref('/\\dave/ws', PAGE)).toBe(false);
		expect(isInternalHref('/s/../dave/ws', PAGE)).toBe(true);
		expect(isInternalHref('/api/v1/s/../../../dave/ws', PAGE)).toBe(true);
		expect(isInternalHref('/s/%2e%2e/dave/ws', PAGE)).toBe(true);
		expect(isInternalHref('/S/abc', PAGE)).toBe(true);
		expect(isInternalHref(`${ORIGIN.toUpperCase()}/dave/ws`, PAGE)).toBe(true);
		expect(isInternalHref('../dave/ws', PAGE)).toBe(true);
	});

	it('an image-map area into the app loses its href', () => {
		const out = inertInternalReferences('<map name="m"><area href="/dave/ws" alt="x"><area href="https://getpad.dev" alt="y"></map>', titles, PAGE);
		const areas = [...doc(out).querySelectorAll('area')];
		expect(areas.map((a) => a.getAttribute('href'))).toEqual([null, 'https://getpad.dev']);
	});
});
