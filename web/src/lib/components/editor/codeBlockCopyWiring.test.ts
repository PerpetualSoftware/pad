import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

// BUG-3111 wiring. The behaviour spec next door drives `createPlainCodeBlockView`
// through a real editor, which vouches for the factory and says nothing about
// whether Editor.svelte mounts it (CONVE-19). Restoring the old inline NodeView
// — a `<code>` handed to a copy button that reads its textContent — would leave
// that spec green. This pins the binding.
//
// Source-level, like mermaidThemeWiring.test.ts, for the same reason: mounting
// Editor.svelte pulls in the collab provider and nothing else mounts it. The
// assertions are anchored on identifiers, not formatting.

const editorSrc = readFileSync(
	fileURLToPath(new URL('./Editor.svelte', import.meta.url)),
	'utf8',
);

describe('BUG-3111 wiring in Editor.svelte', () => {
	it('mounts the shared plain code block view, with the view and getPos it reads the model through', () => {
		expect(editorSrc.includes("import { createPlainCodeBlockView } from './codeBlockCopy'")).toBe(true);
		expect(editorSrc.includes('createPlainCodeBlockView({ node, view, getPos })')).toBe(true);
	});

	// THE DEFECT ITSELF: a copy button built in Editor.svelte, which is where
	// the DOM read lived.
	it('does not build its own copy button', () => {
		expect(/function\s+buildCopyButton\b/.test(editorSrc)).toBe(false);
		expect(/\bbuildCopyButton\s*\(/.test(editorSrc)).toBe(false);
		// The button's CSS rightly stays in Editor.svelte; its CONSTRUCTION must not.
		expect(editorSrc.includes("className = 'code-copy-btn'")).toBe(false);
	});
});
