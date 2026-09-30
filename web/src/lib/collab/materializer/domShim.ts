// DOM shim for the headless materializer (TASK-2198). Must be evaluated before any editor
// module: tiptap-markdown's parser (run once by the Markdown extension at
// editor creation) and its HTML fallback serializer (getHTMLFromFragment ->
// document.implementation.createHTMLDocument, elementFromString ->
// window.DOMParser) need a DOM. linkedom: small, pure JS, no native deps.
import './polyfills';
import { parseHTML, DOMParser } from 'linkedom';

const g = globalThis as any;
// A host with a real DOM (jsdom under vitest, a browser) is left alone.
if (typeof g.document === 'undefined') {
	const { window, document } = parseHTML('<!doctype html><html><head></head><body></body></html>');
	g.window = window;
	g.document = document;
	// Spike run 1 finding: linkedom's parseFromString('<body>…</body>', 'text/html')
	// returns an EMPTY body unless the string carries an <html> element, where a
	// browser (and jsdom) builds html/head/body around the fragment. tiptap-
	// markdown's elementFromString (formatBlock of a block HTML node) depends on
	// the browser behaviour. Wrap such input in a document first.
	class ShimDOMParser extends (DOMParser as any) {
		parseFromString(s: string, type: string) {
			if (type === 'text/html' && !/<html[\s>]/i.test(s)) s = `<!doctype html><html>${s}</html>`;
			return super.parseFromString(s, type);
		}
	}
	g.DOMParser = ShimDOMParser;
	// linkedom's window ignores property assignment (measured: window.DOMParser stayed
	// linkedom's own after `window.DOMParser = …`), so expose it through a Proxy.
	g.window = new Proxy(window, { get: (t, k) => (k === 'DOMParser' ? ShimDOMParser : Reflect.get(t, k)) });
	for (const k of ['Node', 'Element', 'HTMLElement', 'DocumentFragment', 'Text', 'Comment', 'Event', 'CustomEvent', 'navigator']) {
		if (g[k] === undefined && (window as any)[k] !== undefined) g[k] = (window as any)[k];
	}
	// Spike run 1 finding: linkedom's document has no `implementation`, and tiptap's
	// getHTMLFromFragment calls document.implementation.createHTMLDocument()
	// (5 of 9,002 bodies in the spike's census errored on every engine without it). Supply it.
	if (!(document as any).implementation) {
		Object.defineProperty(document, 'implementation', {
			configurable: true,
			value: {
				createHTMLDocument(title = '') {
					return parseHTML(`<!doctype html><html><head><title>${title}</title></head><body></body></html>`).document;
				},
			},
		});
	}
	// Spike run 2 findings (the 4 remaining bodies after the two fixes above, all block
	// HTML tables serialized through prosemirror-model's DOMSerializer):
	//  (a) linkedom PREPENDS a new attribute (shared/attributes.js knownSiblings
	//      after the element start), so setAttribute(class) then (style) emits
	//      style first; browsers and jsdom keep insertion order.
	//  (b) prosemirror-model sets `style` via dom.style.cssText; the CSSOM (jsdom
	//      and browsers) re-serializes it as `prop: value;`, linkedom stores the
	//      raw string. Only declaration FORMAT is normalized here (lowercased
	//      property, `: `, trailing `;`, `; ` separator) — not values (a browser
	//      would also rewrite e.g. colours), which would need a CSS engine.
	const probe = document.createElement('div') as any;
	let EP = Object.getPrototypeOf(probe);
	while (EP && !Object.prototype.hasOwnProperty.call(EP, 'setAttribute')) EP = Object.getPrototypeOf(EP);
	const origSet = EP.setAttribute;
	EP.setAttribute = function (this: any, name: string, value: string) {
		if (this.hasAttribute(name)) return origSet.call(this, name, value);
		const prev: Array<[string, string]> = Array.from(this.attributes as ArrayLike<any>).map((a: any) => [a.name, a.value]);
		if (prev.length === 0) return origSet.call(this, name, value);
		for (const [n] of prev) this.removeAttribute(n);
		origSet.call(this, name, value);
		for (let i = prev.length - 1; i >= 0; i--) origSet.call(this, prev[i][0], prev[i][1]);
	};
	const SP = Object.getPrototypeOf(probe.style);
	const cssDesc = Object.getOwnPropertyDescriptor(SP, 'cssText');
	if (cssDesc?.set) {
		Object.defineProperty(SP, 'cssText', {
			configurable: true,
			get: cssDesc.get,
			set(this: any, value: string) {
				const decls = String(value)
					.split(';')
					.map((d) => d.trim())
					.filter(Boolean)
					.map((d) => {
						const c = d.indexOf(':');
						return c < 0 ? null : `${d.slice(0, c).trim().toLowerCase()}: ${d.slice(c + 1).trim()};`;
					})
					.filter(Boolean);
				cssDesc.set!.call(this, decls.join(' '));
			},
		});
	}
	// Spike run 3 finding: linkedom serializes U+00A0 in text as `&#160;`; the HTML
	// serialization algorithm (browsers, jsdom) emits `&nbsp;`. A literal
	// "&#160;" typed as text serializes as "&amp;#160;", so the substring
	// rewrite cannot touch it.
	for (const prop of ['innerHTML', 'outerHTML']) {
		let P = Object.getPrototypeOf(probe);
		while (P && !Object.prototype.hasOwnProperty.call(P, prop)) P = Object.getPrototypeOf(P);
		const d = P && Object.getOwnPropertyDescriptor(P, prop);
		if (d?.get) {
			Object.defineProperty(P, prop, {
				configurable: true,
				get(this: any) {
					return (d.get!.call(this) as string).replace(/&#160;/g, '&nbsp;');
				},
				set: d.set,
			});
		}
	}
	if (g.navigator === undefined) g.navigator = { userAgent: 'materializer', platform: '' };
	// (window.navigator: linkedom provides one; not assigned through the Proxy.)
}
