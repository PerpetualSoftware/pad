<script lang="ts">
	import { page } from '$app/state';
	import { tick, onMount, onDestroy } from 'svelte';
	import { api } from '$lib/api/client';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { syncService } from '$lib/services/sync.svelte';
	import Editor from '$lib/components/editor/Editor.svelte';
	import EditorBubbleMenu from '$lib/components/editor/EditorBubbleMenu.svelte';
	import EditorLinkPopover from '$lib/components/editor/EditorLinkPopover.svelte';
	import RawMarkdownEditor from '$lib/components/editor/RawMarkdownEditor.svelte';
	import type { Editor as EditorType } from '@tiptap/core';
	import FieldEditor from '$lib/components/fields/FieldEditor.svelte';
	import ItemTimeline from '$lib/components/timeline/ItemTimeline.svelte';
	import ChildItems from '$lib/components/ChildItems.svelte';
	import { goto } from '$app/navigation';
	import { relativeTime, wikiLinksToMarkdown, markdownToWikiLinks, cleanBrokenLinks } from '$lib/utils/markdown';
	import { toastStore } from '$lib/stores/toast.svelte';
	import { editorStore } from '$lib/stores/editor.svelte';
	import type { Item, Collection, CollectionSettings, QuickAction, ItemLink, AgentRole } from '$lib/types';
	import { parseFields, parseSchema, parseSettings, formatItemRef, getTerminalOptions } from '$lib/types';
	import QuickActionsMenu from '$lib/components/common/QuickActionsMenu.svelte';
	import ShareDialog from '$lib/components/ShareDialog.svelte';
	import { copyToClipboard } from '$lib/utils/clipboard';
	import { authStore } from '$lib/stores/auth.svelte';

	type RelationshipEntry = {
		key: string;
		label: string;
		href: string | null;
		status?: string;
		linkId?: string;
	};

	type RelationshipGroup = {
		label: string;
		tone: 'default' | 'blocks' | 'wiki' | 'lineage';
		entries: RelationshipEntry[];
		closureSummary?: string;
	};

	let wsSlug = $derived(page.params.workspace ?? '');
	let username = $derived(page.params.username ?? '');
	let collSlug = $derived(page.params.collection ?? '');
	let itemSlug = $derived(page.params.slug ?? '');

	let item = $state<Item | null>(null);
	let collection = $state<Collection | null>(null);
	let loading = $state(true);
	let error = $state('');

	let editorInstance = $state<EditorType | null>(null);

	let editingTitle = $state(false);
	let titleDraft = $state('');
	let titleInputEl = $state<HTMLTextAreaElement>();

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
			return wikiLinksToMarkdown(raw, allItems, wsSlug, username);
		}
		return raw;
	});

	let contentDebounceTimer: ReturnType<typeof setTimeout> | undefined;
	let saveStatus = $state<'idle' | 'saving' | 'saved'>('idle');
	let saveStatusTimer: ReturnType<typeof setTimeout> | undefined;
	let confirmDelete = $state(false);
	let deleting = $state(false);
	let rawMode = $state(false);
	let showMoveMenu = $state(false);
	let moving = $state(false);
	let itemLinks = $state<ItemLink[]>([]);
	let workspaceMembers = $state<{ user_id: string; user_name: string; user_email: string; role: string }[]>([]);
	let shareDialogOpen = $state(false);
	let agentRoles = $state<AgentRole[]>([]);
	let childItemIds = $state<Set<string>>(new Set());
	let hasChildren = $state(false);
	let copied = $state(false);

	async function handleCopyRef() {
		const ref = formatItemRef(item!);
		if (!ref) return;
		const success = await copyToClipboard(ref);
		if (success) {
			copied = true;
			setTimeout(() => { copied = false; }, 1500);
		}
	}
	let relationshipGroups = $derived(item ? buildRelationshipGroups(item, itemLinks, childItemIds) : []);
	let codeContext = $derived(item?.code_context ?? null);
	let isOwner = $derived(workspaceMembers.some(m => m.user_id === authStore.userId && m.role === 'owner'));
	$effect(() => {
		if (wsSlug && collSlug && itemSlug) {
			loadData();
		}
	});

	// Sync coordinator — refresh item data on tab resume
	let unsubscribeSync: (() => void) | null = null;

	onMount(() => {
		unsubscribeSync = syncService.onSync(async (result) => {
			if (!wsSlug || !itemSlug || !item) return;
			// Don't refresh if the user is actively editing
			if (saveStatus === 'saving' || editingTitle) return;

			if (result.type === 'caught_up') return;

			if (result.type === 'incremental') {
				// Check if our item is in the changed set
				const updated = result.changes.updated.find(i => i.id === item!.id);
				if (updated) {
					// Merge server state without disrupting the editor
					item = {
						...updated,
						content: item!.content
					};
					itemLinks = await api.links.list(wsSlug, updated.slug).catch(() => []);
				}
				// Check if our item was deleted
				if (result.changes.deleted.includes(item!.id)) {
					// Item was deleted — navigate back to collection
					goto(`/${username}/${wsSlug}/${collSlug}`);
}
				return;
			}

			// Full refresh fallback
			try {
				const updated = await api.items.get(wsSlug, itemSlug);
				item = {
					...updated,
					content: item!.content
				};
				itemLinks = await api.links.list(wsSlug, updated.slug).catch(() => []);
				syncService.markSynced(); // Advance cursor now that reload succeeded
			} catch {
				// Ignore — will catch up on next event
			}
		});
	});

	onDestroy(() => {
		unsubscribeSync?.();
		editorStore.resetForDoc();
		collectionStore.setActiveItem(null);
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
			collectionStore.setActiveItem(itemData);
			editorStore.resetForDoc();

			// Fetch child item progress for any item (generalized parent/child)
			try {
				const progress = await api.items.progress(wsSlug, itemData.slug);
				if (progress.total > 0) {
					hasChildren = true;
					computedOverrides = { progress: progress.percentage, _progressDone: progress.done, _progressTotal: progress.total };
				} else {
					hasChildren = false;
					computedOverrides = {};
				}
			} catch {
				hasChildren = false;
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

			// Load workspace members and agent roles for assignment picker
			try {
				const membersData = await api.members.list(wsSlug);
				workspaceMembers = membersData.members ?? [];
			} catch { workspaceMembers = []; }
			try {
				agentRoles = await api.agentRoles.list(wsSlug);
			} catch { agentRoles = []; }
		} catch (e: any) {
			error = e.message ?? 'Failed to load item';
		} finally {
			loading = false;

			// Auto-start title editing for newly created items
			if (page.url.searchParams.get('new') === '1' && item) {
				// Clean up the URL param first, then focus title after DOM settles
				goto(`/${username}/${wsSlug}/${collSlug}/${itemSlug}`, { replaceState: true, noScroll: true });
				await startEditTitle();
			}
		}
	}

	async function startEditTitle() {
		if (!item) return;
		titleDraft = item.title;
		editingTitle = true;
		// Wait for the DOM to render the textarea, then focus + select all
		await tick();
		if (titleInputEl) {
			autoResizeTitle(titleInputEl);
			titleInputEl.focus();
			titleInputEl.setSelectionRange(0, titleInputEl.value.length);
		}
	}

	function autoResizeTitle(el: HTMLTextAreaElement) {
		el.style.height = 'auto';
		el.style.height = el.scrollHeight + 'px';
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

	async function updateAssignedUser(userId: string | null) {
		if (!item) return;
		saveStatus = 'saving';
		try {
			const update: Record<string, any> = {};
			if (userId) {
				update.assigned_user_id = userId;
			} else {
				update.clear_assigned_user = true;
			}
			item = await api.items.update(wsSlug, item.id, update);
			showSaved();
		} catch {
			saveStatus = 'idle';
			toastStore.show('Failed to update assignment', 'error');
		}
	}

	async function updateAgentRole(roleId: string | null) {
		if (!item) return;
		saveStatus = 'saving';
		try {
			const update: Record<string, any> = {};
			if (roleId) {
				update.agent_role_id = roleId;
			} else {
				update.clear_agent_role = true;
			}
			item = await api.items.update(wsSlug, item.id, update);
			showSaved();
		} catch {
			saveStatus = 'idle';
			toastStore.show('Failed to update role', 'error');
		}
	}

	function handleContentUpdate(markdown: string) {
		clearTimeout(contentDebounceTimer);
		editorStore.setDirty(true);
		contentDebounceTimer = setTimeout(() => {
			if (!item) return;
			saveStatus = 'saving';
			// Set lastSaveTime BEFORE the API call so the SSE guard works
			// even if the SSE event arrives before the response.
			editorStore.setLastSaveTime(Date.now());
			const allItems = collectionStore.items ?? [];
			let toSave = markdown;
			if (allItems.length > 0) {
				toSave = markdownToWikiLinks(toSave, allItems);
			}
			toSave = cleanBrokenLinks(toSave);
			api.items.update(wsSlug, item.id, { content: toSave }).then(() => {
				// Don't overwrite item -- resetting editorContent would
				// clobber anything typed since the debounce started.
				editorStore.setLastSaveTime(Date.now());
				editorStore.setDirty(false);
				showSaved();
			}).catch(() => {
				saveStatus = 'idle';
				toastStore.show('Failed to save content', 'error');
			});
		}, 1200);
	}

	function handleRawContentUpdate(markdown: string) {
		clearTimeout(contentDebounceTimer);
		editorStore.setDirty(true);
		contentDebounceTimer = setTimeout(() => {
			if (!item) return;
			saveStatus = 'saving';
			editorStore.setLastSaveTime(Date.now());
			// Raw mode: content is already in storage format (with [[wiki links]])
			api.items.update(wsSlug, item.id, { content: markdown }).then((updated) => {
				item = updated;
				editorStore.setLastSaveTime(Date.now());
				editorStore.setDirty(false);
				showSaved();
			}).catch(() => {
				saveStatus = 'idle';
				toastStore.show('Failed to save content', 'error');
			});
		}, 1200);
	}

	let computedOverrides = $state<Record<string, any>>({});
	let childTerminalStatuses = $state<string[] | undefined>(undefined);

	function handleChildrenChange(items: Item[]) {
		// Track child IDs for deduplication in the relationships section
		childItemIds = new Set(items.map(i => i.id));
		hasChildren = items.length > 0;

		// Recompute progress from the actual children
		const total = items.length;
		const allCollections = collectionStore.collections ?? [];
		// Gather terminal statuses from all collections the children belong to
		const termSet = new Set<string>();
		for (const child of items) {
			const col = allCollections.find(c => c.slug === child.collection_slug);
			if (col) {
				for (const ts of getTerminalOptions(col)) termSet.add(ts);
			}
		}
		const termOpts = termSet.size > 0 ? [...termSet] : ['done', 'cancelled'];
		childTerminalStatuses = termOpts;
		const done = items.filter((i) => termOpts.includes(parseFields(i).status)).length;
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
		return `/${username}/${wsSlug}/${collectionSlug}/${refOrSlug}`;
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
			status,
			linkId: link.id
		};
	}

	function buildRelationshipGroups(currentItem: Item, links: ItemLink[], excludeChildIds: Set<string> = new Set()): RelationshipGroup[] {
		const grouped = new Map<string, RelationshipGroup>();
		const definitions: Record<string, { label: string; tone: RelationshipGroup['tone'] }> = {
			parent_of: { label: 'Children', tone: 'default' },
			child_of: { label: 'Child of', tone: 'default' },
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
		const order = ['parent_of', 'child_of', 'blocks', 'blocked_by', 'links_to', 'referenced_by', 'split_from', 'split_into', 'supersedes', 'superseded_by', 'implements', 'implemented_by', 'related'];

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
				case 'parent':
				case 'phase': {
					// If this item is the parent and the child is already shown in ChildItems, skip it
					if (!isSource && excludeChildIds.has(link.source_id)) break;
					addEntry(isSource ? 'child_of' : 'parent_of', linkEntry(link, !isSource));
					break;
				}
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
				case 'implements': {
					// If this item is the target (implemented by) and the source is already shown in ChildItems, skip it
					if (!isSource && excludeChildIds.has(link.source_id)) break;
					addEntry(isSource ? 'implements' : 'implemented_by', linkEntry(link, !isSource));
					break;
				}
				default:
					addEntry('related', linkEntry(link, !isSource));
					break;
			}
		}

		// Annotate the matching relationship group with closure summary
		if (currentItem.derived_closure) {
			const closureGroupKey: Record<string, string> = {
				superseded_by: 'superseded_by',
				implemented_by: 'implemented_by',
				split_into: 'split_into'
			};
			const key = closureGroupKey[currentItem.derived_closure.kind];
			if (key && grouped.has(key)) {
				grouped.get(key)!.closureSummary = currentItem.derived_closure.summary;
			}
		}

		return order
			.map((key) => grouped.get(key))
			.filter((group): group is RelationshipGroup => Boolean(group && group.entries.length > 0));
	}

	function handleVersionRestore(updatedItem: Item) {
		item = updatedItem;
	}

	async function handleDelete() {
		if (!item) return;
		deleting = true;
		try {
			await api.items.delete(wsSlug, item.id);
			toastStore.show('Item deleted', 'success');
			goto(`/${username}/${wsSlug}/${collSlug}`);
		} catch {
			toastStore.show('Failed to delete item', 'error');
			deleting = false;
			confirmDelete = false;
		}
	}

	let allCollections = $derived(collectionStore.collections ?? []);
	let moveTargets = $derived(allCollections.filter(c => c.slug !== collSlug));

	async function handleDeleteLink(linkId?: string) {
		if (!linkId || !item) return;
		try {
			await api.links.delete(wsSlug, linkId);
			itemLinks = itemLinks.filter(l => l.id !== linkId);
			// Refresh item to update parent info
			const refreshed = await api.items.get(wsSlug, itemSlug);
			item = { ...refreshed, content: item.content };
			toastStore.show('Relationship removed', 'success');
		} catch (e: any) {
			toastStore.show(e.message ?? 'Failed to remove relationship', 'error');
		}
	}

	// ── Add Relationship ─────────────────────────────────────────────────────
	let showAddLink = $state(false);
	let addLinkType = $state('related');
	let addLinkSearch = $state('');
	let addLinkResults = $state<Item[]>([]);
	let addLinkLoading = $state(false);

	async function searchItemsForLink() {
		if (!addLinkSearch.trim()) {
			addLinkResults = [];
			return;
		}
		addLinkLoading = true;
		try {
			const results = await api.search(addLinkSearch, wsSlug);
			// Filter out self and items already linked
			const linkedIds = new Set(itemLinks.flatMap(l => [l.source_id, l.target_id]));
			addLinkResults = (results.results || [])
				.map((r) => r.item)
				.filter((i: Item) => i.id !== item?.id && !linkedIds.has(i.id))
				.slice(0, 10);
		} catch {
			addLinkResults = [];
		} finally {
			addLinkLoading = false;
		}
	}

	async function handleCreateLink(targetItem: Item) {
		if (!item) return;
		try {
			const newLink = await api.links.create(wsSlug, item.slug, {
				target_id: targetItem.id,
				link_type: addLinkType
			});
			itemLinks = [...itemLinks, newLink];
			showAddLink = false;
			addLinkSearch = '';
			addLinkResults = [];
			// Refresh item to update parent info
			const refreshed = await api.items.get(wsSlug, itemSlug);
			item = { ...refreshed, content: item.content };
			toastStore.show('Relationship added', 'success');
		} catch (e: any) {
			toastStore.show(e.message ?? 'Failed to add relationship', 'error');
		}
	}

	async function handleMove(targetSlug: string) {
		if (!item || moving) return;
		moving = true;
		showMoveMenu = false;
		try {
			const moved = await api.items.move(wsSlug, item.slug, targetSlug);
			toastStore.show(`Moved to ${targetSlug}`, 'success');
			goto(`/${username}/${wsSlug}/${targetSlug}/${moved.slug}`, { replaceState: true });
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
		<div class="sticky-header">
			<nav class="breadcrumb">
				<a href="/{username}/{wsSlug}">Home</a>
				<span class="sep">/</span>
				{#if item.parent_collection_slug && item.parent_slug}
					{@const parentColl = allCollections.find(c => c.slug === item.parent_collection_slug)}
					<a href="/{username}/{wsSlug}/{item.parent_collection_slug}">{parentColl?.icon ?? ''} {parentColl?.name ?? item.parent_collection_slug}</a>
					<span class="sep">/</span>
					<a href="/{username}/{wsSlug}/{item.parent_collection_slug}/{item.parent_slug}">{item.parent_ref || item.parent_title}</a>
					<span class="sep">/</span>
				{:else}
					<a href="/{username}/{wsSlug}/{collSlug}">{collection.icon} {collection.name}</a>
					<span class="sep">/</span>
				{/if}
				<span class="current">{formatItemRef(item) || item.title}</span>
				{#if formatItemRef(item)}
					<button class="copy-ref-btn" onclick={handleCopyRef} title="Copy item ID">
						{#if copied}
							<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"></polyline></svg>
							<span class="copied-tooltip">Copied!</span>
						{:else}
							<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path></svg>
						{/if}
					</button>
				{/if}
			</nav>
		</div>

		<!-- Title -->
		<div class="title-row">
			{#if formatItemRef(item)}
				<span class="item-ref">{formatItemRef(item)}</span>
			{/if}
			{#if editingTitle}
				<textarea
					class="title-input"
					rows="1"
					bind:this={titleInputEl}
					bind:value={titleDraft}
					onblur={saveTitle}
					onkeydown={handleTitleKeydown}
					oninput={(e) => autoResizeTitle(e.currentTarget)}
				></textarea>
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
				onclick={() => { document.getElementById('item-timeline')?.scrollIntoView({ behavior: 'smooth' }); }}
			>
				Timeline
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
			{#if isOwner}
				<button class="action-btn" onclick={() => { shareDialogOpen = true; }}>
					Share
				</button>
			{/if}
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
								/>
							</div>
						</div>
					{/if}
				{/each}

				<!-- Assignment: user + role -->
				{#if workspaceMembers.length > 0 || agentRoles.length > 0}
					<div class="fields-header" style="margin-top: var(--space-4)">Assignment</div>
				{/if}
				{#if workspaceMembers.length > 0}
					<div class="field-row">
						<span class="field-label">Assigned to</span>
						<div class="field-value">
							<select
								class="assignment-select"
								value={item.assigned_user_id ?? ''}
								onchange={(e) => {
									const val = (e.target as HTMLSelectElement).value;
									updateAssignedUser(val || null);
								}}
							>
								<option value="">Unassigned</option>
								{#each workspaceMembers as member (member.user_id)}
									<option value={member.user_id}>{member.user_name}</option>
								{/each}
							</select>
						</div>
					</div>
				{/if}
				{#if agentRoles.length > 0}
					<div class="field-row">
						<span class="field-label">Role</span>
						<div class="field-value">
							<select
								class="assignment-select"
								value={item.agent_role_id ?? ''}
								onchange={(e) => {
									const val = (e.target as HTMLSelectElement).value;
									updateAgentRole(val || null);
								}}
							>
								<option value="">No role</option>
								{#each agentRoles as role (role.id)}
									<option value={role.id}>{role.icon ? role.icon + ' ' : ''}{role.name}</option>
								{/each}
							</select>
						</div>
					</div>
				{/if}
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

		{#if relationshipGroups.length > 0}
			<div class="relationships-section">
				<h3 class="section-title">Relationships</h3>
				<div class="relationship-groups">
					{#each relationshipGroups as group (group.label)}
						<div class="relationship-group">
							<h4 class="relationship-group-title">{group.label}</h4>
							{#if group.closureSummary}
								<p class="closure-inline-summary">✓ {group.closureSummary}</p>
							{/if}
							<div class="links-list">
								{#each group.entries as entry (entry.key)}
									<div class="link-row" class:tone-blocks={group.tone === 'blocks'} class:tone-wiki={group.tone === 'wiki'} class:tone-lineage={group.tone === 'lineage'}>
										{#if entry.href}
											<a href={entry.href} class="link-target">{entry.label}</a>
										{:else}
											<span class="link-target">{entry.label}</span>
										{/if}
										<span class="link-row-actions">
											{#if entry.status}
												<span class="link-status">{formatFieldDisplay(entry.status)}</span>
											{/if}
											{#if entry.linkId}
												<button class="link-delete-btn" title="Remove relationship" onclick={() => handleDeleteLink(entry.linkId)}>×</button>
											{/if}
										</span>
									</div>
								{/each}
							</div>
						</div>
					{/each}
				</div>
			</div>
		{/if}

		<!-- Add Relationship -->
		{#if item}
			<div class="add-relationship-section">
				{#if !showAddLink}
					<button class="add-relationship-btn" onclick={() => { showAddLink = true; }}>
						+ Add relationship
					</button>
				{:else}
					<div class="add-link-form">
						<div class="add-link-header">
							<h4>Add Relationship</h4>
							<button class="add-link-close" onclick={() => { showAddLink = false; addLinkSearch = ''; addLinkResults = []; }}>×</button>
						</div>
						<div class="add-link-controls">
							<select bind:value={addLinkType} class="add-link-type-select">
								<option value="related">Related</option>
								<option value="blocks">Blocks</option>
								<option value="implements">Implements</option>
								<option value="split_from">Split from</option>
								<option value="supersedes">Supersedes</option>
								<option value="parent">Parent</option>
							</select>
							<input
								type="text"
								class="add-link-search"
								placeholder="Search items..."
								bind:value={addLinkSearch}
								oninput={() => searchItemsForLink()}
							/>
						</div>
						{#if addLinkLoading}
							<div class="add-link-loading">Searching...</div>
						{:else if addLinkResults.length > 0}
							<div class="add-link-results">
								{#each addLinkResults as result (result.id)}
									<button class="add-link-result" onclick={() => handleCreateLink(result)}>
										{#if formatItemRef(result)}
											<span class="add-link-ref">{formatItemRef(result)}</span>
										{/if}
										<span class="add-link-title">{result.title}</span>
									</button>
								{/each}
							</div>
						{:else if addLinkSearch.trim().length > 0}
							<div class="add-link-loading">No results</div>
						{/if}
					</div>
				{/if}
			</div>
		{/if}

		<!-- Child Items: always mounted so SSE subscriptions stay active even when starting with 0 children -->
		{#if item}
			<ChildItems {wsSlug} {username} {itemSlug} itemId={item.id} parentFields={fields} terminalStatuses={childTerminalStatuses} onChildrenChange={handleChildrenChange} />
		{/if}

		<!-- Unified Timeline (comments + activity + versions) -->
		<div id="item-timeline" class="timeline-section">
			<ItemTimeline
				{wsSlug}
				{itemSlug}
				currentContent={item.content ?? ''}
				items={collectionStore.items ?? []}
				onRestore={handleVersionRestore}
			/>
		</div>

	</div>

	{#if isOwner && item}
		<ShareDialog
			{wsSlug}
			type="item"
			targetSlug={item.slug}
			targetName={formatItemRef(item) || item.title}
			bind:open={shareDialogOpen}
		/>
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

	.sticky-header {
		position: sticky;
		top: 0;
		z-index: 4;
		background: var(--bg-primary);
		margin: 0 calc(-1 * var(--space-6));
		padding: var(--space-2) var(--space-6);
		border-bottom: 1px solid transparent;
		transition: border-color 0.15s ease;
	}

	/* Breadcrumb */
	.breadcrumb {
		display: flex;
		align-items: center;
		gap: var(--space-1);
		font-size: 0.85em;
		color: var(--text-muted);
		margin-bottom: 0;
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
	.copy-ref-btn {
		position: relative;
		display: inline-flex;
		align-items: center;
		justify-content: center;
		width: 24px;
		height: 24px;
		padding: 0;
		margin-left: 2px;
		background: none;
		border: none;
		border-radius: var(--radius-sm);
		color: var(--text-muted);
		cursor: pointer;
		transition: color 0.15s ease, background 0.15s ease;
	}
	.copy-ref-btn:hover {
		color: var(--text-primary);
		background: var(--bg-tertiary);
	}
	.copied-tooltip {
		position: absolute;
		top: 100%;
		left: 50%;
		transform: translateX(-50%);
		margin-top: 4px;
		padding: 2px 8px;
		font-size: 0.75em;
		font-family: var(--font-sans);
		color: var(--text-on-accent);
		background: var(--accent-green, #22c55e);
		border-radius: var(--radius-sm);
		white-space: nowrap;
		pointer-events: none;
		z-index: 20;
	}

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
		resize: none;
		overflow: hidden;
		line-height: 1.3;
		font-family: inherit;
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
	.assignment-select {
		width: 100%;
		padding: 5px 8px;
		font-size: 0.88em;
		font-family: inherit;
		color: var(--text-primary);
		background: var(--bg-tertiary);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		cursor: pointer;
		appearance: auto;
	}
	.assignment-select:hover {
		border-color: var(--accent-blue);
	}
	.assignment-select:focus {
		outline: 2px solid var(--accent-blue);
		outline-offset: -1px;
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

	.closure-inline-summary {
		margin: 0 0 var(--space-2) 0;
		font-size: 0.8em;
		color: var(--accent-green);
		font-weight: 500;
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
	.link-row-actions {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}
	.link-delete-btn {
		display: none;
		background: none;
		border: none;
		color: var(--text-muted);
		cursor: pointer;
		padding: 0 var(--space-1);
		font-size: 1rem;
		line-height: 1;
	}
	.link-delete-btn:hover {
		color: var(--danger);
	}
	.link-row:hover .link-delete-btn {
		display: inline;
	}

	/* Add Relationship */
	.add-relationship-section {
		margin-top: var(--space-4);
	}
	.add-relationship-btn {
		background: none;
		border: 1px dashed var(--border-color);
		color: var(--text-muted);
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius-md);
		cursor: pointer;
		font-size: 0.85rem;
		width: 100%;
		text-align: left;
	}
	.add-relationship-btn:hover {
		color: var(--text-primary);
		border-color: var(--text-muted);
	}
	.add-link-form {
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		padding: var(--space-3);
		background: var(--bg-secondary);
	}
	.add-link-header {
		display: flex;
		justify-content: space-between;
		align-items: center;
		margin-bottom: var(--space-2);
	}
	.add-link-header h4 {
		margin: 0;
		font-size: 0.85rem;
		font-weight: 600;
	}
	.add-link-close {
		background: none;
		border: none;
		color: var(--text-muted);
		cursor: pointer;
		font-size: 1.2rem;
		padding: 0;
		line-height: 1;
	}
	.add-link-controls {
		display: flex;
		gap: var(--space-2);
		margin-bottom: var(--space-2);
	}
	.add-link-type-select {
		padding: var(--space-1) var(--space-2);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background: var(--bg-primary);
		color: var(--text-primary);
		font-size: 0.8rem;
		min-width: 120px;
	}
	.add-link-search {
		flex: 1;
		padding: var(--space-1) var(--space-2);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background: var(--bg-primary);
		color: var(--text-primary);
		font-size: 0.8rem;
	}
	.add-link-results {
		display: flex;
		flex-direction: column;
		gap: 1px;
		max-height: 200px;
		overflow-y: auto;
	}
	.add-link-result {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding: var(--space-2);
		background: var(--bg-primary);
		border: none;
		border-radius: var(--radius-sm);
		cursor: pointer;
		text-align: left;
		color: var(--text-primary);
		font-size: 0.8rem;
	}
	.add-link-result:hover {
		background: var(--bg-hover);
	}
	.add-link-ref {
		color: var(--text-muted);
		font-family: var(--font-mono);
		font-size: 0.75rem;
		flex-shrink: 0;
	}
	.add-link-title {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}
	.add-link-loading {
		padding: var(--space-2);
		color: var(--text-muted);
		font-size: 0.8rem;
	}

	/* Timeline */
	.timeline-section {
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
	@media (max-width: 768px) {
		.layout-balanced .fields-panel {
			grid-template-columns: 1fr;
		}
	}
</style>
