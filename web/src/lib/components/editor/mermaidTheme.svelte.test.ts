import { describe, it, expect } from 'vitest';
import { mermaidThemeForMode, collectMermaidRerenders } from './mermaidTheme';

// BUG-3106: initMermaid passed theme:'dark' unconditionally, so every diagram
// was drawn on the dark palette in light mode.

function html(attr: string | null): Element {
	const el = document.createElement('html');
	if (attr !== null) el.setAttribute('data-theme', attr);
	return el;
}

describe('mermaidThemeForMode', () => {
	// The bug itself: this is the case that was wrong, and it is the one
	// assertion that fails against the unfixed code.
	it('light mode asks for the light palette', () => {
		expect(mermaidThemeForMode(html('light'))).toBe('default');
	});

	it('explicit dark mode asks for the dark palette', () => {
		expect(mermaidThemeForMode(html('dark'))).toBe('dark');
	});

	// The asymmetry worth pinning. +layout only ever SETS data-theme — for a
	// light preference, or from a saved value — and leaves it OFF for the
	// default, which is dark. A `=== 'dark'` predicate would read absent as
	// light and break the common case on a fresh browser, so this is the leg
	// that discriminates between the correct predicate and the plausible
	// wrong one.
	it('treats an ABSENT attribute as dark, not as light', () => {
		expect(mermaidThemeForMode(html(null))).toBe('dark');
	});

	// An unrecognised value is the app's default mode, not a third palette.
	it('treats an unrecognised value as dark', () => {
		expect(mermaidThemeForMode(html('solarized'))).toBe('dark');
		expect(mermaidThemeForMode(html(''))).toBe('dark');
	});

	it('survives being called with no root (SSR)', () => {
		expect(mermaidThemeForMode(null)).toBe('dark');
		expect(mermaidThemeForMode(undefined)).toBe('dark');
	});
});

describe('collectMermaidRerenders', () => {
	function wrapper(opts: { source: string; error?: boolean }): string {
		return `
			<div class="mermaid-wrapper">
				<pre class="code-block mermaid-source"><code class="language-mermaid">${opts.source}</code></pre>
				<div class="mermaid-diagram${opts.error ? ' mermaid-error' : ''}"></div>
			</div>`;
	}

	function root(inner: string): HTMLElement {
		const el = document.createElement('div');
		el.innerHTML = inner;
		return el;
	}

	it('returns each renderable diagram with its own source', () => {
		const r = root(wrapper({ source: 'graph TD; A-->B;' }) + wrapper({ source: 'sequenceDiagram' }));
		const got = collectMermaidRerenders(r);
		expect(got.map((g) => g.source)).toEqual(['graph TD; A-->B;', 'sequenceDiagram']);
		// Each target is the diagram node from its OWN wrapper — a pairing bug
		// would redraw one diagram's source into another's node.
		const wrappers = r.querySelectorAll('.mermaid-wrapper');
		expect(got[0].target).toBe(wrappers[0].querySelector('.mermaid-diagram'));
		expect(got[1].target).toBe(wrappers[1].querySelector('.mermaid-diagram'));
	});

	it('skips a diagram showing a syntax error', () => {
		const r = root(wrapper({ source: 'not a diagram', error: true }));
		expect(collectMermaidRerenders(r)).toEqual([]);
	});

	it('skips an empty or blank source', () => {
		const r = root(wrapper({ source: '' }) + wrapper({ source: '   \n  ' }));
		expect(collectMermaidRerenders(r)).toEqual([]);
	});

	// The error leg must skip for the RIGHT reason: a wrapper identical except
	// for the error class is collected, so the empty result above is the class
	// and not the fixture.
	it('collects the same wrapper once the error class is gone', () => {
		const r = root(wrapper({ source: 'graph TD; A-->B;', error: true }));
		expect(collectMermaidRerenders(r)).toEqual([]);
		r.querySelector('.mermaid-diagram')!.classList.remove('mermaid-error');
		expect(collectMermaidRerenders(r)).toHaveLength(1);
	});

	it('ignores non-mermaid code blocks', () => {
		const r = root(`
			<pre class="code-block"><code class="language-go">func main() {}</code></pre>`);
		expect(collectMermaidRerenders(r)).toEqual([]);
	});
});
