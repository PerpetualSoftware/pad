<script lang="ts">
	import { page } from '$app/state';
	import { tick } from 'svelte';
	import { api } from '$lib/api/client';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import Editor from '$lib/components/editor/Editor.svelte';
	import EditorBubbleMenu from '$lib/components/editor/EditorBubbleMenu.svelte';
	import EditorLinkPopover from '$lib/components/editor/EditorLinkPopover.svelte';
	import RawMarkdownEditor from '$lib/components/editor/RawMarkdownEditor.svelte';
	import type { Editor as EditorType } from '@tiptap/core';
	import FieldEditor from '$lib/components/fields/FieldEditor.svelte';
	import VersionHistory from '$lib/components/versions/VersionHistory.svelte';
	import CommentThread from '$lib/components/comments/CommentThread.svelte';
	import PhaseTasks from '$lib/components/phases/PhaseTasks.svelte';
	import { goto } from '$app/navigation';
	import { relativeTime, wikiLinksToMarkdown, markdownToWikiLinks, cleanBrokenLinks } from '$lib/utils/markdown';
	import { toastStore } from '$lib/stores/toast.svelte';
	import type { Item, Collection, CollectionSettings, QuickAction } from '$lib/types';
	import { parseFields, parseSchema, parseSettings, formatItemRef } from '$lib/types';
	import QuickActionsMenu from '$lib/components/common/QuickActionsMenu.svelte';

	let wsSlug = $derived(page.params.workspace ?? '');
	let collSlug = $derived(page.params.collection ?? '');
	let itemSlug = $derived(page.params.slug ?? '');

	let item = $state<Item | null>(null);
	let collection = $state<Collection | null>(null);
	let loading = $state(true);
	let error = $state('');

	let editorInstance = $state<EditorType | null>(null);

	let editingTitle = $state(false);
	let titleDraft = $state('');
	let titleInputEl = $state<HTMLInputElement>();

	let fields = $derived<Record<string, any>>(item ? parseFields(item) : {});
	let schema = $derived(collection ? parseSchema(collection) : { fields: [] });
	let settings = $derived<CollectionSettings>(collection ? parseSettings(collection) : { layout: 'balanced', default_view: 'list' });
	let layout = $derived(settings.layout);
	let quickActions = $derived<QuickAction[]>(settings.quick_actions ?? []);

	// Convert wiki-links to markdown links for the editor
	let editorContent = $derived.by(() => {
		if (!item) return '';
		const raw = item.content ?? '';
		const allItems = collectionStore.items ?? [];
		if (allItems.length > 0 && raw.includes('[[')) {
			return wikiLinksToMarkdown(raw, allItems, wsSlug);
		}
		return raw;
	});

	let contentDebounceTimer: ReturnType<typeof setTimeout> | undefined;
	let saveStatus = $state<'idle' | 'saving' | 'saved'>('idle');
	let saveStatusTimer: ReturnType<typeof setTimeout> | undefined;
	let showHistory = $state(false);
	let confirmDelete = $state(false);
	let deleting = $state(false);
	let rawMode = $state(false);
	let showMoveMenu = $state(false);
	let moving = $state(false);
	let itemLinks = $state<import('$lib/types').ItemLink[]>([]);

	$effect(() => {
		if (wsSlug && collSlug && itemSlug) {
			loadData();
		}
	});

	async function loadData() {
		loading = true;
		error = '';
		try {
			const [itemData, collData] = await Promise.all([
				api.items.get(wsSlug, itemSlug),
				api.collections.get(wsSlug, collSlug)
			]);
			item = itemData;
			collection = collData;

			// Fetch real progress for phases
			if (collSlug === 'phases' && itemData) {
				try {
					const progress = await api.items.phasesProgress(wsSlug);
					const match = progress.find(p => p.phase_id === itemData.id);
					if (match) {
						const pct = match.total > 0 ? Math.round((match.done / match.total) * 100) : 0;
						computedOverrides = { progress: pct, _progressDone: match.done, _progressTotal: match.total };
					} else {
						computedOverrides = { progress: 0, _progressDone: 0, _progressTotal: 0 };
					}
				} catch {
					computedOverrides = {};
				}
			} else {
				computedOverrides = {};
			}

			// Also load items for wiki-link resolution if not already loaded
			if ((collectionStore.items ?? []).length === 0) {
				collectionStore.loadItems(wsSlug);
			}

			// Load links for this item
			try {
				itemLinks = await api.links.list(wsSlug, itemData.slug);
			} catch { itemLinks = []; }
		} catch (e: any) {
			error = e.message ?? 'Failed to load item';
		} finally {
			loading = false;

			// Auto-start title editing for newly created items
			if (page.url.searchParams.get('new') === '1' && item) {
				// Clean up the URL param first, then focus title after DOM settles
				goto(`/${wsSlug}/${collSlug}/${itemSlug}`, { replaceState: true, noScroll: true });
				await startEditTitle();
			}
		}
	}

	async function startEditTitle() {
		if (!item) return;
		titleDraft = item.title;
		editingTitle = true;
		// Wait for the DOM to render the input, then focus
		await tick();
		titleInputEl?.focus();
		titleInputEl?.select();
	}

	function showSaved() {
		saveStatus = 'saved';
		clearTimeout(saveStatusTimer);
		saveStatusTimer = setTimeout(() => { saveStatus = 'idle'; }, 2000);
	}

	async function saveTitle() {
		editingTitle = false;
		if (!item || titleDraft.trim() === item.title) return;
		saveStatus = 'saving';
		try {
			item = await api.items.update(wsSlug, item.id, { title: titleDraft.trim() });
			showSaved();
		} catch {
			saveStatus = 'idle';
			toastStore.show('Failed to update title', 'error');
		}
	}

	function handleTitleKeydown(e: KeyboardEvent) {
		if (e.key === 'Enter') {
			e.preventDefault();
			saveTitle();
			// Move focus to the editor so you can start writing immediately
			requestAnimationFrame(() => editorInstance?.commands.focus());
		} else if (e.key === 'Escape') {
			editingTitle = false;
		}
	}

	async function updateField(key: string, value: any) {
		if (!item) return;
		const updated = { ...fields, [key]: value };
		saveStatus = 'saving';
		try {
			item = await api.items.update(wsSlug, item.id, { fields: JSON.stringify(updated) });
			showSaved();
		} catch {
			saveStatus = 'idle';
			toastStore.show('Failed to save', 'error');
		}
	}

	function handleContentUpdate(markdown: string) {
		clearTimeout(contentDebounceTimer);
		saveStatus = 'saving';
		contentDebounceTimer = setTimeout(() => {
			if (!item) return;
			const allItems = collectionStore.items ?? [];
			let toSave = markdown;
			if (allItems.length > 0) {
				toSave = markdownToWikiLinks(toSave, allItems);
			}
			toSave = cleanBrokenLinks(toSave);
			api.items.update(wsSlug, item.id, { content: toSave }).then(() => {
				// Don't overwrite item -- resetting editorContent would
				// clobber anything typed since the debounce started.
				showSaved();
			}).catch(() => {
				saveStatus = 'idle';
				toastStore.show('Failed to save content', 'error');
			});
		}, 500);
	}

	function handleRawContentUpdate(markdown: string) {
		clearTimeout(contentDebounceTimer);
		saveStatus = 'saving';
		contentDebounceTimer = setTimeout(() => {
			if (!item) return;
			// Raw mode: content is already in storage format (with [[wiki links]])
			api.items.update(wsSlug, item.id, { content: markdown }).then((updated) => {
				item = updated;
				showSaved();
			}).catch(() => {
				saveStatus = 'idle';
				toastStore.show('Failed to save content', 'error');
			});
		}, 500);
	}

	let computedOverrides = $state<Record<string, any>>({});

	function fieldValue(key: string): any {
		if (key in computedOverrides) return computedOverrides[key];
		return fields[key] ?? '';
	}

	function formatFieldDisplay(value: any): string {
		if (value === null || value === undefined || value === '') return '—';
		return String(value).replace(/_/g, ' ').replace(/\b\w/g, c => c.toUpperCase());
	}

	function handleVersionRestore(updatedItem: Item) {
		item = updatedItem;
		showHistory = false;
	}

	async function handleDelete() {
		if (!item) return;
		deleting = true;
		try {
			await api.items.delete(wsSlug, item.id);
			toastStore.show('Item deleted', 'success');
			goto(`/${wsSlug}/${collSlug}`);
		} catch {
			toastStore.show('Failed to delete item', 'error');
			deleting = false;
			confirmDelete = false;
		}
	}

	let allCollections = $derived(collectionStore.collections ?? []);
	let moveTargets = $derived(allCollections.filter(c => c.slug !== collSlug));

	async function handleMove(targetSlug: string) {
		if (!item || moving) return;
		moving = true;
		showMoveMenu = false;
		try {
			const moved = await api.items.move(wsSlug, item.slug, targetSlug);
			toastStore.show(`Moved to ${targetSlug}`, 'success');
			goto(`/${wsSlug}/${targetSlug}/${moved.slug}`);
		} catch (e: any) {
			toastStore.show(e.message ?? 'Failed to move item', 'error');
		} finally {
			moving = false;
		}
	}
</script>

{#if loading}
	<div class="center-message">Loading...</div>
{:else if error}
	<div class="center-message">{error}</div>
{:else if item && collection}
	<div class="item-page">
		<!-- Breadcrumb -->
		<nav class="breadcrumb">
			<a href="/{wsSlug}">Home</a>
			<span class="sep">/</span>
			<a href="/{wsSlug}/{collSlug}">{collection.icon} {collection.name}</a>
			<span class="sep">/</span>
			<span class="current">{formatItemRef(item) || item.title}</span>
		</nav>

		<!-- Title -->
		<div class="title-row">
			{#if formatItemRef(item)}
				<span class="item-ref">{formatItemRef(item)}</span>
			{/if}
			{#if editingTitle}
				<input
					class="title-input"
					bind:this={titleInputEl}
					bind:value={titleDraft}
					onblur={saveTitle}
					onkeydown={handleTitleKeydown}
				/>
			{:else}
				<button class="title" onclick={startEditTitle}>
					{item.title}
				</button>
			{/if}
		</div>

		<!-- Meta -->
		<div class="meta">
			<span title={new Date(item.created_at).toLocaleString()}>Created {relativeTime(item.created_at)} by {item.created_by || 'unknown'}</span>
			<span class="meta-sep">·</span>
			<span title={new Date(item.updated_at).toLocaleString()}>Updated {relativeTime(item.updated_at)}</span>
			{#if saveStatus === 'saving'}
				<span class="save-status saving">Saving...</span>
			{:else if saveStatus === 'saved'}
				<span class="save-status saved">✓ Saved</span>
			{/if}
			<div class="meta-actions">
				{#if quickActions.length > 0 && collection}
					<QuickActionsMenu actions={quickActions} {item} {collection} scope="item" />
				{/if}
				<button
					class="history-btn"
					class:active={showHistory}
					onclick={() => { showHistory = !showHistory; }}
				>
					History
				</button>
				<div class="move-wrapper">
					<button class="history-btn" onclick={() => { showMoveMenu = !showMoveMenu; }} disabled={moving}>
						{moving ? 'Moving...' : 'Move to...'}
					</button>
					{#if showMoveMenu}
						<div class="move-dropdown">
							{#each moveTargets as target (target.slug)}
								<button class="move-option" onclick={() => handleMove(target.slug)}>
									{#if target.icon}<span class="move-icon">{target.icon}</span>{/if}
									{target.name}
								</button>
							{/each}
						</div>
					{/if}
				</div>
				{#if confirmDelete}
					<span class="delete-confirm">
						Delete this item?
						<button class="delete-confirm-btn yes" disabled={deleting} onclick={handleDelete}>
							{deleting ? '...' : 'Yes'}
						</button>
						<button class="delete-confirm-btn no" onclick={() => { confirmDelete = false; }}>
							No
						</button>
					</span>
				{:else}
					<button class="history-btn delete-btn" onclick={() => { confirmDelete = true; }}>
						Delete
					</button>
				{/if}
			</div>
		</div>

		<!-- Layout wrapper -->
		<div class="item-body layout-{layout}">
			<!-- Fields -->
			<div class="fields-panel">
				<div class="fields-header">Properties</div>
				{#each schema.fields as field (field.key)}
					{#if field.computed}
						<div class="field-row">
							<span class="field-label">{field.label}</span>
							<div class="field-value">
								{#if field.type === 'number' && field.suffix === '%'}
									{@const pct = Math.min(100, Math.max(0, Number(fieldValue(field.key)) || 0))}
									{@const done = computedOverrides._progressDone}
									{@const total = computedOverrides._progressTotal}
									<div class="progress-bar">
										<div class="progress-fill" style:width="{pct}%"></div>
										<span class="progress-text">
											{#if total != null}
												{done}/{total} tasks · {pct}%
											{:else}
												{pct}%
											{/if}
										</span>
									</div>
								{:else}
									<span class="computed-value">{formatFieldDisplay(fieldValue(field.key))}</span>
								{/if}
							</div>
						</div>
					{:else}
						<div class="field-row">
							<span class="field-label">{field.label}</span>
							<div class="field-value">
								<FieldEditor
									{field}
									value={fieldValue(field.key)}
									onchange={(v) => updateField(field.key, v)}
									{wsSlug}
								/>
							</div>
						</div>
					{/if}
				{/each}
			</div>

			<!-- Content editor -->
			<div class="content-panel">
				<div class="editor-mode-toggle">
					<button
						class="mode-btn"
						class:active={!rawMode}
						onclick={() => rawMode = false}
						title="Rich text editor"
					>Rich</button>
					<button
						class="mode-btn"
						class:active={rawMode}
						onclick={() => rawMode = true}
						title="Raw markdown editor"
					>Markdown</button>
				</div>
				{#if rawMode}
					{#key item.id}
						<RawMarkdownEditor content={item.content ?? ''} onUpdate={handleRawContentUpdate} />
					{/key}
				{:else}
					{#key item.id}
						<Editor content={editorContent} onUpdate={handleContentUpdate} editable={true} onEditor={(e) => editorInstance = e} />
					{/key}
					<EditorBubbleMenu
						editor={editorInstance}
						{wsSlug}
						collections={collectionStore.collections}
						onItemCreated={() => collectionStore.loadItems(wsSlug)}
					/>
					<EditorLinkPopover editor={editorInstance} />
				{/if}
			</div>
		</div>

		<!-- Relationships -->
		{#if itemLinks.length > 0}
			<div class="relationships-section">
				<h3 class="section-title">Relationships</h3>
				<div class="links-list">
					{#each itemLinks as link (link.id)}
						{@const isSource = link.source_id === item?.id}
						{@const targetTitle = isSource ? link.target_title : link.source_title}
						{@const linkLabel = link.link_type === 'blocks'
							? (isSource ? 'Blocks' : 'Blocked by')
							: link.link_type === 'wiki_link'
								? (isSource ? 'Links to' : 'Referenced by')
								: link.link_type || 'Related to'}
						<div class="link-row" class:blocking={link.link_type === 'blocks' && !isSource}>
							<span class="link-type" class:type-blocks={link.link_type === 'blocks'} class:type-wiki={link.link_type === 'wiki_link'}>{linkLabel}</span>
							<a href="/{wsSlug}/{link.link_type === 'blocks' ? 'tasks' : collSlug}/{isSource ? link.target_title : link.source_title}" class="link-target">{targetTitle || 'Unknown item'}</a>
						</div>
					{/each}
				</div>
			</div>
		{/if}

		<!-- Phase Tasks (shown only for phases collection) -->
		{#if collSlug === 'phases' && item}
			<PhaseTasks {wsSlug} {itemSlug} itemId={item.id} phaseFields={fields} />
		{/if}

		<!-- Comments -->
		<div class="comments-section">
			<CommentThread {wsSlug} {itemSlug} items={collectionStore.items ?? []} />
		</div>

	</div>

	<!-- Version History Modal -->
	{#if showHistory}
		<!-- svelte-ignore a11y_no_static_element_interactions a11y_click_events_have_key_events -->
		<div class="modal-backdrop" onclick={() => { showHistory = false; }}>
			<div class="modal-container" onclick={(e) => e.stopPropagation()}>
				<VersionHistory
					{wsSlug}
					{itemSlug}
					currentContent={item.content ?? ''}
					onRestore={handleVersionRestore}
					onClose={() => { showHistory = false; }}
				/>
			</div>
		</div>
	{/if}
{/if}

<style>
	.center-message {
		display: flex;
		align-items: center;
		justify-content: center;
		height: 50vh;
		color: var(--text-muted);
	}

	.item-page {
		max-width: var(--content-max-width);
		margin: 0 auto;
		padding: var(--space-6) var(--space-6) var(--space-10);
	}

	/* Breadcrumb */
	.breadcrumb {
		display: flex;
		align-items: center;
		gap: var(--space-1);
		font-size: 0.85em;
		color: var(--text-muted);
		margin-bottom: var(--space-4);
	}
	.breadcrumb a {
		color: var(--text-secondary);
		text-decoration: none;
	}
	.breadcrumb a:hover {
		color: var(--accent-blue);
		text-decoration: underline;
	}
	.sep { color: var(--text-muted); }
	.current { color: var(--text-primary); }

	/* Title */
	.title-row { margin-bottom: var(--space-2); display: flex; align-items: baseline; gap: var(--space-2); }
	.item-ref {
		font-family: var(--font-mono);
		font-size: 0.85em;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		padding: 2px 8px;
		border-radius: var(--radius-sm);
		white-space: nowrap;
		flex-shrink: 0;
	}
	.title {
		display: block;
		font-size: 1.6em;
		font-weight: 700;
		cursor: text;
		border-radius: var(--radius);
		padding: 2px 4px;
		margin: -2px -4px;
		text-align: left;
		width: 100%;
		color: var(--text-primary);
		background: none;
		border: none;
	}
	.title:hover {
		background: var(--bg-secondary);
	}
	.title-input {
		font-size: 1.6em;
		font-weight: 700;
		width: 100%;
		background: var(--bg-secondary);
		border: 1px solid var(--accent-blue);
		border-radius: var(--radius);
		padding: 2px 4px;
		color: var(--text-primary);
	}

	/* Meta */
	.meta {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.8em;
		color: var(--text-muted);
		margin-bottom: var(--space-6);
	}
	.meta-sep { color: var(--text-muted); }
	.save-status {
		font-size: 0.85em;
		margin-left: var(--space-2);
		transition: opacity 0.2s;
	}
	.save-status.saving { color: var(--text-muted); }
	.save-status.saved { color: var(--accent-green); }

	/* Layout variants */
	.item-body {
		display: flex;
		flex-direction: column;
		gap: var(--space-6);
	}

	.layout-balanced .fields-panel {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: 0 var(--space-6);
		padding-bottom: var(--space-4);
		border-bottom: 1px solid var(--border);
	}
	.layout-balanced .fields-header {
		grid-column: 1 / -1;
	}
	.layout-balanced .field-row:last-child {
		border-bottom: none;
	}

	.layout-fields-primary .fields-panel {
		order: -1;
	}

	/* Content-primary: fields as compact horizontal row */
	.layout-content-primary .fields-panel {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		padding-bottom: var(--space-4);
		border-bottom: 1px solid var(--border);
	}
	.layout-content-primary .fields-header {
		display: none;
	}
	.layout-content-primary .field-row {
		flex-direction: row;
		align-items: center;
		gap: var(--space-2);
		padding: 0;
		border: none;
	}
	.layout-content-primary .field-label {
		font-size: 0.75em;
		white-space: nowrap;
	}
	/* Fields panel */
	.fields-panel {
		display: flex;
		flex-direction: column;
		gap: 0;
	}
	.fields-header {
		font-size: 0.7em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
		padding: var(--space-2) 0;
		margin-bottom: var(--space-1);
	}
	.field-row {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-2) 0;
		border-bottom: 1px solid color-mix(in srgb, var(--border) 50%, transparent);
	}
	.field-row:last-child {
		border-bottom: none;
	}
	.field-label {
		font-size: 0.82em;
		color: var(--text-secondary);
		font-weight: 500;
		width: 90px;
		flex-shrink: 0;
	}
	.field-value {
		flex: 1;
		min-width: 0;
	}
	.computed-value {
		font-size: 0.88em;
		color: var(--text-secondary);
	}
	.progress-bar {
		position: relative;
		height: 22px;
		background: var(--bg-tertiary);
		border-radius: var(--radius-sm);
		overflow: hidden;
	}
	.progress-fill {
		height: 100%;
		background: var(--accent-blue);
		opacity: 0.25;
		border-radius: var(--radius-sm);
		transition: width 0.3s ease;
	}
	.progress-text {
		position: absolute;
		inset: 0;
		display: flex;
		align-items: center;
		justify-content: center;
		font-size: 0.8em;
		font-weight: 500;
		color: var(--text-primary);
	}

	/* Content */
	.content-panel {
		min-height: 300px;
	}

	.editor-mode-toggle {
		display: flex;
		gap: 1px;
		margin-bottom: var(--space-3);
		background: var(--bg-tertiary);
		border-radius: var(--radius-sm);
		padding: 2px;
		width: fit-content;
	}

	.mode-btn {
		padding: var(--space-1) var(--space-3);
		font-size: 0.75em;
		font-weight: 500;
		color: var(--text-muted);
		background: none;
		border: none;
		border-radius: var(--radius-sm);
		cursor: pointer;
		transition: color 0.15s, background 0.15s;
	}

	.mode-btn:hover {
		color: var(--text-secondary);
	}

	.mode-btn.active {
		background: var(--bg-secondary);
		color: var(--text-primary);
		box-shadow: 0 1px 2px rgba(0, 0, 0, 0.1);
	}

	/* Relationships */
	.relationships-section {
		margin-top: var(--space-6);
		padding-top: var(--space-6);
		border-top: 1px solid var(--border);
	}
	.section-title {
		font-size: 0.8em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.06em;
		color: var(--text-muted);
		margin-bottom: var(--space-3);
	}
	.links-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.link-row {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-2) var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		font-size: 0.9em;
	}
	.link-row.blocking {
		border-left: 3px solid var(--accent-orange);
	}
	.link-type {
		font-size: 0.75em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--text-muted);
		min-width: 80px;
	}
	.link-type.type-blocks { color: var(--accent-orange); }
	.link-type.type-wiki { color: var(--accent-blue); }
	.link-target {
		font-weight: 500;
		color: var(--text-primary);
		text-decoration: none;
	}
	.link-target:hover {
		color: var(--accent-blue);
		text-decoration: underline;
	}

	/* Comments */
	.comments-section {
		margin-top: var(--space-6);
		padding-top: var(--space-6);
		border-top: 1px solid var(--border);
	}

	/* History */
	.meta-actions {
		margin-left: auto;
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}
	.history-btn {
		padding: 2px var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.85em;
		cursor: pointer;
		transition: all 0.1s;
	}
	.history-btn:hover {
		background: var(--bg-tertiary);
		color: var(--text-primary);
	}
	.history-btn.active {
		background: var(--accent-blue);
		border-color: var(--accent-blue);
		color: #fff;
	}
	.move-wrapper {
		position: relative;
	}
	.move-dropdown {
		position: absolute;
		top: 100%;
		right: 0;
		margin-top: var(--space-1);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		box-shadow: 0 4px 12px rgba(0, 0, 0, 0.3);
		z-index: 100;
		min-width: 180px;
		padding: var(--space-1);
	}
	.move-option {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		width: 100%;
		padding: var(--space-2) var(--space-3);
		background: none;
		border: none;
		color: var(--text-primary);
		font-size: 0.85em;
		cursor: pointer;
		border-radius: var(--radius-sm);
		text-align: left;
	}
	.move-option:hover {
		background: var(--bg-hover);
	}
	.move-icon {
		font-size: 1.1em;
	}
	.delete-btn:hover {
		color: var(--accent-orange);
		border-color: var(--accent-orange);
	}
	.delete-confirm {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.85em;
		color: var(--accent-orange);
		font-weight: 500;
	}
	.delete-confirm-btn {
		padding: 2px var(--space-2);
		border-radius: var(--radius);
		font-size: 0.85em;
		cursor: pointer;
		border: 1px solid var(--border);
		background: var(--bg-secondary);
		color: var(--text-secondary);
	}
	.delete-confirm-btn.yes {
		color: var(--accent-orange);
		border-color: var(--accent-orange);
	}
	.delete-confirm-btn.yes:hover {
		background: var(--accent-orange);
		color: #fff;
	}
	.delete-confirm-btn.no:hover {
		background: var(--bg-tertiary);
		color: var(--text-primary);
	}
	.delete-confirm-btn:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
	.modal-backdrop {
		position: fixed;
		inset: 0;
		background: rgba(0, 0, 0, 0.5);
		z-index: 100;
		display: flex;
		align-items: center;
		justify-content: center;
		padding: var(--space-4);
	}
	.modal-container {
		width: 100%;
		max-width: 720px;
		max-height: 85vh;
		display: flex;
		flex-direction: column;
	}
	.modal-container :global(.version-panel) {
		max-height: 85vh;
	}
	@media (max-width: 768px) {
		.modal-backdrop {
			padding: var(--space-2);
		}
		.modal-container {
			max-height: 92vh;
		}
	}

	@media (max-width: 768px) {
		.layout-balanced .fields-panel {
			grid-template-columns: 1fr;
		}
	}
</style>
