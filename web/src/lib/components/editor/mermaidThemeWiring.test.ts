import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

// BUG-3106 review round 1, the test nit: the unit's behaviour tests exercise
// two pure functions, so restoring the literal `theme: 'dark'` inside
// `initMermaid` left the whole suite green. A direct-call test vouches for a
// component, not for its binding (CONVE-19), and the binding is where the
// defect lived. This pins the binding.
//
// Source-level rather than by mounting Editor.svelte: the component pulls in
// tiptap, ProseMirror and the collab provider, no sibling spec mounts it, and
// the repo already uses this shape for guards of exactly this kind. The
// assertions are deliberately few and anchored on identifiers rather than on
// formatting, so reflowing the file cannot turn them red.

const editorSrc = readFileSync(
	fileURLToPath(new URL('./Editor.svelte', import.meta.url)),
	'utf8',
);
const moduleSrc = readFileSync(
	fileURLToPath(new URL('./mermaidTheme.ts', import.meta.url)),
	'utf8',
);

// `toContain` on a 1900-line component prints the entire file on failure,
// which buries the assertion that failed. These report booleans instead; the
// test name says what was expected.
const has = (needle: string) => editorSrc.includes(needle);
const hasRe = (re: RegExp) => re.test(editorSrc);

describe('BUG-3106 wiring in Editor.svelte', () => {
	// THE DEFECT ITSELF. This is the line that shipped, and the one a careless
	// revert would restore.
	it('does not hardcode a mermaid theme', () => {
		expect(hasRe(/theme:\s*['"]dark['"]/)).toBe(false);
		expect(hasRe(/theme:\s*['"]default['"]/)).toBe(false);
	});

	it('derives the theme from the document and the OS preference', () => {
		expect(has('mermaidThemeForMode(document.documentElement, prefersLightMode())')).toBe(true);
		expect(has("matchMedia('(prefers-color-scheme: light)')")).toBe(true);
	});

	it('arms a data-theme observer and disconnects it on destroy', () => {
		expect(has("attributeFilter: ['data-theme']")).toBe(true);
		expect(has('themeObserver?.disconnect()')).toBe(true);
	});

	// The OS-preference transition is invisible to the MutationObserver, so
	// this listener is not redundant with it — and an added listener with no
	// matching removal is a leak across editor remounts.
	it('adds AND removes the OS-preference listener', () => {
		expect(has("addEventListener('change', prefersLightHandler)")).toBe(true);
		expect(has("removeEventListener('change', prefersLightHandler)")).toBe(true);
	});

	// Round 1 P1: the theme re-render must take the model source the NodeView
	// published, never the contentDOM's text, which carries decorations.
	it('feeds the theme re-render from the model-source map', () => {
		expect(has('collectMermaidRerenders(root,')).toBe(true);
		expect(has('mermaidSources.get(d)')).toBe(true);
		// Published on create AND on update, or a diagram edited after mount
		// would redraw from a stale source.
		expect(editorSrc.match(/mermaidSources\.set\(diagram,/g) ?? []).toHaveLength(2);
	});

	// Round 1 P2: mermaid's config and render are global, so the trackers must
	// not be per-instance. `<script module>` is what makes them shared.
	it('keeps the mermaid module, queue and applied theme at module scope', () => {
		const moduleBlock = editorSrc.slice(
			editorSrc.indexOf('<script lang="ts" module>'),
			editorSrc.indexOf('</script>'),
		);
		expect(moduleBlock).not.toBe('');
		for (const decl of ['mermaidMod', 'renderQueue', 'appliedMermaidTheme', 'mermaidSources']) {
			expect(moduleBlock).toContain(decl);
		}
	});
});

describe('BUG-3106 mermaidTheme.ts', () => {
	// The module has no business reading the DOM's text: that is the P1 defect
	// expressed as a property of the file.
	it('never reads textContent', () => {
		expect(moduleSrc.replace(/\/\*[\s\S]*?\*\//g, '')).not.toContain('textContent');
	});
});
