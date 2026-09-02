<script lang="ts">
	import type { Editor } from '@tiptap/core';
	import type { Collection, Item } from '$lib/types';
	import { parseSchema, isAgentCollection } from '$lib/types';
	import { api, isPlanLimitError, planLimitMessage } from '$lib/api/client';
	import { toastStore } from '$lib/stores/toast.svelte';
	import { localIndex } from '$lib/stores/localIndex.svelte';
	import { toBlockquote } from '$lib/utils/markdown';

	let {
		editor,
		wsSlug,
		collections,
		onItemCreated,
		onComment,
	}: {
		editor: Editor | null;
		wsSlug: string;
		collections: Collection[];
		onItemCreated?: (item: Item, ws: string, sinceEpoch?: number) => void;
		/**
		 * Quote the selection into the host's comment composer (IDEA-2843).
		 *
		 * Supplying this IS the capability — the Comment action renders when a
		 * host has a composer to quote into and not otherwise. There is no
		 * separate boolean, deliberately: this menu mounts in one place today,
		 * and a flag whose value is always `true` beside a callback that is
		 * always supplied is two ways to say one thing.
		 *
		 * It was briefly a peek-INdependent gate of its own, on the theory that
		 * a peeking master can comment (BUG-2263) but could not act on a
		 * selection. Measured end to end, that case does not exist: a
		 * drag-selection in a peeking master RE-ACTIVATES it
		 * (focus-follows-editing, PLAN-2179 DR-2), so by the time a selection
		 * exists the master is editable again. See
		 * `e2e/selection-comment-peek.spec.ts`, which asserts that rather than
		 * leaving it as folklore.
		 */
		onComment?: (markdown: string) => boolean;
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

	// Sort collections: non-agent first, agent-facing last. Membership comes
	// from the bootstrap_include trait (SPEC-5), not a slug list. TASK-2657.
	let sortedCollections = $derived.by(() => {
		const normal = collections.filter((c) => !isAgentCollection(c));
		const agent = collections.filter((c) => isAgentCollection(c));
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
		// Two buttons when the host wired a composer — the clamp keeps the menu
		// on screen, so an under-estimate lets it hang off the edge.
		const menuWidth = showForm ? 340 : (onComment ? 210 : 120);
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

	/**
	 * Quote the selection into the host's comment composer (IDEA-2843).
	 *
	 * The selection is NOT consumed — unlike Extract, which replaces it with a
	 * wiki-link, quoting leaves the document untouched, so a reader can quote
	 * the same passage into a second comment or keep reading from where they
	 * were.
	 */
	function quoteSelection() {
		if (!editor || !onComment) return;
		// Re-extract with a PARAGRAPH separator. `selectedText` is joined with a
		// single space (line 77) because Extract uses it as an item TITLE, where
		// newlines would be wrong — but that join silently flattens a
		// multi-paragraph selection into one run-on line, and a quote has to
		// keep the shape of what was quoted. Found by codex round 1; before it,
		// `toBlockquote`'s blank-line handling could never be reached from this
		// call site at all, which made that test true and unreachable.
		const text = editor.state.doc.textBetween(selFrom, selTo, '\n\n').trim();
		if (!text) return;
		// The return value is the whole reason `appendMarkdown` has one: it
		// says "inserted" or "did nothing", and discarding it here would put
		// the silent no-op back exactly where the handle was built to remove it
		// (codex round 3). A false means the composer is unreachable — the
		// timeline is filtered away, or the user cannot comment — so keep the
		// selection and the menu, and say so.
		if (!onComment(toBlockquote(text))) {
			toastStore.show("Couldn't add that quote — the comment box isn't available.", 'error');
			return;
		}
		hide();
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

		// Capture the workspace at create time. handleCreate awaits the
		// create call and fires onItemCreated after it; reading the live
		// `wsSlug` prop post-await could mis-attribute the item if the user
		// navigated to another workspace mid-create. Per Codex review (round 2).
		const ws = wsSlug;

		// Capture the ORIGINATING editor instance too. This bubble menu is
		// bound to the PERSISTENT editor (outside the split pane's {#key}),
		// which the host remounts per-item. If the item switches A→B during the
		// create await, the live `editor` prop now points at B's document —
		// inserting A's wiki-link at A's stale selection coords would corrupt B.
		// Require the SAME, undestroyed editor before the post-await insert /
		// callback / toast (PLAN-2105 / TASK-2112; coordinator).
		const originEditor = editor;

		// Scope epoch before the create so the parent's optimistic upsert can be
		// refused if a projection resync lands mid-create (BUG-2098).
		const sinceEpoch = localIndex.scopeEpochFor(ws);

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
			const item = await api.items.create(ws, selectedCollectionSlug, {
				title: trimmedTitle,
				content,
				fields: JSON.stringify(defaultFields),
				source: 'web',
			});

			// The editor was swapped out (item switch) during the create — the
			// item WAS created (SSE/reconcile will index it), but do NOT insert
			// its wiki-link into the now-different document, nor fire the
			// callback/toast for it. Reset the persistent menu's form directly:
			// hide() early-returns while `creating` is still true (the normal
			// path hides via the post-insert selectionUpdate, which never fires
			// here), so A's create form/selection would otherwise linger over B
			// (Codex).
			//
			// `!originEditor.isEditable` is the master-freeze arm (PLAN-2179 DR-1
			// / TASK-2180): the peeking `{#key}` remount used to destroy this
			// editor mid-create (caught by `isDestroyed`); now the editor stays
			// alive and merely goes read-only, so we must drop the wiki-link insert
			// explicitly — a frozen master must not be mutated by an in-flight
			// create the user started before the pane opened.
			if (originEditor.isDestroyed || editor !== originEditor || !originEditor.isEditable) {
				visible = false;
				showForm = false;
				title = '';
				errorMsg = '';
				return;
			}

			// Replace the selection with a wiki-link
			const wikiLink = `[[${item.title}]]`;
			originEditor
				.chain()
				.focus()
				.deleteRange({ from: selFrom, to: selTo })
				.insertContentAt(selFrom, wikiLink)
				.run();

			onItemCreated?.(item, ws, sinceEpoch);
			toastStore.show(`Created "${item.title}"`, 'success');
			hide();
		} catch (err: any) {
			if (isPlanLimitError(err)) {
				toastStore.show(planLimitMessage(err) + ' Upgrade to Pro', 'error', 6000, '/console/billing');
			} else {
				errorMsg = err.message || 'Failed to create item';
			}
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
			{#if onComment}
				<!-- The visible label is "Comment" (a bubble menu has no room for
				     more), but the ACCESSIBLE name says what it acts on: the comment
				     composer's submit button is also named "Comment", and two
				     identically-named buttons doing different things is a real
				     ambiguity for anyone navigating by name rather than by sight. -->
				<button class="comment-btn" aria-label="Comment on selection" onclick={quoteSelection}>
					<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
						<path d="M21 11.5a8.38 8.38 0 0 1-.9 3.8 8.5 8.5 0 0 1-7.6 4.7 8.38 8.38 0 0 1-3.8-.9L3 21l1.9-5.7a8.38 8.38 0 0 1-.9-3.8 8.5 8.5 0 0 1 4.7-7.6 8.38 8.38 0 0 1 3.8-.9h.5a8.48 8.48 0 0 1 8 8v.5z" />
					</svg>
					Comment
				</button>
			{/if}
		{/if}
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

	.comment-btn,
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

	.comment-btn:hover,
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
		color: var(--accent-red);
		padding: 0 var(--space-1);
	}
</style>
