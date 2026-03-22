<script lang="ts">
	import { onMount, onDestroy, untrack } from 'svelte';
	import { Editor } from '@tiptap/core';
	import StarterKit from '@tiptap/starter-kit';
	import TaskList from '@tiptap/extension-task-list';
	import TaskItem from '@tiptap/extension-task-item';
	import { Table, TableRow, TableCell, TableHeader } from '@tiptap/extension-table';
	import Link from '@tiptap/extension-link';
	import Placeholder from '@tiptap/extension-placeholder';
	import { Markdown } from 'tiptap-markdown';
	import { unescapeDocLinks } from '$lib/utils/markdown';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { BlockDragHandle } from './block-drag-handle';

	let {
		content = '',
		editable = true,
		onUpdate,
		onEditor,
	}: {
		content?: string;
		editable?: boolean;
		onUpdate?: (markdown: string) => void;
		onEditor?: (editor: Editor) => void;
	} = $props();

	let element = $state<HTMLDivElement>();
	let editor = $state<Editor | null>(null);
	let editorFocused = $state(false);
	let editorTick = $state(0);
	let isMobile = $state(typeof window !== 'undefined' && window.innerWidth <= 768);
	let toolbarBottom = $state(0);
	let keyboardVisible = $state(false);
	let suppressUpdate = false;
	let lastMarkdown = '';

	// Slash command state
	let slashOpen = $state(false);
	let slashQuery = $state('');
	let slashX = $state(0);
	let slashY = $state(0);
	let slashIdx = $state(0);
	let slashStartPos = -1;

	// [[ link picker state
	let linkOpen = $state(false);
	let linkQuery = $state('');
	let linkX = $state(0);
	let linkY = $state(0);
	let linkIdx = $state(0);
	let linkStartPos = -1;
	let bracketCount = $state(0); // track consecutive [ chars

	const SLASH_ITEMS = [
		{ title: 'Heading 1', icon: 'H1', desc: 'Large heading', id: 'h1' },
		{ title: 'Heading 2', icon: 'H2', desc: 'Medium heading', id: 'h2' },
		{ title: 'Heading 3', icon: 'H3', desc: 'Small heading', id: 'h3' },
		{ title: 'Bullet List', icon: '•', desc: 'Unordered list', id: 'bullet' },
		{ title: 'Numbered List', icon: '1.', desc: 'Ordered list', id: 'ordered' },
		{ title: 'Checklist', icon: '☐', desc: 'Task list', id: 'task' },
		{ title: 'Code Block', icon: '<>', desc: 'Fenced code block', id: 'code' },
		{ title: 'Blockquote', icon: '❝', desc: 'Quote block', id: 'quote' },
		{ title: 'Divider', icon: '——', desc: 'Horizontal rule', id: 'hr' },
		{ title: 'Table', icon: '⊞', desc: '3x3 table', id: 'table' },
	];

	function getFilteredSlash() {
		if (!slashQuery) return SLASH_ITEMS;
		const q = slashQuery.toLowerCase();
		return SLASH_ITEMS.filter(i => i.title.toLowerCase().includes(q));
	}

	function execSlash(id: string) {
		if (!editor) return;
		if (slashStartPos >= 0) {
			const to = editor.state.selection.from;
			editor.chain().focus().deleteRange({ from: slashStartPos, to }).run();
		}
		const c = editor.chain().focus();
		if (id === 'h1') c.toggleHeading({ level: 1 }).run();
		else if (id === 'h2') c.toggleHeading({ level: 2 }).run();
		else if (id === 'h3') c.toggleHeading({ level: 3 }).run();
		else if (id === 'bullet') c.toggleBulletList().run();
		else if (id === 'ordered') c.toggleOrderedList().run();
		else if (id === 'task') c.toggleTaskList().run();
		else if (id === 'code') c.toggleCodeBlock().run();
		else if (id === 'quote') c.toggleBlockquote().run();
		else if (id === 'hr') c.setHorizontalRule().run();
		else if (id === 'table') c.insertTable({ rows: 3, cols: 3, withHeaderRow: true }).run();
		closeSlash();
	}

	function closeSlash() {
		slashOpen = false;
		slashQuery = '';
		slashStartPos = -1;
		slashIdx = 0;
	}

	function getFilteredLinks() {
		const items = collectionStore.items ?? [];
		if (!linkQuery) return items.slice(0, 10);
		const q = linkQuery.toLowerCase();
		return items.filter(d => d.title.toLowerCase().includes(q)).slice(0, 10);
	}

	function execLink(title: string) {
		if (!editor) return;
		// Delete the [[ and any query text typed so far
		const to = editor.state.selection.from;
		editor.chain().focus().deleteRange({ from: linkStartPos, to }).insertContent(`[[${title}]]`).run();
		closeLink();
	}

	function closeLink() {
		linkOpen = false;
		linkQuery = '';
		linkStartPos = -1;
		linkIdx = 0;
		bracketCount = 0;
	}

	onMount(() => {
		if (!element) return;

		const extensions = [
			StarterKit.configure({
				codeBlock: {
					HTMLAttributes: { class: 'code-block' },
				},
			}),
			TaskList,
			TaskItem.configure({ nested: true }),
			Table.configure({ resizable: true, HTMLAttributes: { class: 'table-wrapper' } }),
			TableRow,
			TableCell,
			TableHeader,
			Link.configure({
				openOnClick: false,
				HTMLAttributes: { class: 'editor-link', target: null, rel: null },
			}),
			Placeholder.configure({
				placeholder: 'Type / for commands...',
			}),
			Markdown.configure({
				html: true,
				transformPastedText: true,
				transformCopiedText: true,
			}),
			BlockDragHandle,
		];

		editor = new Editor({
			element,
			editable,
			extensions,
			content,
			onUpdate: ({ editor: e }) => {
				if (suppressUpdate) return;
				const md = unescapeDocLinks(e.storage.markdown.getMarkdown());
				if (md === lastMarkdown) return;
				lastMarkdown = md;
				onUpdate?.(md);
				if (slashOpen && slashStartPos >= 0) {
					const curPos = e.state.selection.from;
					if (curPos <= slashStartPos) { closeSlash(); }
					else {
						const text = e.state.doc.textBetween(slashStartPos, curPos, '');
						if (text.startsWith('/')) {
							slashQuery = text.slice(1);
							slashIdx = 0;
							// Auto-close if query has content but nothing matches
							if (slashQuery && getFilteredSlash().length === 0) { closeSlash(); }
						}
						else { closeSlash(); }
					}
				}
				if (linkOpen && linkStartPos >= 0) {
					const curPos = e.state.selection.from;
					if (curPos <= linkStartPos) { closeLink(); }
					else {
						const text = e.state.doc.textBetween(linkStartPos, curPos, '');
						if (text.startsWith('[[')) { linkQuery = text.slice(2); linkIdx = 0; }
						else { closeLink(); }
					}
				}
			},
			onTransaction: () => {
				editor = editor;
				editorTick++;
			},
			editorProps: {
				handleKeyDown: (_view, event) => {
					// --- [[ link picker ---
					if (event.key === '[' && !linkOpen && !slashOpen) {
						bracketCount++;
						if (bracketCount === 2) {
							// Second [ detected — open link picker
							// linkStartPos points to the first [
							linkStartPos = _view.state.selection.from - 1;
							linkQuery = '';
							linkIdx = 0;
							setTimeout(() => {
								const coords = _view.coordsAtPos(_view.state.selection.from);
								linkX = coords.left;
								linkY = coords.bottom + 4;
								linkOpen = true;
							}, 0);
							bracketCount = 0;
							return false;
						}
						// Reset after a short delay if second [ doesn't come
						setTimeout(() => { if (bracketCount === 1) bracketCount = 0; }, 300);
						return false;
					}
					if (event.key !== '[') bracketCount = 0;

					if (linkOpen) {
						const items = getFilteredLinks();
						if (event.key === 'ArrowDown') { event.preventDefault(); linkIdx = (linkIdx + 1) % Math.max(items.length, 1); return true; }
						if (event.key === 'ArrowUp') { event.preventDefault(); linkIdx = (linkIdx - 1 + items.length) % Math.max(items.length, 1); return true; }
						if (event.key === 'Enter') { event.preventDefault(); if (items[linkIdx]) execLink(items[linkIdx].title); return true; }
						if (event.key === 'Escape') { event.preventDefault(); closeLink(); return true; }
						return false;
					}

					// --- slash commands ---
					if (event.key === '/' && !slashOpen) {
						slashStartPos = _view.state.selection.from;
						slashQuery = '';
						slashIdx = 0;
						setTimeout(() => {
							const coords = _view.coordsAtPos(_view.state.selection.from);
							slashX = coords.left;
							slashY = coords.bottom + 4;
							slashOpen = true;
						}, 0);
						return false;
					}
					if (!slashOpen) return false;
					const items = getFilteredSlash();
					if (items.length === 0) {
						// No matches — close and let the keypress through
						closeSlash();
						return false;
					}
					if (event.key === 'ArrowDown') { event.preventDefault(); slashIdx = (slashIdx + 1) % items.length; return true; }
					if (event.key === 'ArrowUp') { event.preventDefault(); slashIdx = (slashIdx - 1 + items.length) % items.length; return true; }
					if (event.key === 'Enter') { event.preventDefault(); if (items[slashIdx]) execSlash(items[slashIdx].id); return true; }
					if (event.key === 'Escape') { event.preventDefault(); closeSlash(); return true; }
					return false;
				},
			},
		});

		lastMarkdown = unescapeDocLinks(editor.storage.markdown.getMarkdown());
		onEditor?.(editor);

		editor.on('focus', () => { editorFocused = true; });
		editor.on('blur', () => { editorFocused = false; });

		// Prevent mobile keyboard from opening when tapping task list checkboxes
		if (isMobile) {
			editor.view.dom.addEventListener('touchend', (e) => {
				const target = e.target as HTMLElement;
				if (target.tagName === 'INPUT' && target.getAttribute('type') === 'checkbox' && target.closest('[data-type="taskItem"]')) {
					// Let the checkbox toggle, but blur to prevent keyboard popup
					requestAnimationFrame(() => {
						editor?.view.dom.blur();
					});
				}
			});
		}

		// Track keyboard height via visualViewport for mobile toolbar positioning
		if (window.visualViewport) {
			const updateToolbarPos = () => {
				const vv = window.visualViewport!;
				const kbHeight = window.innerHeight - vv.height - vv.offsetTop;
				toolbarBottom = kbHeight;
				// Hide toolbar when keyboard is dismissed (viewport matches window)
				keyboardVisible = kbHeight > 50;
			};
			window.visualViewport.addEventListener('resize', updateToolbarPos);
			window.visualViewport.addEventListener('scroll', updateToolbarPos);
		}
	});

	onDestroy(() => {
		editor?.destroy();
	});

	$effect(() => {
		// Only react to editable prop changes, not editor state changes
		const shouldBeEditable = editable;
		untrack(() => {
			editor?.setEditable(shouldBeEditable);
		});
	});

	// Sync content when prop changes (e.g. doc switch, external update)
	const tracker: { prev: string | undefined } = { prev: undefined };
	$effect(() => {
		if (tracker.prev === undefined) {
			// First run: capture initial value without syncing
			tracker.prev = content;
			return;
		}
		if (editor && content !== tracker.prev) {
			tracker.prev = content;
			const currentEditorContent = unescapeDocLinks(editor.storage.markdown?.getMarkdown?.() ?? '');
			if (currentEditorContent !== content) {
				suppressUpdate = true;
				editor.commands.setContent(content);
				lastMarkdown = unescapeDocLinks(editor.storage.markdown?.getMarkdown?.() ?? '');
				suppressUpdate = false;
			}
		}
	});

	export function getEditor(): Editor | null {
		return editor;
	}


</script>

{#if isMobile && keyboardVisible && editor}
	{@const _tick = editorTick}
	<div class="mobile-toolbar" style:bottom="{toolbarBottom}px" onmousedown={(e) => e.preventDefault()}>
		<button class="mt-btn" class:active={_tick >= 0 && editor.isActive('bold')} onclick={() => editor?.chain().focus().toggleBold().run()}>B</button>
		<button class="mt-btn" class:active={_tick >= 0 && editor.isActive('italic')} onclick={() => editor?.chain().focus().toggleItalic().run()}><em>I</em></button>
		<button class="mt-btn" class:active={_tick >= 0 && editor.isActive('strike')} onclick={() => editor?.chain().focus().toggleStrike().run()}><s>S</s></button>
		<span class="mt-sep"></span>
		<button class="mt-btn" class:active={_tick >= 0 && editor.isActive('heading', { level: 2 })} onclick={() => editor?.chain().focus().toggleHeading({ level: 2 }).run()}>H2</button>
		<button class="mt-btn" class:active={_tick >= 0 && editor.isActive('heading', { level: 3 })} onclick={() => editor?.chain().focus().toggleHeading({ level: 3 }).run()}>H3</button>
		<span class="mt-sep"></span>
		<button class="mt-btn" class:active={_tick >= 0 && editor.isActive('bulletList')} onclick={() => editor?.chain().focus().toggleBulletList().run()}>•</button>
		<button class="mt-btn" class:active={_tick >= 0 && editor.isActive('orderedList')} onclick={() => editor?.chain().focus().toggleOrderedList().run()}>1.</button>
		<button class="mt-btn" class:active={_tick >= 0 && editor.isActive('taskList')} onclick={() => editor?.chain().focus().toggleTaskList().run()}>☐</button>
		<span class="mt-sep"></span>
		<button class="mt-btn" class:active={_tick >= 0 && editor.isActive('codeBlock')} onclick={() => editor?.chain().focus().toggleCodeBlock().run()}>&lt;&gt;</button>
		<button class="mt-btn" class:active={_tick >= 0 && editor.isActive('blockquote')} onclick={() => editor?.chain().focus().toggleBlockquote().run()}>❝</button>
	</div>
{/if}

<div class="editor-wrapper">
	<div bind:this={element} class="editor-content prose"></div>
</div>

{#if slashOpen}
	<div style="position:fixed; inset:0; z-index:49;" onclick={closeSlash}></div>
	<div class="slash-menu" style:left="{slashX}px" style:top="{slashY}px">
		{#each getFilteredSlash() as item, i}
			<button
				class="slash-item"
				class:selected={i === slashIdx}
				onmouseenter={() => slashIdx = i}
				onclick={() => execSlash(item.id)}
			>
				<span class="slash-icon">{item.icon}</span>
				<span class="slash-title">{item.title}</span>
			</button>
		{/each}
	</div>
{/if}

{#if linkOpen}
	<!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
	<div style="position:fixed; inset:0; z-index:49;" onclick={closeLink}></div>
	<div class="slash-menu" style:left="{linkX}px" style:top="{linkY}px">
		{#each getFilteredLinks() as doc, i (doc.title)}
			<button
				class="slash-item"
				class:selected={i === linkIdx}
				onmouseenter={() => linkIdx = i}
				onclick={() => execLink(doc.title)}
			>
				<span class="slash-icon">{doc.doc_type === 'roadmap' ? '📋' : doc.doc_type === 'phase-plan' ? '🏗️' : doc.doc_type === 'architecture' ? '🧠' : doc.doc_type === 'ideation' ? '💡' : doc.doc_type === 'feature-spec' ? '📄' : doc.doc_type === 'notes' ? '📝' : doc.doc_type === 'prompt-library' ? '💬' : doc.doc_type === 'reference' ? '📚' : '📄'}</span>
				<span class="slash-title">{doc.title}</span>
			</button>
		{:else}
			<div class="slash-item" style="color: var(--text-muted); cursor: default;">No matching documents</div>
		{/each}
	</div>
{/if}

<style>
	.editor-wrapper {
		min-height: 200px;
		position: relative;
	}
	.editor-content {
		outline: none;
		/* Pad left to make room for the drag handle */
		padding-left: 24px;
	}

	/* Block drag handle */
	.editor-wrapper :global(.block-drag-handle) {
		position: absolute;
		display: flex;
		align-items: center;
		justify-content: center;
		width: 32px;
		height: 32px;
		color: var(--text-secondary);
		cursor: grab;
		font-size: 1.2em;
		opacity: 0.5;
		transition: opacity 0.15s, background 0.15s;
		z-index: 10;
		-webkit-user-select: none;
		user-select: none;
		-webkit-touch-callout: none;
		touch-action: none;
		border-radius: var(--radius-sm);
	}
	/* Larger touch target on mobile via invisible padding */
	@media (max-width: 768px) {
		.editor-wrapper :global(.block-drag-handle) {
			width: 44px;
			height: 44px;
			font-size: 1.4em;
			opacity: 0.7;
		}
	}
	.editor-wrapper :global(.block-drag-handle:hover) {
		opacity: 1;
		background: var(--bg-hover);
		color: var(--text-primary);
	}
	.editor-wrapper :global(.block-drag-handle.active) {
		opacity: 1;
		color: var(--accent-blue);
		background: var(--bg-active);
		cursor: grabbing;
	}
	.editor-wrapper :global(.block-drop-line) {
		position: absolute;
		left: 24px;
		right: 0;
		height: 3px;
		background: var(--accent-blue);
		border-radius: 2px;
		z-index: 10;
		pointer-events: none;
		box-shadow: 0 0 4px var(--accent-blue);
	}
	/* Block context menu (appended to body, so use :global) */
	:global(.block-context-menu) {
		position: fixed;
		z-index: 200;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		box-shadow: 0 8px 30px rgba(0, 0, 0, 0.35);
		padding: 4px;
		min-width: 180px;
		max-height: 70vh;
		overflow-y: auto;
	}
	:global(.block-menu-backdrop) {
		position: fixed;
		inset: 0;
		z-index: 199;
	}
	:global(.block-menu-label) {
		padding: 6px 10px 4px;
		font-size: 0.7em;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
		font-weight: 600;
	}
	:global(.block-menu-item) {
		display: flex;
		align-items: center;
		gap: 8px;
		width: 100%;
		padding: 7px 10px;
		border: none;
		background: none;
		color: var(--text-primary);
		font-size: 0.88em;
		border-radius: var(--radius-sm);
		cursor: pointer;
		text-align: left;
		font-family: inherit;
	}
	:global(.block-menu-item:hover),
	:global(.block-menu-item.active) {
		background: var(--bg-hover);
	}
	:global(.block-menu-item.active) {
		color: var(--accent-blue);
	}
	:global(.block-menu-item-danger) {
		color: #ef4444;
	}
	:global(.block-menu-item-danger:hover) {
		background: rgba(239, 68, 68, 0.1);
	}
	:global(.block-menu-icon) {
		width: 22px;
		text-align: center;
		font-weight: 600;
		font-size: 0.9em;
		flex-shrink: 0;
	}
	:global(.block-menu-divider) {
		height: 1px;
		background: var(--border);
		margin: 4px 0;
	}
	.editor-content :global(.ProseMirror) {
		outline: none;
		min-height: 200px;
	}
	.editor-content :global(.ProseMirror p.is-editor-empty:first-child::before) {
		content: attr(data-placeholder);
		float: left;
		color: var(--text-muted);
		pointer-events: none;
		height: 0;
	}

	/* Task list styles */
	.editor-content :global(ul[data-type="taskList"]) {
		list-style: none;
		padding-left: 0;
	}
	.editor-content :global(ul[data-type="taskList"] li) {
		display: flex;
		align-items: baseline;
		gap: 8px;
	}
	.editor-content :global(ul[data-type="taskList"] li label) {
		flex-shrink: 0;
		display: flex;
		align-items: center;
		position: relative;
		top: 1px;
	}
	.editor-content :global(ul[data-type="taskList"] li label input[type="checkbox"]) {
		margin: 0;
		cursor: pointer;
	}
	.editor-content :global(ul[data-type="taskList"] li > div) {
		flex: 1;
	}

	/* Table styles */
	.editor-content :global(table) {
		border-collapse: collapse;
		width: 100%;
		margin: 0.8em 0;
	}
	.editor-content :global(th),
	.editor-content :global(td) {
		border: 1px solid var(--border);
		padding: var(--space-2) var(--space-3);
		text-align: left;
		min-width: 80px;
	}
	.editor-content :global(th) {
		background: var(--bg-secondary);
		font-weight: 600;
	}
	.editor-content :global(.selectedCell) {
		background: rgba(74, 158, 255, 0.1);
	}

	/* Code block */
	.editor-content :global(pre) {
		background: var(--bg-tertiary);
		padding: var(--space-4);
		border-radius: var(--radius);
		overflow-x: auto;
		margin: 0.8em 0;
		font-family: var(--font-mono);
		font-size: 0.9em;
	}

	/* Mobile keyboard toolbar */
	.mobile-toolbar {
		position: fixed;
		left: 0;
		right: 0;
		z-index: 100;
		display: flex;
		align-items: center;
		gap: 2px;
		padding: var(--space-2) var(--space-3);
		background: var(--bg-secondary);
		border-top: 1px solid var(--border);
		overflow-x: auto;
		-webkit-overflow-scrolling: touch;
	}
	.mt-btn {
		padding: var(--space-1) var(--space-2);
		border-radius: var(--radius-sm);
		font-size: 0.85em;
		font-weight: 600;
		color: var(--text-secondary);
		min-width: 32px;
		text-align: center;
		font-family: var(--font-mono);
		flex-shrink: 0;
	}
	@media (hover: hover) {
		.mt-btn:hover { background: var(--bg-hover); color: var(--text-primary); }
	}
	.mt-btn:focus { outline: none; }
	.mt-btn.active {
		background: var(--bg-active);
		color: var(--accent-blue);
	}
	.mt-sep {
		width: 1px;
		height: 20px;
		background: var(--border);
		margin: 0 2px;
		flex-shrink: 0;
	}
	.slash-backdrop { position: fixed; inset: 0; z-index: 49; }
	.slash-menu {
		position: fixed; z-index: 50; background: var(--bg-secondary);
		border: 1px solid var(--border); border-radius: var(--radius);
		box-shadow: 0 4px 16px rgba(0, 0, 0, 0.2);
		min-width: 200px; max-height: 320px; overflow-y: auto; padding: var(--space-1) 0;
	}
	.slash-item {
		display: flex; align-items: center; gap: var(--space-3);
		width: 100%; padding: var(--space-2) var(--space-3); text-align: left;
		color: var(--text-primary); cursor: pointer; font-size: 0.9em;
	}
	.slash-item:hover, .slash-item.selected { background: var(--bg-hover); }
	.slash-icon {
		width: 24px; text-align: center; font-weight: 600; font-family: var(--font-mono);
		font-size: 0.85em; color: var(--text-secondary);
	}
	.slash-title { font-weight: 500; }
</style>
