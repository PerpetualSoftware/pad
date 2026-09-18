// BUG-3106: mermaid's palette follows the app's light/dark mode.
//
// Extracted from Editor.svelte so the two load-bearing decisions — which
// theme the current mode calls for, and which diagrams a mode change has to
// redraw — are reachable from a test. Editor.svelte imports both.

export type MermaidTheme = 'dark' | 'default';

/**
 * Which mermaid theme the app's CURRENT mode calls for.
 *
 * There is no theme store to read. Four components (+layout's initializer,
 * TopBar, Sidebar, YouSheet) each toggle `data-theme` on <html> and mirror it
 * to localStorage['pad-theme']; the ATTRIBUTE is the one thing all four agree
 * on, so it is what this reads.
 *
 * ABSENT MEANS DARK, and that asymmetry is the app's rather than ours:
 * +layout only ever SETS the attribute — to 'light' for a light preference,
 * or to the saved value when there is one — and leaves it off entirely for
 * the default. So the predicate has to be `!== 'light'`. Writing `=== 'dark'`
 * would send the default mode, which is the common case on a fresh browser,
 * down the light branch and draw every diagram on the wrong palette.
 */
export function mermaidThemeForMode(root: Element | null | undefined): MermaidTheme {
	if (!root) return 'dark';
	return root.getAttribute('data-theme') === 'light' ? 'default' : 'dark';
}

/**
 * The diagrams a theme change has to redraw, within one editor.
 *
 * Each mermaid code block's NodeView builds the same DOM shape: a
 * `.mermaid-wrapper` holding the source `code.language-mermaid` and the
 * `.mermaid-diagram` the SVG lands in. That shape is the only handle on the
 * live set — ProseMirror creates the NodeViews and nothing enumerates them.
 *
 * Two kinds of wrapper are SKIPPED, both deliberately:
 *   - one whose diagram carries `mermaid-error`, because it is showing the
 *     "Invalid Mermaid syntax" message and a re-render would replace that
 *     message with the identical message, flickering an error the user is
 *     already reading;
 *   - one whose source is empty or blank, which has nothing to draw.
 */
export function collectMermaidRerenders(
	root: Element,
): Array<{ source: string; target: HTMLElement }> {
	const out: Array<{ source: string; target: HTMLElement }> = [];
	for (const wrapper of root.querySelectorAll('.mermaid-wrapper')) {
		const diagram = wrapper.querySelector('.mermaid-diagram');
		const code = wrapper.querySelector('code.language-mermaid');
		if (!(diagram instanceof HTMLElement) || !code) continue;
		if (diagram.classList.contains('mermaid-error')) continue;
		const source = code.textContent ?? '';
		if (source.trim() === '') continue;
		out.push({ source, target: diagram });
	}
	return out;
}
