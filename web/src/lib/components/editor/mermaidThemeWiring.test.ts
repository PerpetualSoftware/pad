import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync } from 'node:fs';
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
// The loader, queue and applied-theme tracker moved here from Editor.svelte's
// module block (TASK-2248 U2), so the share page renders through the same one.
const renderSrc = readFileSync(
	fileURLToPath(new URL('./mermaidRender.ts', import.meta.url)),
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
		for (const src of [editorSrc, renderSrc]) {
			expect(/theme:\s*['"]dark['"]/.test(src)).toBe(false);
			expect(/theme:\s*['"]default['"]/.test(src)).toBe(false);
		}
	});

	it('derives the theme from the document and the OS preference', () => {
		expect(renderSrc.includes('mermaidThemeForMode(document.documentElement, prefersLightMode())')).toBe(true);
		expect(renderSrc.includes("matchMedia('(prefers-color-scheme: light)')")).toBe(true);
		// The editor compares the mode against the SHARED tracker, not a copy.
		expect(has('currentMermaidTheme() === mermaidAppliedTheme()')).toBe(true);
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
	// not be per-instance. They are top-level `let`s of mermaidRender.ts, ahead
	// of its first function, and the per-diagram source map stays in Editor's
	// `<script module>` (TASK-2248 U2).
	it('keeps the mermaid module, queue and applied theme at module scope', () => {
		const head = renderSrc.slice(0, renderSrc.search(/^(export )?(async )?function /m));
		for (const decl of ['let mermaidMod', 'let renderQueue', 'let appliedMermaidTheme']) {
			expect(head.includes(decl), decl).toBe(true);
		}
		const moduleBlock = editorSrc.slice(
			editorSrc.indexOf('<script lang="ts" module>'),
			editorSrc.indexOf('</script>'),
		);
		expect(moduleBlock.includes('mermaidSources')).toBe(true);
	});

	// TASK-2248 U2: a second loader anywhere would own a second tracker over the
	// one global config, which is exactly the round-1 P2 defect across files.
	it('loads mermaid in exactly one module', () => {
		const srcRoot = fileURLToPath(new URL('../../../', import.meta.url));
		const loaders = (readdirSync(srcRoot, { recursive: true }) as string[])
			.filter((f) => /\.(ts|svelte)$/.test(f) && !/\.test\.ts$/.test(f))
			.filter((f) => /import\(\s*['"]mermaid['"]\s*\)|from\s+['"]mermaid['"]/.test(readFileSync(srcRoot + f, 'utf8')))
			.map((f) => f.replace(/\\/g, '/'));
		expect(loaders).toEqual(['lib/components/editor/mermaidRender.ts']);
	});
});

describe('BUG-3106 mermaidTheme.ts', () => {
	// The module has no business reading the DOM's text: that is the P1 defect
	// expressed as a property of the file.
	it('never reads textContent', () => {
		expect(moduleSrc.replace(/\/\*[\s\S]*?\*\//g, '')).not.toContain('textContent');
	});
});
