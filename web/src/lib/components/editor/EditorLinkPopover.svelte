<script lang="ts">
	import type { Editor } from '@tiptap/core';

	let {
		editor,
	}: {
		editor: Editor | null;
	} = $props();

	let visible = $state(false);
	let popX = $state(0);
	let popY = $state(0);
	let href = $state('');
	let editing = $state(false);
	let editValue = $state('');

	let popoverEl = $state<HTMLDivElement>();
	let editInputEl = $state<HTMLInputElement>();

	let truncatedHref = $derived(
		href.length > 40 ? href.slice(0, 37) + '...' : href
	);

	function handleUpdate() {
		if (!editor) return;
		const { from, to } = editor.state.selection;

		// Only show when cursor is collapsed (no text selection) and inside a link
		if (from !== to || !editor.isActive('link')) {
			hide();
			return;
		}

		const attrs = editor.getAttributes('link');
		if (!attrs.href) {
			hide();
			return;
		}

		href = attrs.href;
		positionPopover(from);
		visible = true;
	}

	function handleBlur() {
		setTimeout(() => {
			if (!popoverEl?.contains(document.activeElement) && !popoverEl?.matches(':hover')) {
				hide();
			}
		}, 150);
	}

	function positionPopover(pos: number) {
		if (!editor) return;

		// Find the extent of the link mark around the cursor
		const resolvedPos = editor.state.doc.resolve(pos);
		const linkMark = resolvedPos.marks().find((m) => m.type.name === 'link');
		if (!linkMark) return;

		// Walk backwards to find start of link text
		let start = pos;
		while (start > resolvedPos.start()) {
			const prev = editor.state.doc.resolve(start - 1);
			if (!prev.marks().some((m) => m.type === linkMark.type && m.attrs.href === linkMark.attrs.href)) break;
			start--;
		}

		// Walk forwards to find end of link text
		let end = pos;
		const nodeEnd = resolvedPos.end();
		while (end < nodeEnd) {
			const next = editor.state.doc.resolve(end);
			if (!next.marks().some((m) => m.type === linkMark.type && m.attrs.href === linkMark.attrs.href)) break;
			end++;
		}

		const coordsFrom = editor.view.coordsAtPos(start);
		const coordsTo = editor.view.coordsAtPos(end);
		const centerX = (coordsFrom.left + coordsTo.left) / 2;
		const bottomY = Math.max(coordsFrom.bottom, coordsTo.bottom);

		const popWidth = editing ? 320 : 280;
		const padding = 8;

		let x = centerX - popWidth / 2;
		let y = bottomY + 4;

		// Clamp to viewport
		x = Math.max(padding, Math.min(x, window.innerWidth - popWidth - padding));
		y = Math.max(padding, Math.min(y, window.innerHeight - 60 - padding));

		popX = x;
		popY = y;
	}

	function hide() {
		visible = false;
		editing = false;
		editValue = '';
	}

	function openUrl() {
		if (href) {
			window.open(href, '_blank', 'noopener,noreferrer');
		}
	}

	function startEdit() {
		editValue = href;
		editing = true;
		// Reposition to accommodate wider input
		if (editor) {
			const { from } = editor.state.selection;
			positionPopover(from);
		}
		// Focus the input after it renders
		requestAnimationFrame(() => {
			editInputEl?.focus();
			editInputEl?.select();
		});
	}

	function confirmEdit() {
		if (!editor || !editValue.trim()) return;
		editor.chain().focus().extendMarkRange('link').setLink({ href: editValue.trim() }).run();
		href = editValue.trim();
		editing = false;
	}

	function cancelEdit() {
		editing = false;
		editValue = '';
		editor?.chain().focus().run();
	}

	function removeLink() {
		if (!editor) return;
		editor.chain().focus().unsetLink().run();
		hide();
	}

	$effect(() => {
		if (!editor) return;

		const onSelectionUpdate = handleUpdate;
		const onTransaction = handleUpdate;
		const onBlur = handleBlur;

		editor.on('selectionUpdate', onSelectionUpdate);
		editor.on('transaction', onTransaction);
		editor.on('blur', onBlur);

		return () => {
			editor.off('selectionUpdate', onSelectionUpdate);
			editor.off('transaction', onTransaction);
			editor.off('blur', onBlur);
		};
	});
</script>

{#if visible}
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div
		class="link-popover"
		class:editing
		style:left="{popX}px"
		style:top="{popY}px"
		bind:this={popoverEl}
		onmousedown={(e) => {
			const tag = (e.target as HTMLElement).tagName;
			if (tag !== 'INPUT') {
				e.preventDefault();
			}
		}}
	>
		<div class="link-arrow"></div>

		{#if !editing}
			<div class="link-display">
				<span class="link-href" title={href}>{truncatedHref}</span>
				<div class="link-actions">
					<button class="link-btn" onclick={openUrl} title="Open link">
						<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
							<path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6" />
							<polyline points="15 3 21 3 21 9" />
							<line x1="10" y1="14" x2="21" y2="3" />
						</svg>
					</button>
					<button class="link-btn" onclick={startEdit} title="Edit link">
						<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
							<path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7" />
							<path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z" />
						</svg>
					</button>
					<button class="link-btn link-btn-danger" onclick={removeLink} title="Remove link">
						<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
							<path d="M15 7h3a5 5 0 0 1 0 10h-3" />
							<path d="M9 17H6a5 5 0 0 1 0-10h3" />
							<line x1="2" y1="2" x2="22" y2="22" />
						</svg>
					</button>
				</div>
			</div>
		{:else}
			<div class="link-edit">
				<input
					type="text"
					class="link-input"
					bind:this={editInputEl}
					bind:value={editValue}
					placeholder="https://..."
					onkeydown={(e) => {
						if (e.key === 'Enter') { e.preventDefault(); confirmEdit(); }
						if (e.key === 'Escape') { e.preventDefault(); cancelEdit(); }
					}}
				/>
				<button class="link-btn-save" onclick={confirmEdit} disabled={!editValue.trim()}>
					Save
				</button>
				<button class="link-btn-cancel" onclick={cancelEdit}>
					Cancel
				</button>
			</div>
		{/if}
	</div>
{/if}

<style>
	.link-popover {
		position: fixed;
		z-index: 40;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		box-shadow: 0 4px 16px rgba(0, 0, 0, 0.25);
		padding: var(--space-1) var(--space-2);
		animation: link-pop-in 0.12s ease-out;
	}

	.link-popover.editing {
		padding: var(--space-2) var(--space-3);
	}

	@keyframes link-pop-in {
		from {
			opacity: 0;
			transform: scale(0.95) translateY(-4px);
		}
		to {
			opacity: 1;
			transform: scale(1) translateY(0);
		}
	}

	.link-arrow {
		position: absolute;
		top: -5px;
		left: 50%;
		transform: translateX(-50%) rotate(45deg);
		width: 8px;
		height: 8px;
		background: var(--bg-secondary);
		border-left: 1px solid var(--border);
		border-top: 1px solid var(--border);
	}

	.link-display {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	.link-href {
		font-size: 0.82em;
		color: var(--text-muted);
		font-family: var(--font-mono);
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
		max-width: 180px;
		padding: var(--space-1) 0;
	}

	.link-actions {
		display: flex;
		align-items: center;
		gap: 2px;
		margin-left: var(--space-1);
	}

	.link-btn {
		display: flex;
		align-items: center;
		justify-content: center;
		padding: var(--space-1);
		border-radius: var(--radius-sm);
		color: var(--text-secondary);
		cursor: pointer;
	}

	.link-btn:hover {
		background: var(--bg-hover);
		color: var(--text-primary);
	}

	.link-btn-danger:hover {
		background: rgba(239, 68, 68, 0.1);
		color: #ef4444;
	}

	.link-edit {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		min-width: 280px;
	}

	.link-input {
		flex: 1;
		padding: var(--space-1) var(--space-2);
		font-size: 0.85em;
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		outline: none;
		font-family: var(--font-mono);
	}

	.link-input:focus {
		border-color: var(--accent-blue);
	}

	.link-btn-save {
		padding: var(--space-1) var(--space-3);
		font-size: 0.82em;
		font-weight: 600;
		background: var(--accent-blue);
		color: #fff;
		border-radius: var(--radius-sm);
		white-space: nowrap;
		cursor: pointer;
	}

	.link-btn-save:hover:not(:disabled) {
		filter: brightness(1.1);
	}

	.link-btn-save:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.link-btn-cancel {
		padding: var(--space-1) var(--space-2);
		font-size: 0.82em;
		color: var(--text-muted);
		border-radius: var(--radius-sm);
		white-space: nowrap;
		cursor: pointer;
	}

	.link-btn-cancel:hover {
		color: var(--text-primary);
		background: var(--bg-hover);
	}
</style>
