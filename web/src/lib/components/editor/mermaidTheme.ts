// BUG-3106: mermaid's palette follows the app's light/dark mode.
//
// Extracted from Editor.svelte so the decisions worth testing are reachable
// from a test. Editor.svelte imports all of this.

export type MermaidTheme = 'dark' | 'default';

/**
 * Which mermaid theme the app's CURRENT mode calls for.
 *
 * There is no theme store. FIVE places write `data-theme` on <html> and
 * mirror it to localStorage['pad-theme'] — `+layout.svelte`'s mount
 * initializer, `TopBar`, `Sidebar`, `YouSheet`, and the workspace settings
 * page — so the attribute is the only thing they all agree on.
 *
 * THE ATTRIBUTE IS NOT THE WHOLE ANSWER, and an earlier version of this
 * function claiming "absent means dark" was wrong. `app.css` decides the
 * palette with
 *
 *     @media (prefers-color-scheme: light) {
 *       :root:not([data-theme="dark"]) { ...light... }
 *     }
 *
 * so an ABSENT attribute renders LIGHT when the OS prefers light, and dark
 * otherwise. `+layout` papers over that at mount by writing
 * `data-theme="light"` if the media query matches THEN, but it writes nothing
 * when the OS is dark — so the attribute stays absent, and a later OS switch
 * to light flips the CSS with no attribute mutation for anyone to observe.
 *
 * Hence the three-way read, in the CSS's own order of precedence:
 *   - `dark`  -> dark unconditionally (the `:not([data-theme="dark"])` guard
 *                excludes it from the light block even under an OS light
 *                preference)
 *   - `light` -> light unconditionally (`[data-theme="light"]` is a
 *                standalone rule, not inside the media query)
 *   - absent, or any other value -> whatever the OS prefers
 *
 * `prefersLight` is injected rather than read here, so a test can drive the
 * OS preference; Editor.svelte passes a matchMedia read.
 */
export function mermaidThemeForMode(
	root: Element | null | undefined,
	prefersLight: boolean,
): MermaidTheme {
	const attr = root?.getAttribute('data-theme');
	if (attr === 'dark') return 'dark';
	if (attr === 'light') return 'default';
	return prefersLight ? 'default' : 'dark';
}

/**
 * The diagrams a theme change has to redraw, within one editor.
 *
 * Each mermaid code block's NodeView builds the same DOM shape: a
 * `.mermaid-wrapper` holding the source `<code>` and the `.mermaid-diagram`
 * the SVG lands in. That shape is the only handle on the live set —
 * ProseMirror creates the NodeViews and nothing enumerates them.
 *
 * THE SOURCE COMES FROM `getSource`, NOT FROM THE DOM, and that parameter is
 * the whole point. An earlier version read `code.textContent`, which is wrong
 * because that `code` element is the NodeView's `contentDOM`, so ProseMirror
 * renders DECORATIONS into it alongside the text. A remote collaborator's
 * caret is a widget decoration whose label holds a text node with the peer's
 * display name (CollaborationCaret's default `render`, which Editor.svelte
 * does not override), so `textContent` for a block with a peer's cursor in it
 * reads `graph TD; A-->B;Dave`. mermaid throws, the render path swaps the SVG
 * for "Invalid Mermaid syntax", and the error-skip below then makes that
 * STICK until the source is edited — the two rules combining into a
 * persistent broken diagram.
 *
 * Both of the NodeView's own render paths read the MODEL
 * (`node.textContent` / `updatedNode.textContent`), so the model is the
 * consistent choice as well as the correct one. `getSource` returns undefined
 * for a diagram the caller has no model source for, and such a wrapper is
 * skipped rather than guessed at.
 *
 * Also skipped, deliberately: a diagram carrying `mermaid-error`. It is
 * showing the "Invalid Mermaid syntax" message, and a re-render would replace
 * that message with the identical message, flickering an error the reader is
 * already looking at.
 */
export function collectMermaidRerenders(
	root: Element,
	getSource: (diagram: HTMLElement) => string | undefined,
): Array<{ source: string; target: HTMLElement }> {
	const out: Array<{ source: string; target: HTMLElement }> = [];
	for (const wrapper of root.querySelectorAll('.mermaid-wrapper')) {
		const diagram = wrapper.querySelector('.mermaid-diagram');
		if (!(diagram instanceof HTMLElement)) continue;
		if (diagram.classList.contains('mermaid-error')) continue;
		const source = getSource(diagram);
		if (source === undefined || source.trim() === '') continue;
		out.push({ source, target: diagram });
	}
	return out;
}
