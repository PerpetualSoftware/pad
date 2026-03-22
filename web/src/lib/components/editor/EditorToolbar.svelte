<script lang="ts">
	import type { Editor } from '@tiptap/core';
	import { editorStore } from '$lib/stores/editor.svelte';

	let { editor }: { editor: Editor | null } = $props();

	function btn(label: string, action: () => void, isActive: boolean = false) {
		return { label, action, isActive };
	}

	const buttons = $derived(editor ? [
		{ group: 'inline', items: [
			btn('B', () => editor!.chain().focus().toggleBold().run(), editor.isActive('bold')),
			btn('I', () => editor!.chain().focus().toggleItalic().run(), editor.isActive('italic')),
			btn('S', () => editor!.chain().focus().toggleStrike().run(), editor.isActive('strike')),
			btn('`', () => editor!.chain().focus().toggleCode().run(), editor.isActive('code')),
		]},
		{ group: 'headings', items: [
			btn('H1', () => editor!.chain().focus().toggleHeading({ level: 1 }).run(), editor.isActive('heading', { level: 1 })),
			btn('H2', () => editor!.chain().focus().toggleHeading({ level: 2 }).run(), editor.isActive('heading', { level: 2 })),
			btn('H3', () => editor!.chain().focus().toggleHeading({ level: 3 }).run(), editor.isActive('heading', { level: 3 })),
		]},
		{ group: 'lists', items: [
			btn('•', () => editor!.chain().focus().toggleBulletList().run(), editor.isActive('bulletList')),
			btn('1.', () => editor!.chain().focus().toggleOrderedList().run(), editor.isActive('orderedList')),
			btn('☐', () => editor!.chain().focus().toggleTaskList().run(), editor.isActive('taskList')),
		]},
		{ group: 'blocks', items: [
			btn('<>', () => editor!.chain().focus().toggleCodeBlock().run(), editor.isActive('codeBlock')),
			btn('""', () => editor!.chain().focus().toggleBlockquote().run(), editor.isActive('blockquote')),
			btn('──', () => editor!.chain().focus().setHorizontalRule().run(), false),
			btn('⊞', () => editor!.chain().focus().insertTable({ rows: 3, cols: 3, withHeaderRow: true }).run(), false),
		]},
	] : []);
</script>

{#if editor}
	<div class="toolbar">
		{#each buttons as group, gi}
			{#if gi > 0}<span class="sep"></span>{/if}
			{#each group.items as b}
				<button
					class="tool-btn"
					class:active={b.isActive}
					onclick={b.action}
					title={b.label}
				>
					{b.label}
				</button>
			{/each}
		{/each}

		<span class="sep"></span>
		<span class="spacer"></span>

		<button
			class="tool-btn mode-btn"
			class:active={editorStore.mode === 'raw'}
			onclick={() => editorStore.mode === 'raw' ? editorStore.setMode('edit') : editorStore.setMode('raw')}
			title="Toggle raw markdown"
		>
			{editorStore.mode === 'raw' ? 'Rich' : 'Raw'}
		</button>
	</div>
{/if}

<style>
	.toolbar {
		display: flex;
		align-items: center;
		gap: 2px;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		margin-bottom: var(--space-4);
		position: sticky;
		top: 0;
		z-index: 5;
		flex-wrap: wrap;
	}
	.tool-btn {
		padding: var(--space-1) var(--space-2);
		border-radius: var(--radius-sm);
		font-size: 0.85em;
		font-weight: 600;
		color: var(--text-secondary);
		min-width: 28px;
		text-align: center;
		font-family: var(--font-mono);
	}
	.tool-btn:hover {
		background: var(--bg-hover);
		color: var(--text-primary);
	}
	.tool-btn.active {
		background: var(--bg-active);
		color: var(--accent-blue);
	}
	.sep {
		width: 1px;
		height: 20px;
		background: var(--border);
		margin: 0 var(--space-1);
	}
	.spacer { flex: 1; }
	.mode-btn {
		font-family: var(--font-ui);
		font-size: 0.8em;
		padding: var(--space-1) var(--space-3);
	}
</style>
