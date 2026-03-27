<script lang="ts">
	import type { Editor } from '@tiptap/core';
	import type { Collection, Item } from '$lib/types';
	import { parseSchema } from '$lib/types';
	import { api } from '$lib/api/client';
	import { toastStore } from '$lib/stores/toast.svelte';

	const AGENT_SLUGS = ['conventions', 'playbooks'];

	let {
		editor,
		wsSlug,
		collections,
		onItemCreated,
	}: {
		editor: Editor | null;
		wsSlug: string;
		collections: Collection[];
		onItemCreated?: (item: Item) => void;
	} = $props();

	let visible = $state(false);
	let menuX = $state(0);
	let menuY = $state(0);
	let selFrom = $state(0);
	let selTo = $state(0);
	let selectedText = $state('');

	// Extract form state
	let showForm = $state(false);
	let title = $state('');
	let selectedCollectionSlug = $state('');
	let creating = $state(false);
	let errorMsg = $state('');

	let menuEl = $state<HTMLDivElement>();

	// Sort collections: non-agent first, agent slugs last
	let sortedCollections = $derived.by(() => {
		const normal = collections.filter((c) => !AGENT_SLUGS.includes(c.slug));
		const agent = collections.filter((c) => AGENT_SLUGS.includes(c.slug));
		return [...normal, ...agent];
	});

	let defaultCollectionSlug = $derived(
		sortedCollections.length > 0 ? sortedCollections[0].slug : ''
	);

	function handleSelectionUpdate() {
		if (!editor) return;
		const { from, to } = editor.state.selection;
		if (from === to) {
			hide();
			return;
		}
		const text = editor.state.doc.textBetween(from, to, ' ');
		if (!text.trim()) {
			hide();
			return;
		}
		selFrom = from;
		selTo = to;
		selectedText = text;
		positionMenu(from, to);
		visible = true;
	}

	function handleBlur() {
		// Delay to allow clicks on the menu itself
		setTimeout(() => {
			if (!menuEl?.contains(document.activeElement) && !menuEl?.matches(':hover')) {
				hide();
			}
		}, 150);
	}

	function positionMenu(from: number, to: number) {
		if (!editor) return;
		const coordsFrom = editor.view.coordsAtPos(from);
		const coordsTo = editor.view.coordsAtPos(to);
		const centerX = (coordsFrom.left + coordsTo.left) / 2;
		const topY = Math.min(coordsFrom.top, coordsTo.top);

		// Estimate menu dimensions for clamping
		const menuWidth = showForm ? 340 : 120;
		const menuHeight = 40;
		const padding = 8;

		let x = centerX - menuWidth / 2;
		let y = topY - menuHeight - 8;

		// Clamp to viewport
		x = Math.max(padding, Math.min(x, window.innerWidth - menuWidth - padding));
		y = Math.max(padding, y);

		menuX = x;
		menuY = y;
	}

	function hide() {
		if (creating) return;
		visible = false;
		showForm = false;
		title = '';
		errorMsg = '';
		creating = false;
	}

	function openForm() {
		title = selectedText.trim();
		selectedCollectionSlug = defaultCollectionSlug;
		showForm = true;
		errorMsg = '';
		// Reposition to accommodate larger menu
		if (editor) {
			positionMenu(selFrom, selTo);
		}
	}

	function cancelForm() {
		showForm = false;
		title = '';
		errorMsg = '';
	}

	function getDefaultFields(collection: Collection): Record<string, any> {
		const schema = parseSchema(collection);
		const fields: Record<string, any> = {};
		for (const field of schema.fields) {
			if (field.key === 'status' && field.options && field.options.length > 0) {
				fields.status = field.options[0];
			}
		}
		return fields;
	}

	async function handleCreate() {
		if (!editor || !title.trim()) return;
		creating = true;
		errorMsg = '';

		const coll = collections.find((c) => c.slug === selectedCollectionSlug);
		if (!coll) {
			errorMsg = 'Collection not found';
			creating = false;
			return;
		}

		try {
			const defaultFields = getDefaultFields(coll);
			// If the selected text is longer than the title, include full text as content
			const trimmedTitle = title.trim();
			const fullText = selectedText.trim();
			const content = fullText.length > trimmedTitle.length ? fullText : '';
			const item = await api.items.create(wsSlug, selectedCollectionSlug, {
				title: trimmedTitle,
				content,
				fields: JSON.stringify(defaultFields),
				source: 'web',
			});

			// Replace the selection with a wiki-link
			const wikiLink = `[[${item.title}]]`;
			editor
				.chain()
				.focus()
				.deleteRange({ from: selFrom, to: selTo })
				.insertContentAt(selFrom, wikiLink)
				.run();

			onItemCreated?.(item);
			toastStore.show(`Created "${item.title}"`, 'success');
			hide();
		} catch (err: any) {
			errorMsg = err.message || 'Failed to create item';
		} finally {
			creating = false;
		}
	}

	// Subscribe/unsubscribe to editor events
	let boundSelectionUpdate: (() => void) | null = null;
	let boundBlur: (() => void) | null = null;

	$effect(() => {
		if (!editor) return;

		boundSelectionUpdate = handleSelectionUpdate;
		boundBlur = handleBlur;

		editor.on('selectionUpdate', boundSelectionUpdate);
		editor.on('blur', boundBlur);

		return () => {
			if (boundSelectionUpdate) editor.off('selectionUpdate', boundSelectionUpdate);
			if (boundBlur) editor.off('blur', boundBlur);
		};
	});
</script>

{#if visible}
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div
		class="bubble-menu"
		class:expanded={showForm}
		style:left="{menuX}px"
		style:top="{menuY}px"
		bind:this={menuEl}
		onmousedown={(e) => {
			const tag = (e.target as HTMLElement).tagName;
			if (tag !== 'SELECT' && tag !== 'INPUT' && tag !== 'OPTION') {
				e.preventDefault();
			}
		}}
	>
		<div class="bubble-arrow"></div>

		{#if !showForm}
			<button class="extract-btn" onclick={openForm}>
				<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
					<circle cx="6" cy="6" r="3" />
					<path d="M8.12 8.12L12 12" />
					<path d="M20 4L8.12 8.12" />
					<circle cx="12" cy="12" r="2" />
					<path d="M13.41 13.41L20 20" />
				</svg>
				Extract
			</button>
		{:else}
			<div class="extract-form">
				<div class="form-row">
					<input
						type="text"
						class="title-input"
						bind:value={title}
						placeholder="Item title..."
						onkeydown={(e) => {
							if (e.key === 'Enter') { e.preventDefault(); handleCreate(); }
							if (e.key === 'Escape') { e.preventDefault(); cancelForm(); }
						}}
					/>
				</div>
				<div class="form-row">
					<select class="coll-select" bind:value={selectedCollectionSlug}>
						{#each sortedCollections as coll (coll.id)}
							<option value={coll.slug}>{coll.icon} {coll.name}</option>
						{/each}
					</select>
					<button class="btn-create" onclick={handleCreate} disabled={creating || !title.trim()}>
						{creating ? '...' : 'Create'}
					</button>
					<button class="btn-cancel" onclick={cancelForm}>Cancel</button>
				</div>
				{#if errorMsg}
					<div class="form-error">{errorMsg}</div>
				{/if}
			</div>
		{/if}
	</div>
{/if}

<style>
	.bubble-menu {
		position: fixed;
		z-index: 40;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		box-shadow: 0 4px 16px rgba(0, 0, 0, 0.25);
		padding: var(--space-1);
		animation: bubble-in 0.12s ease-out;
	}

	.bubble-menu.expanded {
		padding: var(--space-2) var(--space-3);
	}

	@keyframes bubble-in {
		from {
			opacity: 0;
			transform: scale(0.95) translateY(4px);
		}
		to {
			opacity: 1;
			transform: scale(1) translateY(0);
		}
	}

	.bubble-arrow {
		position: absolute;
		bottom: -5px;
		left: 50%;
		transform: translateX(-50%) rotate(45deg);
		width: 8px;
		height: 8px;
		background: var(--bg-secondary);
		border-right: 1px solid var(--border);
		border-bottom: 1px solid var(--border);
	}

	.extract-btn {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-1) var(--space-3);
		border-radius: var(--radius-sm);
		font-size: 0.85em;
		font-weight: 500;
		color: var(--text-primary);
		white-space: nowrap;
		cursor: pointer;
	}

	.extract-btn:hover {
		background: var(--bg-hover);
		color: var(--accent-blue);
	}

	.extract-form {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		min-width: 280px;
	}

	.form-row {
		display: flex;
		gap: var(--space-2);
		align-items: center;
	}

	.title-input {
		flex: 1;
		padding: var(--space-1) var(--space-2);
		font-size: 0.85em;
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		outline: none;
	}

	.title-input:focus {
		border-color: var(--accent-blue);
	}

	.coll-select {
		flex: 1;
		padding: var(--space-1) var(--space-2);
		font-size: 0.82em;
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		outline: none;
		cursor: pointer;
	}

	.btn-create {
		padding: var(--space-1) var(--space-3);
		font-size: 0.82em;
		font-weight: 600;
		background: var(--accent-blue);
		color: #fff;
		border-radius: var(--radius-sm);
		white-space: nowrap;
		cursor: pointer;
	}

	.btn-create:hover:not(:disabled) {
		filter: brightness(1.1);
	}

	.btn-create:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.btn-cancel {
		padding: var(--space-1) var(--space-2);
		font-size: 0.82em;
		color: var(--text-muted);
		border-radius: var(--radius-sm);
		white-space: nowrap;
		cursor: pointer;
	}

	.btn-cancel:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}

	.form-error {
		font-size: 0.8em;
		color: #ef4444;
		padding: 0 var(--space-1);
	}
</style>
