import { describe, it, expect } from 'vitest';
import { mermaidThemeForMode, collectMermaidRerenders } from './mermaidTheme';

// BUG-3106: initMermaid passed theme:'dark' unconditionally, so every diagram
// was drawn on the dark palette in light mode.
//
// Round 1 of review reshaped both functions, so these tests are written
// against the corrected contract rather than the first attempt's.

function html(attr: string | null): Element {
	const el = document.createElement('html');
	if (attr !== null) el.setAttribute('data-theme', attr);
	return el;
}

describe('mermaidThemeForMode', () => {
	// The original defect: light mode drew the dark palette.
	it('explicit light asks for the light palette, whatever the OS prefers', () => {
		expect(mermaidThemeForMode(html('light'), false)).toBe('default');
		expect(mermaidThemeForMode(html('light'), true)).toBe('default');
	});

	// data-theme="dark" wins over an OS light preference, because app.css
	// excludes it from the light block via :not([data-theme="dark"]).
	it('explicit dark asks for the dark palette, whatever the OS prefers', () => {
		expect(mermaidThemeForMode(html('dark'), true)).toBe('dark');
		expect(mermaidThemeForMode(html('dark'), false)).toBe('dark');
	});

	// THE ROUND-1 FINDING. The first version returned 'dark' for an absent
	// attribute on the claim that absent means dark. app.css:699 says
	// otherwise: `@media (prefers-color-scheme: light) {
	// :root:not([data-theme="dark"]) { ...light... } }`, so absent + OS light
	// renders the app light. This leg fails against that first version.
	it('absent attribute follows the OS preference', () => {
		expect(mermaidThemeForMode(html(null), true)).toBe('default');
		expect(mermaidThemeForMode(html(null), false)).toBe('dark');
	});

	// An unrecognised value is not a third palette: it matches
	// :not([data-theme="dark"]), so it behaves exactly like absent.
	it('an unrecognised value follows the OS preference, like absent', () => {
		for (const attr of ['solarized', '']) {
			expect(mermaidThemeForMode(html(attr), true)).toBe('default');
			expect(mermaidThemeForMode(html(attr), false)).toBe('dark');
		}
	});

	it('survives no root (SSR), defaulting to dark', () => {
		expect(mermaidThemeForMode(null, false)).toBe('dark');
		expect(mermaidThemeForMode(undefined, false)).toBe('dark');
	});
});

describe('collectMermaidRerenders', () => {
	// The DOM shape the NodeView builds. `text` is what lands INSIDE the
	// contentDOM <code> — which in the real editor includes ProseMirror
	// decorations, not just the model text. `model` is what the NodeView
	// published for this diagram; undefined means it published nothing.
	function wrapper(opts: { text: string; model?: string; error?: boolean }): string {
		return `
			<div class="mermaid-wrapper">
				<pre class="code-block mermaid-source"><code class="language-mermaid">${opts.text}</code></pre>
				<div class="mermaid-diagram${opts.error ? ' mermaid-error' : ''}" data-model="${
					opts.model === undefined ? '' : encodeURIComponent(opts.model)
				}"></div>
			</div>`;
	}

	function root(inner: string): HTMLElement {
		const el = document.createElement('div');
		el.innerHTML = inner;
		return el;
	}

	// Stands in for the WeakMap the component keeps: returns the model source
	// the NodeView published, or undefined when it published none.
	const modelSource = (d: HTMLElement) => {
		const raw = d.getAttribute('data-model');
		return raw ? decodeURIComponent(raw) : undefined;
	};

	it('returns each renderable diagram with its own source', () => {
		const r = root(
			wrapper({ text: 'graph TD; A-->B;', model: 'graph TD; A-->B;' }) +
				wrapper({ text: 'sequenceDiagram', model: 'sequenceDiagram' }),
		);
		const got = collectMermaidRerenders(r, modelSource);
		expect(got.map((g) => g.source)).toEqual(['graph TD; A-->B;', 'sequenceDiagram']);
		// Each target is the diagram from its OWN wrapper — a pairing bug
		// would redraw one diagram's source into another's node.
		const wrappers = r.querySelectorAll('.mermaid-wrapper');
		expect(got[0].target).toBe(wrappers[0].querySelector('.mermaid-diagram'));
		expect(got[1].target).toBe(wrappers[1].querySelector('.mermaid-diagram'));
	});

	// THE ROUND-1 P1. The `code` element is the NodeView's contentDOM, so
	// ProseMirror renders decorations into it — a remote collaborator's caret
	// is a widget whose label carries a text node with the peer's display
	// name. The first version read `code.textContent` and so would have
	// rendered `graph TD; A-->B;Dave`, which mermaid rejects; the resulting
	// error class then made the breakage stick.
	it('ignores decoration text in the contentDOM and uses the model source', () => {
		const polluted =
			'graph TD; A-->B;<span class="collaboration-carets__caret">' +
			'<div class="collaboration-carets__label">Dave</div></span>';
		const r = root(wrapper({ text: polluted, model: 'graph TD; A-->B;' }));

		// The fixture really does reproduce the pollution, or this leg would
		// be asserting against a DOM that never had the problem.
		expect(r.querySelector('code')!.textContent).toContain('Dave');

		const got = collectMermaidRerenders(r, modelSource);
		expect(got).toHaveLength(1);
		expect(got[0].source).toBe('graph TD; A-->B;');
		expect(got[0].source).not.toContain('Dave');
	});

	it('skips a diagram the NodeView published no source for', () => {
		const r = root(wrapper({ text: 'graph TD; A-->B;' })); // model undefined
		expect(collectMermaidRerenders(r, modelSource)).toEqual([]);
	});

	it('skips a diagram showing a syntax error', () => {
		const r = root(wrapper({ text: 'nope', model: 'nope', error: true }));
		expect(collectMermaidRerenders(r, modelSource)).toEqual([]);
	});

	it('skips an empty or blank model source', () => {
		const r = root(
			wrapper({ text: '', model: '' }) + wrapper({ text: ' ', model: '   \n  ' }),
		);
		expect(collectMermaidRerenders(r, modelSource)).toEqual([]);
	});

	// The error leg must skip for the RIGHT reason: a wrapper identical except
	// for the error class is collected, so the empty result above is the class
	// and not the fixture.
	it('collects the same wrapper once the error class is gone', () => {
		const r = root(wrapper({ text: 'graph TD; A-->B;', model: 'graph TD; A-->B;', error: true }));
		expect(collectMermaidRerenders(r, modelSource)).toEqual([]);
		r.querySelector('.mermaid-diagram')!.classList.remove('mermaid-error');
		expect(collectMermaidRerenders(r, modelSource)).toHaveLength(1);
	});

	it('ignores non-mermaid code blocks', () => {
		const r = root(`
			<pre class="code-block"><code class="language-go">func main() {}</code></pre>`);
		expect(collectMermaidRerenders(r, modelSource)).toEqual([]);
	});
});
