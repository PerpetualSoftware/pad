<script lang="ts">
	import type { Collection } from '$lib/types';
	import { parseSchema } from '$lib/types';
	import BottomSheet from '$lib/components/common/BottomSheet.svelte';
	import { viewport } from '$lib/stores/breakpoint.svelte';
	import TagFilter from '$lib/components/collections/TagFilter.svelte';
	import ItemPicker from '$lib/components/items/ItemPicker.svelte';
	import { localIndex } from '$lib/stores/localIndex.svelte';
	import { collectionStore } from '$lib/stores/collections.svelte';
	import { narrowRelationRow, relationChipFor } from '$lib/collections/relationGroups';

	interface Props {
		collection: Collection;
		activeFilters: Record<string, string>;
		searchQuery: string;
		onFilterChange: (filters: Record<string, string>) => void;
		onSearchChange: (query: string) => void;
		relationLabels?: Record<string, string>;
		/** Tags present in this collection, with counts, ordered by count desc. */
		tagCounts?: { tag: string; count: number }[];
		/** Currently-selected tag filters (OR semantics). */
		selectedTags?: string[];
		onTagFilterChange?: (tags: string[]) => void;
		searchInputEl?: HTMLInputElement | undefined;
		/**
		 * Whether the "Unparented only" chip should render at all (TASK-2099 /
		 * PLAN-2095 DR-2). Driven by the caller's projection-scope state
		 * (`localIndex.includesUnparentedMetadataFor`) — `false` for
		 * restricted callers, who must never see this filter exists.
		 */
		unparentedAvailable?: boolean;
		/** Current effective state of the unparented chip. */
		unparentedActive?: boolean;
		onUnparentedChange?: (value: boolean) => void;
		/**
		 * Workspace slug, for the relation filters (TASK-2998 / PLAN-2857 U7).
		 * Omitted = no relation filter renders, which is what a caller with no
		 * workspace context (the public share view) wants: it cannot resolve a
		 * target's title, and a filter chip showing a bare id is worse than no
		 * chip.
		 */
		wsSlug?: string;
	}

	let {
		collection,
		activeFilters,
		searchQuery,
		onFilterChange,
		onSearchChange,
		relationLabels = {},
		tagCounts = [],
		selectedTags = [],
		onTagFilterChange = () => {},
		searchInputEl = $bindable(),
		unparentedAvailable = false,
		unparentedActive = false,
		onUnparentedChange = () => {},
		wsSlug = '',
	}: Props = $props();

	let schema = $derived(parseSchema(collection));
	let statusField = $derived(schema.fields.find((f) => f.key === 'status'));
	let statusOptions = $derived(statusField?.options ?? []);

	let activeStatus = $derived(activeFilters.status ?? 'all');
	let showAll = $derived(activeStatus === 'all');

	function setStatusFilter(value: string) {
		const next = { ...activeFilters };
		if (value === 'all') {
			delete next.status;
		} else {
			next.status = value;
		}
		onFilterChange(next);
	}

	// FILTERING BY A RELATION VALUE (TASK-2998 / PLAN-2857 U7).
	//
	// The page's `filteredItems` already compares `fields[key] === value` for
	// any key, so the mechanism has always been there — what was missing is a
	// way to SET one, and a chip that says what the stored id means. Both reuse
	// U3's picker and U2's chip vocabulary rather than inventing a second way
	// to choose and name an item.
	//
	// The hardcoded tasks→plans parent filter above is deliberately left alone:
	// it filters on `parent_link_id`, not on a field, so it is a different
	// mechanism wearing a similar hat. Generalising THAT is not this unit.
	let knownCollectionSlugs = $derived(new Set(collectionStore.collections.map((c) => c.slug)));
	let relationFields = $derived(
		wsSlug ? schema.fields.filter((f) => f.type === 'relation' && !!f.collection) : [],
	);
	let openPickerFor = $state<string | null>(null);

	function resolveRelation(id: string, declared: string | undefined) {
		if (!wsSlug) return null;
		return narrowRelationRow(
			localIndex.findByIdOrSlug(wsSlug, id),
			id,
			declared,
			knownCollectionSlugs,
		);
	}

	function setRelationFilter(key: string, value: string) {
		const next = { ...activeFilters };
		if (value) {
			next[key] = value;
		} else {
			delete next[key];
		}
		openPickerFor = null;
		onFilterChange(next);
	}

	let hasParentFilter = $derived(Object.keys(relationLabels).length > 0);
	let activeParent = $derived(activeFilters.parent ?? '');
	let activeParentLabel = $derived(activeParent ? (relationLabels[activeParent] ?? activeParent) : 'All plans');

	function setParentFilterValue(value: string) {
		const next = { ...activeFilters };
		if (value === '') {
			delete next.parent;
		} else {
			next.parent = value;
		}
		onFilterChange(next);
	}

	function setParentFilter(e: Event) {
		const value = (e.target as HTMLSelectElement).value;
		setParentFilterValue(value);
	}

	function handleSearchInput(e: Event) {
		const target = e.target as HTMLInputElement;
		onSearchChange(target.value);
	}

	function formatLabel(value: string): string {
		return value.replace(/_/g, ' ').replace(/\b\w/g, (c) => c.toUpperCase());
	}

	// ── Viewport detection ───────────────────────────────────────────────
	// On mobile the parent filter is rendered as a chip that opens a
	// BottomSheet of options; on desktop the native <select> is kept
	// because it's compact inside the toolbar and familiar.
	let parentSheetOpen = $state(false);

	// If the viewport crosses above the mobile breakpoint while the sheet is
	// open (e.g. device rotation), close it so it doesn't spring back open as
	// soon as the viewport returns to mobile. Reads the shared breakpoint flag
	// (TASK-2028); writes only `parentSheetOpen`, so no self-invalidation.
	$effect(() => {
		if (!viewport.isMobile) parentSheetOpen = false;
	});

	function openParentSheet() {
		parentSheetOpen = true;
	}

	function handleParentSheetSelect(value: string) {
		setParentFilterValue(value);
		parentSheetOpen = false;
	}
</script>

<div class="filter-bar">
	{#if statusOptions.length > 0}
		<div class="status-filters">
			<button
				class="filter-btn"
				class:active={showAll}
				onclick={() => setStatusFilter('all')}
			>All</button>
			{#each statusOptions as option (option)}
				<button
					class="filter-btn"
					class:active={activeStatus === option}
					onclick={() => setStatusFilter(option)}
				>{formatLabel(option)}</button>
			{/each}
		</div>
	{/if}

	{#if hasParentFilter}
		{#if viewport.isMobile}
			<!--
				Mobile: render the parent filter as a chip + BottomSheet to keep
				option labels readable full-width and avoid the native <select>'s
				inconsistent mobile styling.
			-->
			<button class="parent-chip" type="button" onclick={openParentSheet}>
				<span class="parent-chip-label">{activeParentLabel}</span>
				<span class="parent-chip-caret" aria-hidden="true">▾</span>
			</button>
			{#if parentSheetOpen}
				<!--
					Gate the sheet on `parentSheetOpen` so BottomSheet's global
					keydown listener isn't mounted when the filter is idle.
					Same gate-on-open pattern as ReactionPicker/Move menu.
				-->
				<BottomSheet
					open={parentSheetOpen}
					onclose={() => (parentSheetOpen = false)}
					title="Filter by plan"
				>
					<div class="parent-sheet-body">
						<button
							class="parent-sheet-option"
							class:active={activeParent === ''}
							type="button"
							onclick={() => handleParentSheetSelect('')}
						>All plans</button>
						{#each Object.entries(relationLabels) as [id, label] (id)}
							<button
								class="parent-sheet-option"
								class:active={activeParent === id}
								type="button"
								onclick={() => handleParentSheetSelect(id)}
							>{label}</button>
						{/each}
					</div>
				</BottomSheet>
			{/if}
		{:else}
			<select class="parent-filter" value={activeParent} onchange={setParentFilter}>
				<option value="">All plans</option>
				{#each Object.entries(relationLabels) as [id, label] (id)}
					<option value={id}>{label}</option>
				{/each}
			</select>
		{/if}
	{/if}

	{#each relationFields as rf (rf.key)}
		{@const active = activeFilters[rf.key] ?? ''}
		{@const chip = active ? relationChipFor(active, (id) => resolveRelation(id, rf.collection)) : null}
		<div class="relation-filter">
			<button
				type="button"
				class="relation-filter-trigger"
				class:active={!!active}
				aria-haspopup="dialog"
				aria-expanded={openPickerFor === rf.key}
				onclick={() => (openPickerFor = openPickerFor === rf.key ? null : rf.key)}
			>
				{#if chip}
					{#if chip.ref}<span class="relation-filter-ref">{chip.ref}</span>{/if}
					<span>{chip.title ?? chip.label}</span>
					{#if chip.state === 'deleted'}<span class="relation-filter-note">(deleted)</span>{/if}
				{:else}
					All {(rf.label ?? rf.key).toLowerCase()}
				{/if}
			</button>
			{#if active}
				<button
					type="button"
					class="relation-filter-clear"
					aria-label="Clear {rf.label ?? rf.key} filter"
					onclick={() => setRelationFilter(rf.key, '')}
				>×</button>
			{/if}
			{#if openPickerFor === rf.key}
				<div class="relation-filter-picker" role="dialog" aria-label="Filter by {rf.label ?? rf.key}">
					<ItemPicker
						{wsSlug}
						collection={rf.collection}
						source="index"
						autofocus
						label="Filter by {rf.label ?? rf.key}"
						placeholder="Search {rf.collection}…"
						onselect={(row) => setRelationFilter(rf.key, row.id)}
					/>
				</div>
			{/if}
		</div>
	{/each}

	{#if unparentedAvailable}
		<!--
			"Unparented only" chip (TASK-2099 / PLAN-2095). Rendered only when
			the caller's projection scope confirms `is_unparented` metadata is
			present — restricted callers never see this exists (DR-2). Mutually
			exclusive with the parent filter; the page component owns that
			mutex, not this component (it doesn't know about `unparented` as a
			field in `activeFilters` — the flag lives outside it).
		-->
		<button
			type="button"
			class="unparented-chip"
			class:active={unparentedActive}
			aria-pressed={unparentedActive}
			onclick={() => onUnparentedChange(!unparentedActive)}
		>Unparented only</button>
	{/if}

	{#if tagCounts.length > 0}
		<TagFilter tags={tagCounts} selected={selectedTags} onchange={onTagFilterChange} />
	{/if}

	<div class="search-wrapper">
		<!--
			Search hits the local in-memory index (titles + parsed fields)
			for sub-millisecond typing. Prefix vocabulary (TASK-1367):
			  - `body:foo` / `content:foo` — server FTS over the rich-text
			    body. The only way to grep content, which doesn't live
			    in the client-side index by design.
			  - `#5` / `item:5` — exact item-number lookup.
			  - `TASK-5` — exact-ref hoist to the top of results.
			(Use the Show archived toggle for archived rows — a
			combined `is:archived body:` prefix is intentionally
			deferred until /search supports it.)
		-->
		<input
			bind:this={searchInputEl}
			type="text"
			class="search-input"
			placeholder="Search {collection.name.toLowerCase()}..."
			title="Search titles + fields locally.&#10;  body:foo  search rich-text body (server)&#10;  #5  exact item number&#10;  TASK-5  exact ref"
			value={searchQuery}
			oninput={handleSearchInput}
			onkeydown={(e) => { if (e.key === 'Escape') { onSearchChange(''); searchInputEl?.blur(); } }}
		/>
	</div>
</div>

<style>
	.filter-bar {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		flex-wrap: wrap;
		flex: 1;
	}

	.status-filters {
		display: flex;
		border: 1px solid var(--border);
		border-radius: var(--radius);
		overflow: hidden;
	}

	.filter-btn {
		background: var(--bg-secondary);
		border: none;
		padding: var(--space-1) var(--space-3);
		cursor: pointer;
		font-size: 0.82em;
		color: var(--text-secondary);
		white-space: nowrap;
	}

	.filter-btn:not(:last-child) {
		border-right: 1px solid var(--border);
	}

	.filter-btn.active {
		background: var(--bg-tertiary);
		color: var(--text-primary);
		font-weight: 600;
	}

	.filter-btn:hover:not(.active) {
		background: var(--bg-hover);
	}

	.parent-filter {
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-1) var(--space-3);
		font-size: 0.82em;
		color: var(--text-primary);
		cursor: pointer;
		max-width: 180px;
	}
	.parent-filter:focus {
		border-color: var(--accent-blue);
		outline: none;
	}

	/* Mobile: chip trigger for the parent filter. Styled to match the
	   segmented filter buttons so it reads as part of the same toolbar. */
	.parent-chip {
		display: inline-flex;
		align-items: center;
		gap: var(--space-1);
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-1) var(--space-3);
		font-size: 0.82em;
		color: var(--text-primary);
		cursor: pointer;
		max-width: 220px;
	}

	.parent-chip-label {
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.parent-chip-caret {
		color: var(--text-muted);
		font-size: 0.9em;
		line-height: 1;
	}

	.parent-chip:hover {
		border-color: var(--accent-blue);
	}

	.parent-sheet-body {
		display: flex;
		flex-direction: column;
		padding: 0 var(--space-2) var(--space-3);
	}

	.parent-sheet-option {
		display: block;
		width: 100%;
		text-align: left;
		background: none;
		border: none;
		padding: var(--space-3);
		color: var(--text-primary);
		font-size: 1em;
		cursor: pointer;
		border-radius: var(--radius-sm);
	}

	.parent-sheet-option:hover {
		background: var(--bg-hover);
	}

	.parent-sheet-option.active {
		background: var(--bg-tertiary);
		font-weight: 600;
	}

	.unparented-chip {
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-1) var(--space-3);
		font-size: 0.82em;
		color: var(--text-primary);
		cursor: pointer;
		white-space: nowrap;
	}

	.unparented-chip:hover:not(.active) {
		border-color: var(--accent-blue);
	}

	.unparented-chip.active {
		background: var(--bg-tertiary);
		border-color: var(--accent-blue);
		font-weight: 600;
	}

	.relation-filter {
		position: relative;
		display: inline-flex;
		align-items: center;
		gap: 0.25rem;
	}

	.relation-filter-trigger {
		display: inline-flex;
		align-items: center;
		gap: 0.35rem;
		padding: 0.35rem 0.6rem;
		border: 1px solid var(--border);
		border-radius: 999px;
		background: var(--bg-elevated, var(--bg));
		color: var(--text);
		font-size: 0.85rem;
		cursor: pointer;
	}

	.relation-filter-trigger.active {
		border-color: var(--accent, var(--border));
	}

	.relation-filter-ref {
		font-family: var(--font-mono, ui-monospace, monospace);
		font-size: 0.85em;
		opacity: 0.7;
	}

	.relation-filter-note {
		font-size: 0.85em;
		opacity: 0.7;
		font-style: italic;
	}

	.relation-filter-clear {
		border: none;
		background: none;
		color: var(--text-muted, var(--text));
		cursor: pointer;
		font-size: 1rem;
		line-height: 1;
		padding: 0 0.2rem;
	}

	.relation-filter-picker {
		position: absolute;
		top: calc(100% + 0.35rem);
		left: 0;
		z-index: 30;
		min-width: min(22rem, 90vw);
		max-width: 90vw;
		padding: 0.5rem;
		border: 1px solid var(--border);
		border-radius: 0.5rem;
		background: var(--bg-elevated, var(--bg));
		box-shadow: 0 8px 24px rgb(0 0 0 / 0.18);
	}

	.search-wrapper {
		flex: 1;
		min-width: 140px;
		max-width: 260px;
	}

	.search-input {
		width: 100%;
		background: var(--bg-secondary);
		border: 1px solid var(--border);
		border-radius: var(--radius);
		padding: var(--space-1) var(--space-3);
		font-size: 0.82em;
		color: var(--text-primary);
	}

	.search-input::placeholder {
		color: var(--text-muted);
	}

	.search-input:focus {
		border-color: var(--accent-blue);
		outline: none;
	}
</style>
