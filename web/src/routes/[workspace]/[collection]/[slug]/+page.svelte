<script lang="ts">
	import { page } from '$app/state';
	import { tick, onMount, onDestroy } from 'svelte';
	import { api } from '$lib/api/client';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { visibility } from '$lib/services/visibility.svelte';
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
	import type { Item, Collection, CollectionSettings, QuickAction, ItemLink, ItemRelationRef, ItemImplementationNote, ItemDecisionLogEntry } from '$lib/types';
	import { parseFields, parseSchema, parseSettings, formatItemRef } from '$lib/types';
	import QuickActionsMenu from '$lib/components/common/QuickActionsMenu.svelte';

	type RelationshipEntry = {
		key: string;
		label: string;
		href: string | null;
		status?: string;
	};

	type RelationshipGroup = {
		label: string;
		tone: 'default' | 'blocks' | 'wiki' | 'lineage';
		entries: RelationshipEntry[];
	};

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
	let itemLinks = $state<ItemLink[]>([]);
	let relationshipGroups = $derived(item ? buildRelationshipGroups(item, itemLinks) : []);
	let closureEntries = $derived(item?.derived_closure?.related_items?.map((related) => relationRefEntry(related)) ?? []);
	let codeContext = $derived(item?.code_context ?? null);
	let implementationNotes = $derived(item?.implementation_notes ?? []);
	let decisionLog = $derived(item?.decision_log ?? []);
	let noteSummary = $state('');
	let noteDetails = $state('');
	let decisionTitle = $state('');
	let decisionRationale = $state('');
	let savingNote = $state(false);
	let savingDecision = $state(false);

	$effect(() => {
		if (wsSlug && collSlug && itemSlug) {
			loadData();
		}
	});

	// Refresh item when the tab regains focus (SSE events may have been lost)
	let unsubscribeVisibility: (() => void) | null = null;

	onMount(() => {
		unsubscribeVisibility = visibility.onTabResume(async () => {
			if (!wsSlug || !itemSlug || !item) return;
			// Don't refresh if the user is actively editing
			if (saveStatus === 'saving' || editingTitle) return;
			try {
				const updated = await api.items.get(wsSlug, itemSlug);
				// Merge server state without disrupting the editor:
				// update fields/metadata but preserve local content to avoid resetting the editor
				item = {
					...updated,
					content: item!.content
				};
				// Refresh links too
				itemLinks = await api.links.list(wsSlug, updated.slug).catch(() => []);
			} catch {
				// Ignore — will catch up on next event
			}
		});
	});

	onDestroy(() => {
		unsubscribeVisibility?.();
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

	function buildStructuredEntryID(prefix: string): string {
		if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
			return `${prefix}-${crypto.randomUUID()}`;
		}
		return `${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;
	}

	async function updateStructuredFields(updatedFields: Record<string, any>) {
		if (!item) return;
		saveStatus = 'saving';
		item = await api.items.update(wsSlug, item.id, { fields: JSON.stringify(updatedFields) });
		showSaved();
	}

	async function addImplementationNote() {
		if (!item || savingNote) return;
		const summary = noteSummary.trim();
		const details = noteDetails.trim();
		if (!summary) return;

		savingNote = true;
		try {
			const updatedFields = {
				...fields,
				implementation_notes: [
					...(implementationNotes ?? []),
					{
						id: buildStructuredEntryID('note'),
						summary,
						details: details || undefined,
						created_at: new Date().toISOString(),
						created_by: 'web'
					} satisfies ItemImplementationNote
				]
			};
			await updateStructuredFields(updatedFields);
			noteSummary = '';
			noteDetails = '';
			toastStore.show('Implementation note added', 'success');
		} catch {
			saveStatus = 'idle';
			toastStore.show('Failed to add implementation note', 'error');
		} finally {
			savingNote = false;
		}
	}

	async function addDecisionLogEntry() {
		if (!item || savingDecision) return;
		const decision = decisionTitle.trim();
		const rationale = decisionRationale.trim();
		if (!decision) return;

		savingDecision = true;
		try {
			const updatedFields = {
				...fields,
				decision_log: [
					...(decisionLog ?? []),
					{
						id: buildStructuredEntryID('decision'),
						decision,
						rationale: rationale || undefined,
						created_at: new Date().toISOString(),
						created_by: 'web'
					} satisfies ItemDecisionLogEntry
				]
			};
			await updateStructuredFields(updatedFields);
			decisionTitle = '';
			decisionRationale = '';
			toastStore.show('Decision log entry added', 'success');
		} catch {
			saveStatus = 'idle';
			toastStore.show('Failed to add decision log entry', 'error');
		} finally {
			savingDecision = false;
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

	function handlePhaseTasksChange(tasks: Item[]) {
		if (collSlug !== 'phases') return;
		const total = tasks.length;
		const done = tasks.filter((task) => parseFields(task).status === 'done').length;
		const progress = total > 0 ? Math.round((done / total) * 100) : 0;
		computedOverrides = { progress, _progressDone: done, _progressTotal: total };
	}

	function fieldValue(key: string): any {
		if (key in computedOverrides) return computedOverrides[key];
		return fields[key] ?? '';
	}

	function formatFieldDisplay(value: any): string {
		if (value === null || value === undefined || value === '') return '—';
		return String(value).replace(/_/g, ' ').replace(/\b\w/g, c => c.toUpperCase());
	}

	function relationLabel(ref?: string, title?: string, fallback?: string): string {
		if (ref && title) return `${ref} ${title}`;
		if (ref) return ref;
		if (title) return title;
		return fallback || 'Unknown item';
	}

	function relationHref(collectionSlug?: string, refOrSlug?: string): string | null {
		if (!collectionSlug || !refOrSlug) return null;
		return `/${wsSlug}/${collectionSlug}/${refOrSlug}`;
	}

	function relationRefEntry(related: ItemRelationRef): RelationshipEntry {
		return {
			key: related.id,
			label: relationLabel(related.ref, related.title, related.id),
			href: relationHref(related.collection_slug, related.ref ?? related.slug),
			status: related.status
		};
	}

	function linkEntry(link: ItemLink, useSource: boolean): RelationshipEntry {
		const ref = useSource ? link.source_ref : link.target_ref;
		const title = useSource ? link.source_title : link.target_title;
		const status = useSource ? link.source_status : link.target_status;
		const id = useSource ? link.source_id : link.target_id;
		const slug = useSource ? link.source_slug : link.target_slug;
		const collectionSlug = useSource ? link.source_collection_slug : link.target_collection_slug;
		const href = relationHref(collectionSlug, ref ?? slug);
		return {
			key: `${link.id}:${useSource ? 'source' : 'target'}`,
			label: relationLabel(ref, title, id),
			href,
			status
		};
	}

	function buildRelationshipGroups(currentItem: Item, links: ItemLink[]): RelationshipGroup[] {
		const grouped = new Map<string, RelationshipGroup>();
		const definitions: Record<string, { label: string; tone: RelationshipGroup['tone'] }> = {
			blocks: { label: 'Blocks', tone: 'blocks' },
			blocked_by: { label: 'Blocked by', tone: 'blocks' },
			links_to: { label: 'Links to', tone: 'wiki' },
			referenced_by: { label: 'Referenced by', tone: 'wiki' },
			split_from: { label: 'Split from', tone: 'lineage' },
			split_into: { label: 'Split into', tone: 'lineage' },
			supersedes: { label: 'Supersedes', tone: 'lineage' },
			superseded_by: { label: 'Superseded by', tone: 'lineage' },
			implements: { label: 'Implements', tone: 'lineage' },
			implemented_by: { label: 'Implemented by', tone: 'lineage' },
			related: { label: 'Related', tone: 'default' }
		};
		const order = ['blocks', 'blocked_by', 'links_to', 'referenced_by', 'split_from', 'split_into', 'supersedes', 'superseded_by', 'implements', 'implemented_by', 'related'];

		function addEntry(groupKey: string, entry: RelationshipEntry) {
			const definition = definitions[groupKey];
			if (!definition) return;
			if (!grouped.has(groupKey)) {
				grouped.set(groupKey, { label: definition.label, tone: definition.tone, entries: [] });
			}
			grouped.get(groupKey)?.entries.push(entry);
		}

		for (const link of links) {
			const isSource = link.source_id === currentItem.id;
			switch (link.link_type) {
				case 'blocks':
					addEntry(isSource ? 'blocks' : 'blocked_by', linkEntry(link, !isSource));
					break;
				case 'wiki_link':
					addEntry(isSource ? 'links_to' : 'referenced_by', linkEntry(link, !isSource));
					break;
				case 'split_from':
					addEntry(isSource ? 'split_from' : 'split_into', linkEntry(link, !isSource));
					break;
				case 'supersedes':
					addEntry(isSource ? 'supersedes' : 'superseded_by', linkEntry(link, !isSource));
					break;
				case 'implements':
					addEntry(isSource ? 'implements' : 'implemented_by', linkEntry(link, !isSource));
					break;
				default:
					addEntry('related', linkEntry(link, !isSource));
					break;
			}
		}

		return order
			.map((key) => grouped.get(key))
			.filter((group): group is RelationshipGroup => Boolean(group && group.entries.length > 0));
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

		<!-- Meta info -->
		<div class="meta-info">
			<span title={new Date(item.created_at).toLocaleString()}>Created {relativeTime(item.created_at)} by {item.created_by || 'unknown'}</span>
			<span class="meta-sep">·</span>
			<span title={new Date(item.updated_at).toLocaleString()}>Updated {relativeTime(item.updated_at)}</span>
			<span class="save-status" class:saving={saveStatus === 'saving'} class:saved={saveStatus === 'saved'} class:visible={saveStatus !== 'idle'}>
				{#if saveStatus === 'saving'}Saving...{:else}✓ Saved{/if}
			</span>
		</div>

		<!-- Actions -->
		<div class="meta-actions">
			{#if quickActions.length > 0 && collection}
				<QuickActionsMenu actions={quickActions} {item} {collection} scope="item" />
			{/if}
			<button
				class="action-btn"
				class:active={showHistory}
				onclick={() => { showHistory = !showHistory; }}
			>
				History
			</button>
			<div class="move-wrapper">
				<button class="action-btn" onclick={() => { showMoveMenu = !showMoveMenu; }} disabled={moving}>
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
				<button class="action-btn delete-btn" onclick={() => { confirmDelete = true; }}>
					Delete
				</button>
			{/if}
		</div>

		{#if codeContext}
			<div class="code-context-section">
				<h3 class="section-title">Code Context</h3>
				<div class="code-context-card">
					<div class="code-context-meta">
						<span class="code-provider">{formatFieldDisplay(codeContext.provider)}</span>
						{#if codeContext.repo}
							<span class="code-chip">{codeContext.repo}</span>
						{/if}
						{#if codeContext.branch}
							<span class="code-chip">{codeContext.branch}</span>
						{/if}
					</div>
					{#if codeContext.pull_request}
						<div class="code-pr-row">
							<a href={codeContext.pull_request.url} class="code-pr-link" target="_blank" rel="noreferrer">
								PR #{codeContext.pull_request.number}: {codeContext.pull_request.title}
							</a>
							<span class="code-pr-state">{formatFieldDisplay(codeContext.pull_request.state)}</span>
						</div>
						{#if codeContext.pull_request.updated_at}
							<div class="code-pr-updated">
								Updated {relativeTime(codeContext.pull_request.updated_at)}
							</div>
						{/if}
					{/if}
				</div>
			</div>
		{/if}

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
		{#if item.derived_closure}
			<div class="closure-notice">
				<div class="closure-notice-header">
					<h3 class="section-title">Derived Closure</h3>
					<span class="closure-kind">{formatFieldDisplay(item.derived_closure.kind)}</span>
				</div>
				<p class="closure-summary">{item.derived_closure.summary}</p>
				{#if closureEntries.length > 0}
					<div class="closure-related-list">
						{#each closureEntries as related (related.key)}
							<div class="closure-related-item">
								{#if related.href}
									<a href={related.href} class="closure-related-link">{related.label}</a>
								{:else}
									<span class="closure-related-link">{related.label}</span>
								{/if}
								{#if related.status}
									<span class="link-status">{formatFieldDisplay(related.status)}</span>
								{/if}
							</div>
						{/each}
					</div>
				{/if}
			</div>
		{/if}

		{#if relationshipGroups.length > 0}
			<div class="relationships-section">
				<h3 class="section-title">Relationships</h3>
				<div class="relationship-groups">
					{#each relationshipGroups as group (group.label)}
						<div class="relationship-group">
							<h4 class="relationship-group-title">{group.label}</h4>
							<div class="links-list">
								{#each group.entries as entry (entry.key)}
									<div class="link-row" class:tone-blocks={group.tone === 'blocks'} class:tone-wiki={group.tone === 'wiki'} class:tone-lineage={group.tone === 'lineage'}>
										{#if entry.href}
											<a href={entry.href} class="link-target">{entry.label}</a>
										{:else}
											<span class="link-target">{entry.label}</span>
										{/if}
										{#if entry.status}
											<span class="link-status">{formatFieldDisplay(entry.status)}</span>
										{/if}
									</div>
								{/each}
							</div>
						</div>
					{/each}
				</div>
			</div>
		{/if}

		<div class="structured-notes-section">
			<div class="structured-notes-grid">
				<section class="structured-card">
					<div class="structured-card-header">
						<h3 class="section-title">Implementation Notes</h3>
						<span class="structured-count">{implementationNotes.length}</span>
					</div>
					<div class="structured-entry-form">
						<input
							bind:value={noteSummary}
							class="structured-input"
							type="text"
							placeholder="What changed or mattered?"
							onkeydown={(e) => {
								if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') addImplementationNote();
							}}
						/>
						<textarea
							bind:value={noteDetails}
							class="structured-textarea"
							rows="3"
							placeholder="Optional details, tradeoffs, or follow-up context"
						></textarea>
						<button class="structured-submit" onclick={addImplementationNote} disabled={savingNote || !noteSummary.trim()}>
							{savingNote ? 'Saving…' : 'Add note'}
						</button>
					</div>
					{#if implementationNotes.length > 0}
						<div class="structured-entry-list">
							{#each implementationNotes as note (note.id ?? `${note.summary}-${note.created_at ?? ''}`)}
								<article class="structured-entry">
									<div class="structured-entry-title">{note.summary}</div>
									<div class="structured-entry-meta">
										{#if note.created_at}
											<span>{relativeTime(note.created_at)}</span>
										{/if}
										{#if note.created_by}
											<span>{note.created_by}</span>
										{/if}
									</div>
									{#if note.details}
										<p class="structured-entry-body">{note.details}</p>
									{/if}
								</article>
							{/each}
						</div>
					{:else}
						<p class="structured-empty">No implementation notes yet.</p>
					{/if}
				</section>

				<section class="structured-card">
					<div class="structured-card-header">
						<h3 class="section-title">Decision Log</h3>
						<span class="structured-count">{decisionLog.length}</span>
					</div>
					<div class="structured-entry-form">
						<input
							bind:value={decisionTitle}
							class="structured-input"
							type="text"
							placeholder="What decision did we make?"
							onkeydown={(e) => {
								if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') addDecisionLogEntry();
							}}
						/>
						<textarea
							bind:value={decisionRationale}
							class="structured-textarea"
							rows="3"
							placeholder="Optional rationale, tradeoffs, or alternatives considered"
						></textarea>
						<button class="structured-submit" onclick={addDecisionLogEntry} disabled={savingDecision || !decisionTitle.trim()}>
							{savingDecision ? 'Saving…' : 'Add decision'}
						</button>
					</div>
					{#if decisionLog.length > 0}
						<div class="structured-entry-list">
							{#each decisionLog as decision (decision.id ?? `${decision.decision}-${decision.created_at ?? ''}`)}
								<article class="structured-entry">
									<div class="structured-entry-title">{decision.decision}</div>
									<div class="structured-entry-meta">
										{#if decision.created_at}
											<span>{relativeTime(decision.created_at)}</span>
										{/if}
										{#if decision.created_by}
											<span>{decision.created_by}</span>
										{/if}
									</div>
									{#if decision.rationale}
										<p class="structured-entry-body">{decision.rationale}</p>
									{/if}
								</article>
							{/each}
						</div>
					{:else}
						<p class="structured-empty">No decisions recorded yet.</p>
					{/if}
				</section>
			</div>
		</div>

		<!-- Phase Tasks (shown only for phases collection) -->
		{#if collSlug === 'phases' && item}
			<PhaseTasks {wsSlug} {itemSlug} itemId={item.id} phaseFields={fields} onTasksChange={handlePhaseTasksChange} />
		{/if}

		<!-- Comments -->
		<div class="comments-section">
			<CommentThread {wsSlug} {itemSlug} items={collectionStore.items ?? []} />
		</div>

	</div>

	<!-- Version History Modal -->
	{#if showHistory}
		<div class="modal-backdrop" role="button" tabindex="0"
			onclick={() => { showHistory = false; }}
			onkeydown={(e) => { if (e.key === 'Escape' || e.key === 'Enter') { showHistory = false; } }}>
			<!-- svelte-ignore a11y_click_events_have_key_events -->
			<div class="modal-container" role="none" onclick={(e) => e.stopPropagation()}>
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
	.meta-info {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		font-size: 0.8em;
		color: var(--text-muted);
		margin-bottom: var(--space-2);
		flex-wrap: wrap;
	}
	.meta-sep { color: var(--text-muted); }
	.save-status {
		font-size: 0.85em;
		margin-left: var(--space-2);
		opacity: 0;
		transition: opacity 0.2s;
	}
	.save-status.visible { opacity: 1; }
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

	/* Code context */
	.code-context-section {
		margin-bottom: var(--space-6);
	}
	.code-context-card {
		padding: var(--space-4);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.code-context-meta {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		align-items: center;
	}
	.code-provider {
		font-size: 0.8em;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--accent-blue);
	}
	.code-chip {
		font-family: var(--font-mono);
		font-size: 0.8em;
		color: var(--text-secondary);
		background: var(--bg-tertiary);
		padding: 2px 8px;
		border-radius: 999px;
	}
	.code-pr-row {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
		flex-wrap: wrap;
	}
	.code-pr-link {
		font-weight: 600;
		color: var(--text-primary);
		text-decoration: none;
	}
	.code-pr-link:hover {
		color: var(--accent-blue);
		text-decoration: underline;
	}
	.code-pr-state {
		font-size: 0.75em;
		font-weight: 600;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		padding: 2px 8px;
		border-radius: 999px;
	}
	.code-pr-updated {
		font-size: 0.8em;
		color: var(--text-muted);
	}

	/* Derived closure */
	.closure-notice {
		margin-top: var(--space-6);
		padding: var(--space-4);
		background: color-mix(in srgb, var(--accent-green) 10%, var(--bg-secondary));
		border: 1px solid color-mix(in srgb, var(--accent-green) 35%, var(--border));
		border-radius: var(--radius);
	}
	.closure-notice-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
		margin-bottom: var(--space-2);
		flex-wrap: wrap;
	}
	.closure-kind {
		font-size: 0.75em;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--accent-green);
	}
	.closure-summary {
		margin: 0;
		color: var(--text-primary);
	}
	.closure-related-list {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		margin-top: var(--space-3);
	}
	.closure-related-item {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2) var(--space-3);
		background: var(--bg-primary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
	}
	.closure-related-link {
		font-weight: 500;
		color: var(--text-primary);
		text-decoration: none;
	}
	.closure-related-link:hover {
		color: var(--accent-blue);
		text-decoration: underline;
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
	.relationship-groups {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
	}
	.relationship-group {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.relationship-group-title {
		margin: 0;
		font-size: 0.95em;
		font-weight: 600;
		color: var(--text-secondary);
	}
	.link-row {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
		padding: var(--space-2) var(--space-3);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		font-size: 0.9em;
		flex-wrap: wrap;
	}
	.link-row.tone-blocks {
		border-left: 3px solid var(--accent-orange);
	}
	.link-row.tone-wiki {
		border-left: 3px solid var(--accent-blue);
	}
	.link-row.tone-lineage {
		border-left: 3px solid var(--accent-green);
	}
	.link-target {
		font-weight: 500;
		color: var(--text-primary);
		text-decoration: none;
	}
	.link-target:hover {
		color: var(--accent-blue);
		text-decoration: underline;
	}
	.link-status {
		font-size: 0.75em;
		font-weight: 600;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		padding: 2px 8px;
		border-radius: 999px;
		white-space: nowrap;
	}

	/* Structured notes */
	.structured-notes-section {
		margin-top: var(--space-6);
		padding-top: var(--space-6);
		border-top: 1px solid var(--border);
	}
	.structured-notes-grid {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
		gap: var(--space-4);
	}
	.structured-card {
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-4);
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.structured-card-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
	}
	.structured-card .section-title {
		margin-bottom: 0;
	}
	.structured-count {
		font-size: 0.75em;
		font-weight: 600;
		color: var(--text-muted);
		background: var(--bg-tertiary);
		padding: 2px 8px;
		border-radius: 999px;
	}
	.structured-entry-form {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}
	.structured-input,
	.structured-textarea {
		width: 100%;
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		background: var(--bg-primary);
		color: var(--text-primary);
		padding: var(--space-2) var(--space-3);
		font: inherit;
	}
	.structured-textarea {
		resize: vertical;
		min-height: 88px;
	}
	.structured-submit {
		align-self: flex-start;
		padding: var(--space-2) var(--space-3);
		background: var(--accent-blue);
		color: #fff;
		border: none;
		border-radius: var(--radius-sm);
		font-size: 0.9em;
		font-weight: 600;
		cursor: pointer;
	}
	.structured-submit:disabled {
		opacity: 0.5;
		cursor: default;
	}
	.structured-entry-list {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
	}
	.structured-entry {
		padding-top: var(--space-3);
		border-top: 1px solid color-mix(in srgb, var(--border) 60%, transparent);
	}
	.structured-entry:first-child {
		padding-top: 0;
		border-top: none;
	}
	.structured-entry-title {
		font-weight: 600;
		color: var(--text-primary);
	}
	.structured-entry-meta {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		margin-top: 4px;
		font-size: 0.78em;
		color: var(--text-muted);
	}
	.structured-entry-body {
		margin: var(--space-2) 0 0;
		color: var(--text-secondary);
		white-space: pre-wrap;
	}
	.structured-empty {
		margin: 0;
		color: var(--text-muted);
		font-size: 0.9em;
	}

	/* Comments */
	.comments-section {
		margin-top: var(--space-6);
		padding-top: var(--space-6);
		border-top: 1px solid var(--border);
	}

	/* History */
	.meta-actions {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		margin-bottom: var(--space-6);
		flex-wrap: wrap;
	}
	.action-btn {
		padding: var(--space-1) var(--space-3);
		min-width: 70px;
		text-align: center;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		color: var(--text-secondary);
		font-size: 0.85em;
		cursor: pointer;
		transition: all 0.1s;
		white-space: nowrap;
	}
	.action-btn:hover {
		background: var(--bg-tertiary);
		color: var(--text-primary);
	}
	.action-btn.active {
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
