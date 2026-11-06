// TASK-4315 PR 2a: the Chat markdown renderer. Every case feeds untrusted text
// (agent and human bodies take the same path) and asserts on the parsed DOM of
// what a reader would be handed, never on string shape alone.
import { describe, it, expect } from 'vitest';
import { renderChatMarkdown } from './markdown';
import { extractRefs } from './refs';
import { renderMarkdown, sanitizeMarkdownHtml } from '$lib/utils/markdown';
import { ITEM_PIN_INPUTS } from '$lib/utils/markdown.itemPin.inputs';
import type { ChatRef } from '$lib/types';

const ORIGIN = () => window.location.origin;
const UUID = '0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d';

function dom(html: string): HTMLDivElement {
	const d = document.createElement('div');
	d.innerHTML = html;
	return d;
}
const render = (body: string, refs: ChatRef[] = [], ws?: string) => dom(renderChatMarkdown(body, refs, ws));
const attUrl = (ws = 'docapp', id = UUID, q = '') => `${ORIGIN()}/api/v1/workspaces/${ws}/attachments/${id}${q}`;

/** No executable surface anywhere in the output. */
function expectInert(d: HTMLElement) {
	for (const el of Array.from(d.querySelectorAll('*'))) {
		for (const a of Array.from(el.attributes)) {
			expect(a.name.startsWith('on'), `${el.tagName} ${a.name}`).toBe(false);
			expect(a.name).not.toBe('style');
		}
		expect(['SCRIPT', 'IFRAME', 'OBJECT', 'EMBED', 'FORM', 'SVG', 'MATH', 'INPUT', 'STYLE']).not.toContain(el.tagName);
	}
}

describe('raw HTML shows literally', () => {
	for (const payload of [
		'<script>alert(1)</script>',
		'<img src=x onerror=alert(1)>',
		'<iframe src="https://evil.example"></iframe>',
		'<svg onload=alert(1)></svg>',
		'<div style="position:fixed">x</div>',
		'<form action="https://evil.example"><input></form>'
	]) {
		it(payload, () => {
			const d = render(`before\n\n${payload}\n\nafter`);
			expectInert(d);
			expect(d.querySelector('img,script,iframe,svg,form,div')).toBeNull();
			expect(d.textContent).toContain(payload);
		});
	}
	it('inline raw html inside a paragraph too', () => {
		const d = render('a <b onclick=x()>bold</b> b');
		expect(d.querySelector('b')).toBeNull();
		expect(d.textContent).toContain('<b onclick=x()>bold</b>');
	});
});

describe('link schemes', () => {
	const bad = [
		'javascript:alert(1)',
		'JaVaScRiPt:alert(1)',
		'java\tscript:alert(1)',
		'java\nscript:alert(1)',
		'&#106;avascript:alert(1)',
		'&#x6A;avascript:alert(1)',
		'%6Aavascript:alert(1)',
		'data:text/html;base64,PHNjcmlwdD4=',
		'blob:https://example.com/uuid',
		'vbscript:msgbox(1)',
		'file:///etc/passwd',
		'mailto:a@b.example',
		'tel:123',
		'ftp://example.com/x',
		'custom-scheme:foo',
		'https://user:pw@example.com/',
		'https://user@example.com/',
		'http://\u0001example.com/',
		'https://exa\u0000mple.com/'
	];
	for (const href of bad) {
		it(`rejects ${JSON.stringify(href)}`, () => {
			const d = render(`[click](<${href}>) and <${href}>`);
			expectInert(d);
			expect(d.querySelector('[href]'), d.innerHTML).toBeNull();
			expect(d.textContent).toContain('click');
		});
	}
	it('accepts http and https', () => {
		const d = render('[a](https://example.com/p?q=1#f) [b](http://example.com)');
		const hrefs = Array.from(d.querySelectorAll('a')).map((a) => a.getAttribute('href'));
		expect(hrefs).toEqual(['https://example.com/p?q=1#f', 'http://example.com/']);
	});
	it('resolves a relative link against the page origin (same-origin stays path-relative)', () => {
		const d = render('[r](/-/r/docapp/TASK-1) [s](//example.org/x)');
		const hrefs = Array.from(d.querySelectorAll('a')).map((a) => a.getAttribute('href'));
		expect(hrefs[0]).toBe('/-/r/docapp/TASK-1');
		expect(hrefs[1]).toBe(`${window.location.protocol}//example.org/x`);
	});
	it('autolinks a bare url', () => {
		const d = render('see https://example.com/x now');
		expect(d.querySelector('a')!.getAttribute('href')).toBe('https://example.com/x');
	});
});

describe('opener protection on every anchor', () => {
	it('overwrites authored target and rel on a raw <a> (shown literally, so no anchor appears)', () => {
		const d = render('<a href="https://evil.example/" target="_top" rel="opener">x</a>');
		expect(d.querySelector('a')).toBeNull();
	});
	it('markdown links carry target=_blank and exactly rel="noopener noreferrer"', () => {
		const d = render('[a](https://e.example/ "t") [b](https://e.example/b)');
		const as = Array.from(d.querySelectorAll('a'));
		expect(as).toHaveLength(2);
		for (const a of as) {
			expect(a.getAttribute('target')).toBe('_blank');
			expect(a.getAttribute('rel')).toBe('noopener noreferrer');
		}
	});
	it('the sanitizer hook discards authored target and rel itself (Chat profile)', () => {
		const out = dom(
			sanitizeMarkdownHtml(
				'<a href="https://e.example/" target="_top" rel="opener nofollow">x</a><a href="https://e.example/" target="_self" rel="">y</a><a href="/-/r/w/A-1">z</a>',
				'chat'
			)
		);
		const as = Array.from(out.querySelectorAll('a'));
		expect(as).toHaveLength(3);
		for (const a of as) {
			expect(a.getAttribute('target')).toBe('_blank');
			expect(a.getAttribute('rel')).toBe('noopener noreferrer');
		}
	});
	it('the hook drops an anchor href that fails the link policy', () => {
		const out = dom(sanitizeMarkdownHtml('<a href="https://u:p@e.example/">x</a><a href="javascript:1">y</a>', 'chat'));
		expect(out.querySelector('[href]')).toBeNull();
		for (const a of Array.from(out.querySelectorAll('a'))) expect(a.getAttribute('rel')).toBe('noopener noreferrer');
	});
});

describe('images', () => {
	it('external images become escaped alt text plus a note, with no fetch', () => {
		const d = render('![a <b>&"x](https://evil.example/t.png)');
		expect(d.querySelector('img')).toBeNull();
		expect(d.textContent).toContain('a <b>&"x');
		expect(d.textContent).toContain('External image omitted');
		expect(d.querySelector('b')).toBeNull();
	});
	it('data:, blob:, javascript: and relative-external images are omitted', () => {
		for (const src of ['data:image/png;base64,AAAA', 'blob:https://x/y', 'javascript:alert(1)', '/not/an/attachment.png']) {
			const d = render(`![x](<${src}>)`);
			expect(d.querySelector('img'), src).toBeNull();
		}
	});
	it('pad-attachment: images and links stay inert placeholders', () => {
		const d = render(`![pic](pad-attachment:${UUID}) [file](pad-attachment:${UUID})`);
		expect(d.querySelector('img,a')).toBeNull();
		expect(d.textContent).toContain('pic');
		expect(d.textContent).toContain('file');
	});

	const good: Array<[string, string]> = [
		['bare', attUrl()],
		['thumb-sm', attUrl('docapp', UUID, '?variant=thumb-sm')],
		['thumb-md', attUrl('docapp', UUID, '?variant=thumb-md')],
		['original', attUrl('docapp', UUID, '?variant=original')],
		['upper-case uuid', attUrl('docapp', UUID.toUpperCase())],
		['relative same-origin path', `/api/v1/workspaces/docapp/attachments/${UUID}`],
		['hyphenated slug', attUrl('my-work-space')]
	];
	for (const [name, src] of good) {
		it(`accepts ${name}`, () => {
			const d = render(`![alt text](${src} "ttl")`);
			const img = d.querySelector('img');
			expect(img, d.innerHTML).not.toBeNull();
			expect(img!.getAttribute('alt')).toBe('alt text');
			expect(img!.getAttribute('loading')).toBe('lazy');
			expect(img!.getAttribute('decoding')).toBe('async');
			expect(img!.getAttribute('src')!.endsWith(new URL(src, ORIGIN()).pathname + new URL(src, ORIGIN()).search)).toBe(true);
		});
	}
	const bad: Array<[string, string]> = [
		['other origin', `https://evil.example/api/v1/workspaces/docapp/attachments/${UUID}`],
		['other scheme, same host', `${ORIGIN().replace(/^https?/, (s) => (s === 'http' ? 'https' : 'http'))}/api/v1/workspaces/docapp/attachments/${UUID}`],
		['credentials', attUrl().replace('//', '//u:p@')],
		['not a uuid', attUrl('docapp', 'abc')],
		['uuid with suffix', attUrl('docapp', UUID + 'x')],
		['trailing path', attUrl('docapp', UUID) + '/extra'],
		['trailing slash', attUrl('docapp', UUID) + '/'],
		['transform route', attUrl('docapp', UUID) + '/transform'],
		['encoded slash in slug', `${ORIGIN()}/api/v1/workspaces/docapp%2Fx/attachments/${UUID}`],
		['encoded separator before uuid', `${ORIGIN()}/api/v1/workspaces/docapp/attachments%2F${UUID}`],
		['encoded dot segment', `${ORIGIN()}/api/v1/workspaces/docapp/attachments/%2e%2e/${UUID}`],
		['dot segment', `${ORIGIN()}/api/v1/workspaces/docapp/attachments/../attachments/${UUID}`],
		['backslash', `${ORIGIN()}/api/v1/workspaces/docapp\\attachments/${UUID}`],
		['wrong prefix', `${ORIGIN()}/api/v2/workspaces/docapp/attachments/${UUID}`],
		['unknown variant', attUrl('docapp', UUID, '?variant=huge')],
		['extra query param', attUrl('docapp', UUID, '?variant=original&x=1')],
		['other query param', attUrl('docapp', UUID, '?x=1')],
		['repeated variant', attUrl('docapp', UUID, '?variant=original&variant=original')],
		['empty query', attUrl('docapp', UUID, '?')],
		['fragment', attUrl('docapp', UUID, '#x')],
		['query then fragment', attUrl('docapp', UUID, '?variant=original#x')],
		['upper-case variant', attUrl('docapp', UUID, '?variant=ORIGINAL')],
		['empty slug', `${ORIGIN()}/api/v1/workspaces//attachments/${UUID}`]
	];
	for (const [name, src] of bad) {
		it(`rejects ${name}`, () => {
			const d = render(`![alt](<${src}>)`);
			expect(d.querySelector('img'), d.innerHTML).toBeNull();
			expect(d.textContent).toContain('alt');
		});
	}
	it('the sanitizer hook also strips a non-route img src (Chat profile)', () => {
		const out = dom(
			sanitizeMarkdownHtml(
				`<img src="https://evil.example/x.png" alt="a" onerror="x()" width="9999"><img src="${attUrl()}" alt="b" style="x" width="9">`,
				'chat'
			)
		);
		const imgs = Array.from(out.querySelectorAll('img'));
		expect(imgs.filter((i) => i.hasAttribute('src'))).toHaveLength(1);
		expect(imgs.find((i) => i.hasAttribute('src'))!.getAttributeNames().sort()).toEqual(['alt', 'decoding', 'loading', 'src']);
	});
});

describe('attribute allowlist (Chat profile)', () => {
	it('keeps only the documented attributes and drops ids, data, aria and style', () => {
		const out = dom(
			sanitizeMarkdownHtml(
				'<p id="x" class="c" style="color:red" data-a="1" aria-label="l" title="t">p</p><span class="c" data-x="1">s</span><ol start="3" id="o"><li>a</li></ol>',
				'chat'
			)
		);
		expect(out.querySelector('p')!.getAttributeNames()).toEqual([]);
		expect(out.querySelector('span')!.getAttributeNames()).toEqual([]);
		expect(out.querySelector('ol')!.getAttributeNames()).toEqual(['start']);
	});
	it('drops elements outside the list but keeps their text', () => {
		const out = dom(
			sanitizeMarkdownHtml('<div><section><abbr>a</abbr><input value="x"><button>b</button><style>x{}</style></section></div>', 'chat')
		);
		expect(out.querySelector('*')).toBeNull();
		expect(out.textContent).toContain('a');
	});
	it('every element in the exact list survives', () => {
		const tags = 'p br strong em del blockquote ul ol li pre code h1 h2 h3 h4 h5 h6 hr table thead tbody tr th td span'.split(' ');
		const html = '<table><thead><tr><th>h</th></tr></thead><tbody><tr><td>c</td></tr></tbody></table><ul><li>l</li></ul><ol><li>o</li></ol>' +
			'<h1>1</h1><h2>2</h2><h3>3</h3><h4>4</h4><h5>5</h5><h6>6</h6><blockquote>q</blockquote><pre><code>c</code></pre><hr><p><br><strong>s</strong><em>e</em><del>d</del><span>x</span></p>';
		const out = dom(sanitizeMarkdownHtml(html, 'chat'));
		for (const t of tags) expect(out.querySelector(t), t).not.toBeNull();
	});
	it('ordered-list start must be an integer', () => {
		const d = render('7. a\n8. b');
		expect(d.querySelector('ol')!.getAttribute('start')).toBe('7');
		for (const bad of ['x', '1e3', '-1', '1.5', '99999999999999999999', 'javascript:1', '']) {
			const out = dom(sanitizeMarkdownHtml(`<ol start="${bad}"><li>a</li></ol>`, 'chat'));
			expect(out.querySelector('ol')!.hasAttribute('start'), JSON.stringify(bad)).toBe(false);
		}
	});
	it('code class is one generated language-* token', () => {
		const d = render('```ts\nconst a = 1;\n```');
		expect(d.querySelector('code')!.getAttribute('class')).toBe('language-ts');
		for (const bad of ['language-ts evil', 'evil', 'language-', 'language-a b', 'language-"x', 'language-<x>', 'Language-ts']) {
			const out = dom(sanitizeMarkdownHtml(`<pre><code class="${bad.replace(/"/g, '&quot;')}">c</code></pre>`, 'chat'));
			expect(out.querySelector('code')!.hasAttribute('class'), bad).toBe(false);
		}
	});
	it('a hostile fence info string cannot inject attributes or classes', () => {
		const d = render('```"><script>alert(1)</script> onclick=x\ncode\n```');
		expectInert(d);
		const cls = d.querySelector('code')!.getAttribute('class');
		expect(cls === null || /^language-[^\s]+$/.test(cls)).toBe(true);
	});
	it('span class is limited to the two generated names', () => {
		const out = dom(sanitizeMarkdownHtml('<span class="ref-restricted">a</span><span class="md-note">b</span><span class="evil">c</span>', 'chat'));
		const classes = Array.from(out.querySelectorAll('span')).map((s) => s.getAttribute('class'));
		expect(classes).toEqual(['ref-restricted', 'md-note', null]);
	});
});

describe('fenced code is inert', () => {
	it('shows markup, links, refs and attachments as text', () => {
		const refs: ChatRef[] = [{ workspace: 'docapp', ref: 'TASK-1', state: 'visible', title: 'T' }];
		const d = render('```\n<b>x</b> [l](https://e.example) [[TASK-1]] ![i](pad-attachment:x)\n```\n\n`[[TASK-1]]`', refs, 'docapp');
		expect(d.querySelectorAll('a,img,b')).toHaveLength(0);
		expect(d.querySelector('pre code')!.textContent).toContain('<b>x</b> [l](https://e.example) [[TASK-1]]');
		expect(d.textContent).toContain('[[TASK-1]]');
	});
});

describe('wiki refs', () => {
	it('keeps a ref inside code after a backslash-escaped backtick literal', () => {
		const source = "\\`a `[[TASK-1]]`";
		expect(extractRefs(source)).toHaveLength(1);
		const refs: ChatRef[] = [{ workspace: 'docapp', ref: 'TASK-1', state: 'visible', title: 'Task' }];
		const d = render(source, refs, 'docapp');
		expect(d.querySelector('code a')).toBeNull();
		expect(d.querySelector('code')!.textContent).toBe('[[TASK-1]]');
		expect(render('[[TASK-1]]', refs, 'docapp').querySelector('a')?.getAttribute('href')).toBe('/-/r/docapp/TASK-1');
	});

	const refs: ChatRef[] = [
		{ workspace: 'docapp', ref: 'TASK-1', state: 'visible', title: 'Fix the thing' },
		{ workspace: 'docapp', ref: 'TASK-2', state: 'restricted' },
		{ workspace: 'docapp', ref: 'TASK-3', state: 'unresolved' },
		{ workspace: 'other', ref: 'BUG-9', state: 'visible', title: 'Other ws' },
		{ workspace: 'docapp', ref: 'TASK-6', state: 'visible', title: '"><img src=x onerror=alert(1)><script>1</script>' }
	];
	it('visible becomes a /-/r link, restricted a title-less span, unresolved stays literal', () => {
		const d = render('a [[TASK-1]] b [[TASK-2]] c [[TASK-3]] d [[TASK-4]]', refs, 'docapp');
		const a = d.querySelector('a')!;
		expect(a.getAttribute('href')).toBe('/-/r/docapp/TASK-1');
		expect(a.textContent).toBe('TASK-1');
		expect(a.getAttribute('target')).toBe('_blank');
		expect(a.getAttribute('rel')).toBe('noopener noreferrer');
		const r = d.querySelector('span.ref-restricted')!;
		expect(r.textContent).toBe('restricted item');
		expect(r.getAttributeNames()).toEqual(['class']);
		expect(d.querySelectorAll('a')).toHaveLength(1);
		expect(d.textContent).toContain('[[TASK-3]]');
		expect(d.textContent).toContain('[[TASK-4]]');
	});
	it('uses the explicit workspace, and never the page workspace for a bare ref when none is given', () => {
		const d = render('[[other::BUG-9|shown]] [[TASK-1]]', refs, undefined);
		expect(d.querySelector('a')!.getAttribute('href')).toBe('/-/r/other/BUG-9');
		expect(d.querySelector('a')!.textContent).toBe('shown');
		expect(d.querySelectorAll('a')).toHaveLength(1);
		expect(d.textContent).toContain('[[TASK-1]]');
	});
	it('excludes code spans and fences', () => {
		const d = render('`[[TASK-1]]` and\n\n```\n[[TASK-1]]\n```\n\nreal [[TASK-1]]', refs, 'docapp');
		expect(d.querySelectorAll('a')).toHaveLength(1);
	});
	it('a hostile title is inert', () => {
		const d = render('[[TASK-6]]', refs, 'docapp');
		expectInert(d);
		expect(d.querySelector('img,script')).toBeNull();
		const a = d.querySelector('a')!;
		// DOMPurify may drop a title it finds suspicious; either way it is only attribute text.
		const title = a.getAttribute('title');
		expect(title === null || title.includes('<img src=x onerror=alert(1)>')).toBe(true);
		expect(a.getAttributeNames().every((n) => ['href', 'rel', 'target', 'title'].includes(n))).toBe(true);
	});
	it('a hostile display override is inert', () => {
		const d = render('[[TASK-1|<img src=x onerror=alert(1)>]]', refs, 'docapp');
		expectInert(d);
		expect(d.querySelector('img')).toBeNull();
		expect(d.querySelector('a')!.textContent).toBe('<img src=x onerror=alert(1)>');
	});
	it('a ref inside a url or link target leaves no markup behind', () => {
		const d = render('[x](https://e.example/[[TASK-1]]) and ![i](<https://e.example/[[TASK-1]]>)', refs, 'docapp');
		expectInert(d);
		expect(d.querySelectorAll('a').length).toBeLessThanOrEqual(1);
		expect(d.innerHTML).not.toContain('padref');
	});
	it('an indented code block stays inert text, with no link inside the pre', () => {
		const d = render('    [[TASK-1]]', refs, 'docapp');
		expect(d.querySelector('pre')).not.toBeNull();
		expect(d.querySelector('pre a')).toBeNull();
		expect(d.querySelector('pre')!.textContent).toContain('[[TASK-1]]');
	});
	it('a ref inside link text never nests an anchor', () => {
		const d = render('[see [[TASK-1]]](https://x.example) and https://x.example/[[TASK-1]]', refs, 'docapp');
		expect(d.querySelector('a a')).toBeNull();
		for (const a of Array.from(d.querySelectorAll('a'))) {
			expect(a.getAttribute('href')).not.toContain('/-/r/');
		}
		expect(d.textContent).toContain('[[TASK-1]]');
	});
	it('a ref inside emphasis and a heading still links', () => {
		const d = render('# **[[TASK-1]]**', refs, 'docapp');
		expect(d.querySelector('h1 strong a')).not.toBeNull();
	});
});

describe('prose', () => {
	it('keeps single newlines as line breaks', () => {
		const d = render('one\ntwo');
		expect(d.querySelector('br')).not.toBeNull();
	});
	it('renders tables, quotes, lists, emphasis, strike', () => {
		const d = render('| a | b |\n|---|---|\n| 1 | 2 |\n\n> q\n\n- x\n\n**b** _i_ ~~s~~');
		for (const s of ['table th', 'blockquote', 'ul li', 'strong', 'em', 'del']) expect(d.querySelector(s), s).not.toBeNull();
	});
	it('task list markers show as text, never as inputs', () => {
		const d = render('- [ ] todo\n- [x] done');
		expect(d.querySelector('input')).toBeNull();
		expect(d.textContent).toContain('todo');
	});
	it('escapes entities in text', () => {
		const d = render('1 < 2 & 3 > 2 "q"');
		expect(d.textContent!.trim()).toBe('1 < 2 & 3 > 2 "q"');
	});
});

describe('maximum-size message', () => {
	it('renders a 128 KiB body in full, quickly, with no truncation', () => {
		const line = 'word **bold** [l](https://example.com/p) `code` [[TASK-1]] <b>x</b>\n\n';
		let body = '';
		while (body.length < 128 * 1024 - 64) body += line;
		const t0 = performance.now();
		const html = renderChatMarkdown(body, [{ workspace: 'docapp', ref: 'TASK-1', state: 'visible', title: 't' }], 'docapp');
		const ms = performance.now() - t0;
		const count = body.split('\n\n').length - 1;
		const d = dom(html);
		expect(d.querySelectorAll('p')).toHaveLength(count);
		expect(d.querySelectorAll('a[href$="/TASK-1"]')).toHaveLength(count);
		expect(d.textContent).not.toMatch(/truncat|\.\.\./i);
		expect(ms).toBeLessThan(10_000);
	});
	it('a long unbroken url and a long line render in full', () => {
		const url = 'https://example.com/' + 'a'.repeat(50_000);
		const d = render(`[l](${url}) ${'z'.repeat(50_000)}`);
		expect(d.querySelector('a')!.getAttribute('href')).toBe(url);
		expect(d.textContent!.length).toBeGreaterThan(50_000);
	});
	it('deeply nested quotes and lists stay bounded', () => {
		const d = render('> '.repeat(500) + 'x\n\n' + '- '.repeat(200) + 'y');
		expect(d.textContent).toContain('x');
	});
});

describe('item renderer is untouched by Chat', () => {
	const fixture = (name: string) => ITEM_PIN_INPUTS[name];
	const itemSnap = () => Object.fromEntries(Object.keys(ITEM_PIN_INPUTS).map((k) => [k, [sanitizeMarkdownHtml(fixture(k)), renderMarkdown(fixture(k), [], 'ws')]]));
	it('chat render before item render leaves item output unchanged, and the reverse', () => {
		const baseline = itemSnap();
		renderChatMarkdown('[a](https://e.example) ![i](pad-attachment:x) `c`\n\n3. x', [], 'ws');
		sanitizeMarkdownHtml('<a href="https://e.example" target="_top" rel="opener">x</a>', 'chat');
		expect(itemSnap()).toEqual(baseline);
		// reverse order: an item render first must not change what Chat produces
		const chatA = renderChatMarkdown('[a](https://e.example/x) <b>x</b>', [], 'ws');
		itemSnap();
		renderMarkdown('<a href="https://e.example" target="_blank" rel="opener">x</a>', [], 'ws');
		expect(renderChatMarkdown('[a](https://e.example/x) <b>x</b>', [], 'ws')).toBe(chatA);
	});
	it('the item path still keeps its raw html, rel handling and data-less attributes', () => {
		const out = dom(sanitizeMarkdownHtml('<a href="https://e.example/" target="_blank" rel="opener nofollow">x</a><b>y</b>'));
		expect(out.querySelector('a')!.getAttribute('rel')).toBe('nofollow noopener noreferrer');
		expect(out.querySelector('b')).not.toBeNull();
	});
	it('the default profile argument is the item profile', () => {
		const s = '<a href="https://e.example/" title="t" class="c" id="i">x</a><abbr>a</abbr>';
		expect(sanitizeMarkdownHtml(s)).toBe(sanitizeMarkdownHtml(s, undefined));
		expect(sanitizeMarkdownHtml(s)).toContain('<abbr>');
	});
	it('chat hooks are not left registered on the shared sanitizer', () => {
		renderChatMarkdown('[a](https://e.example)', [], 'ws');
		const out = dom(sanitizeMarkdownHtml('<a href="https://e.example/" target="_top" rel="x">y</a>'));
		// the item profile's own behaviour: authored target kept, rel merged by enforceNoOpener
		expect(out.querySelector('a')!.getAttribute('target')).toBe('_top');
		expect(out.querySelector('a')!.getAttribute('rel')).toBe('x noopener noreferrer');
	});
	it('does not alter the global marked instance', () => {
		const html = renderMarkdown('<script>1</script>[a](javascript:1)\n\n![x](https://i.example/a.png)', [], 'ws');
		expect(html).toContain('<img');
	});
});
