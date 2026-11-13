<script lang="ts">
	import { renderChatMarkdown } from '$lib/chat/markdown';
	import { copyToClipboard } from '$lib/utils/clipboard';
	import type { ChatRef } from '$lib/types';

	// A confirmed message body as sanitised markdown (TASK-4315 PR 2a). The HTML
	// comes only from renderChatMarkdown, which ends in exactly one pass through
	// the Chat profile of sanitizeMarkdownHtml. Pending bodies, excerpts and reply
	// previews stay escaped plain text and do not use this component.
	//
	// Two things are added AFTER sanitising, both by the DOM API and never from
	// message text: a "Copy code" button after each fenced block (outside the
	// sanitised HTML, so a message cannot style or script it), and the fixed
	// "Attachment unavailable." text that replaces an image that fails to load.
	let {
		body,
		refs,
		ctxWorkspace
	}: {
		body: string;
		/** The thread's ref verdicts; absent means every ref stays literal. */
		refs?: ReadonlyMap<string, ChatRef>;
		ctxWorkspace?: string;
	} = $props();

	let html = $derived(renderChatMarkdown(body, refs ? [...refs.values()] : [], ctxWorkspace));

	function enhance(node: HTMLElement, _html: string) {
		let added: HTMLElement[] = [];
		const timers = new Set<ReturnType<typeof setTimeout>>();
		let destroyed = false;
		function decorate() {
			for (const b of added) b.remove();
			added = [];
			for (const pre of Array.from(node.querySelectorAll('pre'))) {
				const btn = document.createElement('button');
				btn.type = 'button';
				btn.className = 'md-copy';
				btn.textContent = 'Copy code';
				btn.addEventListener('click', async () => {
					const ok = await copyToClipboard(pre.textContent ?? '');
					if (destroyed) return;
					btn.textContent = ok ? 'Copied' : 'Copy failed';
					const t = setTimeout(() => {
						timers.delete(t);
						btn.textContent = 'Copy code';
					}, 1500);
					timers.add(t);
				});
				pre.after(btn);
				added.push(btn);
			}
		}
		// Image errors do not bubble: listen in the capture phase.
		function onError(e: Event) {
			const img = e.target;
			if (!(img instanceof HTMLImageElement)) return;
			const note = document.createElement('span');
			note.className = 'md-note';
			note.textContent = 'Attachment unavailable.';
			img.replaceWith(note);
		}
		node.addEventListener('error', onError, true);
		decorate();
		return {
			update: decorate,
			destroy() {
				destroyed = true;
				node.removeEventListener('error', onError, true);
				for (const t of timers) clearTimeout(t);
			}
		};
	}
</script>

<!-- The html is sanitised by renderChatMarkdown; see the header comment. -->
<!-- eslint-disable-next-line svelte/no-at-html-tags -->
<div class="body md" use:enhance={html}>{@html html}</div>

<style>
	.md {
		margin: var(--space-1) 0 0;
		overflow-wrap: anywhere;
		word-break: break-word;
		min-width: 0;
	}
	.md :global(> :first-child) {
		margin-top: 0;
	}
	.md :global(> :last-child) {
		margin-bottom: 0;
	}
	.md :global(p),
	.md :global(ul),
	.md :global(ol),
	.md :global(blockquote),
	.md :global(table),
	.md :global(pre) {
		margin: var(--space-2) 0;
	}
	.md :global(ul),
	.md :global(ol) {
		padding-left: 1.4em;
	}
	.md :global(h1),
	.md :global(h2),
	.md :global(h3),
	.md :global(h4),
	.md :global(h5),
	.md :global(h6) {
		margin: var(--space-2) 0 var(--space-1);
		font-size: 1rem;
		line-height: 1.3;
	}
	.md :global(a) {
		color: var(--accent-blue);
		overflow-wrap: anywhere;
	}
	.md :global(blockquote) {
		padding-left: var(--space-2);
		border-left: 3px solid var(--border);
		color: var(--text-secondary);
	}
	.md :global(code) {
		font-family: var(--font-mono);
		font-size: 0.85em;
	}
	.md :global(pre) {
		padding: var(--space-2);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		/* --bg-primary was invisible on the dock (TASK-4515). */
		background: var(--bg-tertiary);
		overflow-x: auto;
		white-space: pre;
		max-width: 100%;
	}
	.md :global(pre code) {
		font-size: 0.8rem;
	}
	/* Tables scroll on their own, never stretching the bubble. */
	.md :global(table) {
		display: block;
		max-width: 100%;
		overflow-x: auto;
		border-collapse: collapse;
	}
	.md :global(th),
	.md :global(td) {
		padding: var(--space-1) var(--space-2);
		border: 1px solid var(--border);
		text-align: left;
	}
	.md :global(img) {
		display: block;
		max-width: 100%;
		max-height: 20rem;
		height: auto;
		border-radius: var(--radius);
	}
	.md :global(.ref-restricted),
	.md :global(.md-note) {
		font-style: italic;
		color: var(--text-secondary);
	}
	.md :global(.md-copy) {
		display: block;
		margin: calc(-1 * var(--space-1)) 0 var(--space-2) auto;
		padding: 0 var(--space-2);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		background: var(--bg-secondary);
		color: var(--text-secondary);
		font: inherit;
		font-size: 0.75rem;
		cursor: pointer;
	}
	.md :global(.md-copy:hover) {
		color: var(--text-primary);
	}
</style>
