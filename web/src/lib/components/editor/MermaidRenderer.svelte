<script lang="ts">
	import type { Editor } from '@tiptap/core';

	let {
		editor,
	}: {
		editor: Editor | null;
	} = $props();

	interface MermaidBlock {
		source: string;
		svg: string;
		error: boolean;
	}

	let blocks = $state<MermaidBlock[]>([]);
	let mermaidMod: typeof import('mermaid') | null = null;
	let rendering = $state(false);

	async function ensureMermaid() {
		if (!mermaidMod) {
			mermaidMod = await import('mermaid');
			mermaidMod.default.initialize({
				startOnLoad: false,
				theme: 'dark',
				securityLevel: 'strict',
				fontFamily: 'inherit',
			});
		}
		return mermaidMod;
	}

	function extractMermaidSources(ed: Editor): string[] {
		const sources: string[] = [];
		ed.state.doc.descendants((node) => {
			if (node.type.name === 'codeBlock' && node.attrs.language === 'mermaid') {
				const text = node.textContent.trim();
				if (text) sources.push(text);
			}
		});
		return sources;
	}

	let lastSourcesKey = '';

	async function update() {
		if (!editor || rendering) return;

		const sources = extractMermaidSources(editor);
		const key = sources.join('\n---\n');
		if (key === lastSourcesKey) return;
		lastSourcesKey = key;

		if (sources.length === 0) {
			blocks = [];
			return;
		}

		rendering = true;
		try {
			const m = await ensureMermaid();
			const newBlocks: MermaidBlock[] = [];

			// Render one at a time — mermaid can't handle concurrent renders
			for (const source of sources) {
				try {
					const id = `mmd-${Math.random().toString(36).slice(2, 10)}`;
					const { svg } = await m.default.render(id, source);
					newBlocks.push({ source, svg, error: false });
				} catch {
					newBlocks.push({ source, svg: '', error: true });
				}
			}

			blocks = newBlocks;
		} finally {
			rendering = false;
		}
	}

	$effect(() => {
		if (!editor) return;

		const handler = () => update();
		editor.on('update', handler);

		// Initial render — delay slightly to let editor DOM settle
		setTimeout(update, 100);

		return () => {
			editor.off('update', handler);
		};
	});
</script>

{#if blocks.length > 0}
	<div class="mermaid-section">
		<div class="mermaid-header">Diagrams</div>
		{#each blocks as block, i (i)}
			<div class="mermaid-block">
				{#if block.error}
					<div class="mermaid-error">Could not render diagram</div>
				{:else}
					<div class="mermaid-diagram">{@html block.svg}</div>
				{/if}
			</div>
		{/each}
	</div>
{/if}

<style>
	.mermaid-section {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		margin-top: var(--space-6);
		padding-top: var(--space-4);
		border-top: 1px solid var(--border);
	}

	.mermaid-header {
		font-size: 0.8em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
	}

	.mermaid-block {
		background: var(--bg-tertiary);
		border-radius: var(--radius);
		padding: var(--space-4);
		overflow-x: auto;
	}

	.mermaid-diagram {
		display: flex;
		justify-content: center;
	}

	.mermaid-diagram :global(svg) {
		max-width: 100%;
		height: auto;
	}

	.mermaid-error {
		color: var(--accent-orange);
		font-size: 0.85em;
		text-align: center;
		padding: var(--space-2);
	}
</style>
