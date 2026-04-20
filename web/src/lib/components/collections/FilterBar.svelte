<script lang="ts">
	import type { Collection } from '$lib/types';
	import { parseSchema } from '$lib/types';
	import BottomSheet from '$lib/components/common/BottomSheet.svelte';

	interface Props {
		collection: Collection;
		activeFilters: Record<string, string>;
		searchQuery: string;
		onFilterChange: (filters: Record<string, string>) => void;
		onSearchChange: (query: string) => void;
		relationLabels?: Record<string, string>;
		searchInputEl?: HTMLInputElement | undefined;
	}

	let { collection, activeFilters, searchQuery, onFilterChange, onSearchChange, relationLabels = {}, searchInputEl = $bindable() }: Props = $props();

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
	let isMobile = $state(false);
	$effect(() => {
		if (typeof window === 'undefined') return;
		const mq = window.matchMedia('(max-width: 639.98px)');
		isMobile = mq.matches;
		const onChange = (e: MediaQueryListEvent) => {
			isMobile = e.matches;
			// If the viewport crosses above the mobile breakpoint while the
			// sheet is open (e.g. device rotation), close it so it doesn't
			// spring back open as soon as the viewport returns to mobile.
			if (!e.matches) {
				parentSheetOpen = false;
			}
		};
		mq.addEventListener('change', onChange);
		return () => mq.removeEventListener('change', onChange);
	});

	let parentSheetOpen = $state(false);

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
		{#if isMobile}
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

	<div class="search-wrapper">
		<input
			bind:this={searchInputEl}
			type="text"
			class="search-input"
			placeholder="Search {collection.name.toLowerCase()}..."
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
