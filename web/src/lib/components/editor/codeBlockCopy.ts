// The plain (non-mermaid) code block's NodeView and its hover "Copy" button.
//
// Extracted from Editor.svelte so the copy path is reachable from a test
// through the SAME factory the editor mounts; Editor.svelte imports it.

import { copyToClipboard } from '$lib/utils/clipboard';

/**
 * The code block's text, read from the ProseMirror MODEL at the moment of the
 * call — or null when the block can no longer be located.
 *
 * NOT the DOM. The `<code>` element is the block's contentDOM, and
 * ProseMirror renders DECORATIONS into a contentDOM beside the text. A remote
 * collaborator's caret is a widget decoration whose label is a real text node
 * holding their display name, so `code.textContent` returned the code with a
 * peer's name spliced in at their caret — reproduced in a browser as
 * "const ansE2E Adminwer = 42;" (BUG-3111; the same mechanism BUG-3106 fixed
 * for mermaid source).
 *
 * Read at CALL time through getPos rather than from a node captured when the
 * view was built: the block's text changes after creation, and this view has
 * no update() to be told about it. Resolving through getPos also means a
 * view ProseMirror has since discarded answers null instead of stale text.
 */
export function codeBlockModelText(
	view: { state: { doc: { nodeAt(pos: number): { type: { name: string }; textContent: string } | null } } },
	getPos: (() => number | undefined) | undefined,
): string | null {
	const pos = typeof getPos === 'function' ? getPos() : undefined;
	if (typeof pos !== 'number') return null;
	const node = view.state.doc.nodeAt(pos);
	if (!node || node.type.name !== 'codeBlock') return null;
	return node.textContent;
}

/**
 * A hover-to-reveal "Copy" button. `getText` is called on each click; null
 * means there is nothing trustworthy to copy, which is reported as a failure
 * rather than copying an empty string over the user's clipboard.
 */
export function buildCopyButton(getText: () => string | null): HTMLButtonElement {
	const btn = document.createElement('button');
	btn.type = 'button';
	btn.className = 'code-copy-btn';
	btn.setAttribute('contenteditable', 'false');
	btn.setAttribute('aria-label', 'Copy code');
	btn.title = 'Copy';
	btn.textContent = 'Copy';
	// mousedown + preventDefault avoids stealing focus / clobbering the selection
	btn.addEventListener('mousedown', (e) => e.preventDefault());
	btn.addEventListener('click', async (e) => {
		e.preventDefault();
		e.stopPropagation();
		const text = getText();
		const ok = text !== null && (await copyToClipboard(text));
		const prev = btn.textContent;
		btn.textContent = ok ? 'Copied' : 'Failed';
		btn.classList.toggle('copied', ok);
		setTimeout(() => {
			btn.textContent = prev;
			btn.classList.remove('copied');
		}, 1200);
	});
	return btn;
}

/**
 * The NodeView for a non-mermaid code block: `<pre class="code-block">` with
 * the `<code>` contentDOM and a Copy button that copies the MODEL text.
 */
export function createPlainCodeBlockView(props: {
	node: { attrs: { language?: string | null } };
	view: Parameters<typeof codeBlockModelText>[0];
	getPos: (() => number | undefined) | undefined;
}): { dom: HTMLElement; contentDOM: HTMLElement } {
	const { node, view, getPos } = props;
	const lang = node.attrs.language;
	const pre = document.createElement('pre');
	pre.classList.add('code-block');
	const code = document.createElement('code');
	if (lang) code.classList.add(`language-${lang}`);
	pre.appendChild(code);
	pre.appendChild(buildCopyButton(() => codeBlockModelText(view, getPos)));
	return { dom: pre, contentDOM: code };
}
