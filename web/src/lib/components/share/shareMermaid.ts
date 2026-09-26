/**
 * Mermaid diagrams on the public share page (TASK-2248 U2, audit cluster
 * C120). The owner sees a rendered diagram in the app, and a share viewer used
 * to see the raw `graph TD …` source.
 *
 * Renders through `queueMermaidRender`, which is the app's ONE mermaid
 * loader: the same lazy import, the same serialized queue, `securityLevel:
 * 'strict'` and the same mode-following palette. It does not load its own,
 * because mermaid's config is one global.
 *
 * The code block stays VISIBLE until the diagram is actually drawn, and only
 * then is hidden, so a mermaid that never settles (a stalled import, a
 * pathological diagram) leaves the reader the code rather than nothing (codex
 * round 1 on TASK-2248 U2). A parse failure, or a failure to load mermaid at
 * all, removes the empty diagram. The reader always gets one of the two.
 */
import type { Attachment } from 'svelte/attachments';
import { queueMermaidRender } from '$lib/components/editor/mermaidRender';

const DONE = 'shareMermaid';

/** Draw every not-yet-drawn ```mermaid block under `root`. */
export function renderMermaidBlocks(root: ParentNode): void {
	for (const code of Array.from(root.querySelectorAll('pre > code.language-mermaid'))) {
		const pre = code.parentElement as HTMLElement;
		if (pre.dataset[DONE]) continue;
		pre.dataset[DONE] = '1';
		const source = code.textContent ?? '';
		if (!source.trim()) continue;
		const diagram = document.createElement('div');
		diagram.className = 'mermaid-diagram share-mermaid';
		pre.before(diagram);
		queueMermaidRender(
			source,
			diagram,
			(target) => target.remove(),
			() => {
				pre.hidden = true;
			}
		);
	}
}

/**
 * `{@attach mermaidBlocks(html)}` on the element whose `{@html}` holds the
 * body. Passing the html makes a changed body a new attachment, so it runs
 * again. The scan waits one microtask so it reads the body as rendered.
 */
export function mermaidBlocks(html: string): Attachment<HTMLElement> {
	return (node) => {
		if (!html.includes('language-mermaid')) return;
		queueMicrotask(() => renderMermaidBlocks(node));
	};
}
